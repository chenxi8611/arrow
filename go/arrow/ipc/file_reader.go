// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ipc

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/apache/arrow/go/v8/arrow"
	"github.com/apache/arrow/go/v8/arrow/array"
	"github.com/apache/arrow/go/v8/arrow/bitutil"
	"github.com/apache/arrow/go/v8/arrow/internal/flatbuf"
	"github.com/apache/arrow/go/v8/arrow/memory"
	"golang.org/x/xerrors"
)

// FileReader is an Arrow file reader.
type FileReader struct {
	r ReadAtSeeker

	footer struct {
		offset int64
		buffer *memory.Buffer
		data   *flatbuf.Footer
	}

	fields dictTypeMap
	memo   dictMemo

	schema *arrow.Schema
	record arrow.Record

	irec int   // current record index. used for the arrio.Reader interface
	err  error // last error

	mem memory.Allocator
}

// NewFileReader opens an Arrow file using the provided reader r.
func NewFileReader(r ReadAtSeeker, opts ...Option) (*FileReader, error) {
	var (
		cfg = newConfig(opts...)
		err error

		f = FileReader{
			r:      r,
			fields: make(dictTypeMap),
			memo:   newMemo(),
			mem:    cfg.alloc,
		}
	)

	if cfg.footer.offset <= 0 {
		cfg.footer.offset, err = f.r.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, xerrors.Errorf("arrow/ipc: could retrieve footer offset: %w", err)
		}
	}
	f.footer.offset = cfg.footer.offset

	err = f.readFooter()
	if err != nil {
		return nil, xerrors.Errorf("arrow/ipc: could not decode footer: %w", err)
	}

	err = f.readSchema()
	if err != nil {
		return nil, xerrors.Errorf("arrow/ipc: could not decode schema: %w", err)
	}

	if cfg.schema != nil && !cfg.schema.Equal(f.schema) {
		return nil, xerrors.Errorf("arrow/ipc: inconsistent schema for reading (got: %v, want: %v)", f.schema, cfg.schema)
	}

	return &f, err
}

func (f *FileReader) readFooter() error {
	var err error

	if f.footer.offset <= int64(len(Magic)*2+4) {
		return xerrors.Errorf("arrow/ipc: file too small (size=%d)", f.footer.offset)
	}

	eof := int64(len(Magic) + 4)
	buf := make([]byte, eof)
	n, err := f.r.ReadAt(buf, f.footer.offset-eof)
	if err != nil {
		return xerrors.Errorf("arrow/ipc: could not read footer: %w", err)
	}
	if n != len(buf) {
		return xerrors.Errorf("arrow/ipc: could not read %d bytes from end of file", len(buf))
	}

	if !bytes.Equal(buf[4:], Magic) {
		return errNotArrowFile
	}

	size := int64(binary.LittleEndian.Uint32(buf[:4]))
	if size <= 0 || size+int64(len(Magic)*2+4) > f.footer.offset {
		return errInconsistentFileMetadata
	}

	buf = make([]byte, size)
	n, err = f.r.ReadAt(buf, f.footer.offset-size-eof)
	if err != nil {
		return xerrors.Errorf("arrow/ipc: could not read footer data: %w", err)
	}
	if n != len(buf) {
		return xerrors.Errorf("arrow/ipc: could not read %d bytes from footer data", len(buf))
	}

	f.footer.buffer = memory.NewBufferBytes(buf)
	f.footer.data = flatbuf.GetRootAsFooter(buf, 0)
	return err
}

func (f *FileReader) readSchema() error {
	var err error
	f.fields, err = dictTypesFromFB(f.footer.data.Schema(nil))
	if err != nil {
		return xerrors.Errorf("arrow/ipc: could not load dictionary types from file: %w", err)
	}

	//lint:ignore SA4008 readDictionary always panics currently. ignore lint until DictionaryArray is implemented.
	for i := 0; i < f.NumDictionaries(); i++ {
		blk, err := f.dict(i)
		if err != nil {
			return xerrors.Errorf("arrow/ipc: could read dictionary[%d]: %w", i, err)
		}
		switch {
		case !bitutil.IsMultipleOf8(blk.Offset):
			return xerrors.Errorf("arrow/ipc: invalid file offset=%d for dictionary %d", blk.Offset, i)
		case !bitutil.IsMultipleOf8(int64(blk.Meta)):
			return xerrors.Errorf("arrow/ipc: invalid file metadata=%d position for dictionary %d", blk.Meta, i)
		case !bitutil.IsMultipleOf8(blk.Body):
			return xerrors.Errorf("arrow/ipc: invalid file body=%d position for dictionary %d", blk.Body, i)
		}

		msg, err := blk.NewMessage()
		if err != nil {
			return err
		}

		id, dict, err := readDictionary(msg.meta, f.fields, f.r)
		msg.Release()
		if err != nil {
			return xerrors.Errorf("arrow/ipc: could not read dictionary %d from file: %w", i, err)
		}
		f.memo.Add(id, dict)
		dict.Release() // memo.Add increases ref-count of dict.
	}

	schema := f.footer.data.Schema(nil)
	if schema == nil {
		return xerrors.Errorf("arrow/ipc: could not load schema from flatbuffer data")
	}
	f.schema, err = schemaFromFB(schema, &f.memo)
	if err != nil {
		return xerrors.Errorf("arrow/ipc: could not read schema: %w", err)
	}

	return err
}

func (f *FileReader) block(i int) (fileBlock, error) {
	var blk flatbuf.Block
	if !f.footer.data.RecordBatches(&blk, i) {
		return fileBlock{}, xerrors.Errorf("arrow/ipc: could not extract file block %d", i)
	}

	return fileBlock{
		Offset: blk.Offset(),
		Meta:   blk.MetaDataLength(),
		Body:   blk.BodyLength(),
		r:      f.r,
	}, nil
}

func (f *FileReader) dict(i int) (fileBlock, error) {
	var blk flatbuf.Block
	if !f.footer.data.Dictionaries(&blk, i) {
		return fileBlock{}, xerrors.Errorf("arrow/ipc: could not extract dictionary block %d", i)
	}

	return fileBlock{
		Offset: blk.Offset(),
		Meta:   blk.MetaDataLength(),
		Body:   blk.BodyLength(),
		r:      f.r,
	}, nil
}

func (f *FileReader) Schema() *arrow.Schema {
	return f.schema
}

func (f *FileReader) NumDictionaries() int {
	if f.footer.data == nil {
		return 0
	}
	return f.footer.data.DictionariesLength()
}

func (f *FileReader) NumRecords() int {
	return f.footer.data.RecordBatchesLength()
}

func (f *FileReader) Version() MetadataVersion {
	return MetadataVersion(f.footer.data.Version())
}

// Close cleans up resources used by the File.
// Close does not close the underlying reader.
func (f *FileReader) Close() error {
	if f.footer.data != nil {
		f.footer.data = nil
	}

	if f.footer.buffer != nil {
		f.footer.buffer.Release()
		f.footer.buffer = nil
	}

	if f.record != nil {
		f.record.Release()
		f.record = nil
	}
	return nil
}

// Record returns the i-th record from the file.
// The returned value is valid until the next call to Record.
// Users need to call Retain on that Record to keep it valid for longer.
func (f *FileReader) Record(i int) (arrow.Record, error) {
	record, err := f.RecordAt(i)
	if err != nil {
		return nil, err
	}

	if f.record != nil {
		f.record.Release()
	}

	f.record = record
	return record, nil
}

// Record returns the i-th record from the file. Ownership is transferred to the
// caller and must call Release() to free the memory. This method is safe to
// call concurrently.
func (f *FileReader) RecordAt(i int) (arrow.Record, error) {
	if i < 0 || i > f.NumRecords() {
		panic("arrow/ipc: record index out of bounds")
	}

	blk, err := f.block(i)
	if err != nil {
		return nil, err
	}
	switch {
	case !bitutil.IsMultipleOf8(blk.Offset):
		return nil, xerrors.Errorf("arrow/ipc: invalid file offset=%d for record %d", blk.Offset, i)
	case !bitutil.IsMultipleOf8(int64(blk.Meta)):
		return nil, xerrors.Errorf("arrow/ipc: invalid file metadata=%d position for record %d", blk.Meta, i)
	case !bitutil.IsMultipleOf8(blk.Body):
		return nil, xerrors.Errorf("arrow/ipc: invalid file body=%d position for record %d", blk.Body, i)
	}

	msg, err := blk.NewMessage()
	if err != nil {
		return nil, err
	}
	defer msg.Release()

	if msg.Type() != MessageRecordBatch {
		return nil, xerrors.Errorf("arrow/ipc: message %d is not a Record", i)
	}

	return newRecord(f.schema, msg.meta, bytes.NewReader(msg.body.Bytes()), f.mem), nil
}

// Read reads the current record from the underlying stream and an error, if any.
// When the Reader reaches the end of the underlying stream, it returns (nil, io.EOF).
//
// The returned record value is valid until the next call to Read.
// Users need to call Retain on that Record to keep it valid for longer.
func (f *FileReader) Read() (rec arrow.Record, err error) {
	if f.irec == f.NumRecords() {
		return nil, io.EOF
	}
	rec, f.err = f.Record(f.irec)
	f.irec++
	return rec, f.err
}

// ReadAt reads the i-th record from the underlying stream and an error, if any.
func (f *FileReader) ReadAt(i int64) (arrow.Record, error) {
	return f.Record(int(i))
}

func newRecord(schema *arrow.Schema, meta *memory.Buffer, body ReadAtSeeker, mem memory.Allocator) arrow.Record {
	var (
		msg   = flatbuf.GetRootAsMessage(meta.Bytes(), 0)
		md    flatbuf.RecordBatch
		codec decompressor
	)
	initFB(&md, msg.Header)
	rows := md.Length()

	bodyCompress := md.Compression(nil)
	if bodyCompress != nil {
		codec = getDecompressor(bodyCompress.Codec())
		defer codec.Close()
	}

	ctx := &arrayLoaderContext{
		src: ipcSource{
			meta:  &md,
			r:     body,
			codec: codec,
			mem:   mem,
		},
		max: kMaxNestingDepth,
	}

	cols := make([]arrow.Array, len(schema.Fields()))
	for i, field := range schema.Fields() {
		cols[i] = ctx.loadArray(field.Type)
		defer cols[i].Release()
	}

	return array.NewRecord(schema, cols, rows)
}

type ipcSource struct {
	meta  *flatbuf.RecordBatch
	r     ReadAtSeeker
	codec decompressor
	mem   memory.Allocator
}

func (src *ipcSource) buffer(i int) *memory.Buffer {
	var buf flatbuf.Buffer
	if !src.meta.Buffers(&buf, i) {
		panic("buffer index out of bound")
	}
	if buf.Length() == 0 {
		return memory.NewBufferBytes(nil)
	}

	raw := memory.NewResizableBuffer(src.mem)
	if src.codec == nil {
		raw.Resize(int(buf.Length()))
		_, err := src.r.ReadAt(raw.Bytes(), buf.Offset())
		if err != nil {
			panic(err)
		}
	} else {
		sr := io.NewSectionReader(src.r, buf.Offset(), buf.Length())
		var uncompressedSize uint64

		err := binary.Read(sr, binary.LittleEndian, &uncompressedSize)
		if err != nil {
			panic(err)
		}

		var r io.Reader = sr
		// check for an uncompressed buffer
		if int64(uncompressedSize) != -1 {
			raw.Resize(int(uncompressedSize))
			src.codec.Reset(sr)
			r = src.codec
		} else {
			raw.Resize(int(buf.Length()))
		}

		if _, err = io.ReadFull(r, raw.Bytes()); err != nil {
			panic(err)
		}
	}

	return raw
}

func (src *ipcSource) fieldMetadata(i int) *flatbuf.FieldNode {
	var node flatbuf.FieldNode
	if !src.meta.Nodes(&node, i) {
		panic("field metadata out of bound")
	}
	return &node
}

type arrayLoaderContext struct {
	src     ipcSource
	ifield  int
	ibuffer int
	max     int
}

func (ctx *arrayLoaderContext) field() *flatbuf.FieldNode {
	field := ctx.src.fieldMetadata(ctx.ifield)
	ctx.ifield++
	return field
}

func (ctx *arrayLoaderContext) buffer() *memory.Buffer {
	buf := ctx.src.buffer(ctx.ibuffer)
	ctx.ibuffer++
	return buf
}

func (ctx *arrayLoaderContext) loadArray(dt arrow.DataType) arrow.Array {
	switch dt := dt.(type) {
	case *arrow.NullType:
		return ctx.loadNull()

	case *arrow.BooleanType,
		*arrow.Int8Type, *arrow.Int16Type, *arrow.Int32Type, *arrow.Int64Type,
		*arrow.Uint8Type, *arrow.Uint16Type, *arrow.Uint32Type, *arrow.Uint64Type,
		*arrow.Float16Type, *arrow.Float32Type, *arrow.Float64Type,
		*arrow.Decimal128Type,
		*arrow.Time32Type, *arrow.Time64Type,
		*arrow.TimestampType,
		*arrow.Date32Type, *arrow.Date64Type,
		*arrow.MonthIntervalType, *arrow.DayTimeIntervalType, *arrow.MonthDayNanoIntervalType,
		*arrow.DurationType:
		return ctx.loadPrimitive(dt)

	case *arrow.BinaryType, *arrow.StringType:
		return ctx.loadBinary(dt)

	case *arrow.FixedSizeBinaryType:
		return ctx.loadFixedSizeBinary(dt)

	case *arrow.ListType:
		return ctx.loadList(dt)

	case *arrow.FixedSizeListType:
		return ctx.loadFixedSizeList(dt)

	case *arrow.StructType:
		return ctx.loadStruct(dt)

	case *arrow.MapType:
		return ctx.loadMap(dt)

	case arrow.ExtensionType:
		storage := ctx.loadArray(dt.StorageType())
		defer storage.Release()
		return array.NewExtensionArrayWithStorage(dt, storage)

	default:
		panic(xerrors.Errorf("array type %T not handled yet", dt))
	}
}

func (ctx *arrayLoaderContext) loadCommon(nbufs int) (*flatbuf.FieldNode, []*memory.Buffer) {
	buffers := make([]*memory.Buffer, 0, nbufs)
	field := ctx.field()

	var buf *memory.Buffer
	switch field.NullCount() {
	case 0:
		ctx.ibuffer++
	default:
		buf = ctx.buffer()
	}
	buffers = append(buffers, buf)

	return field, buffers
}

func (ctx *arrayLoaderContext) loadChild(dt arrow.DataType) arrow.Array {
	if ctx.max == 0 {
		panic("arrow/ipc: nested type limit reached")
	}
	ctx.max--
	sub := ctx.loadArray(dt)
	ctx.max++
	return sub
}

func (ctx *arrayLoaderContext) loadNull() arrow.Array {
	field := ctx.field()
	data := array.NewData(arrow.Null, int(field.Length()), nil, nil, int(field.NullCount()), 0)
	defer data.Release()

	return array.MakeFromData(data)
}

func (ctx *arrayLoaderContext) loadPrimitive(dt arrow.DataType) arrow.Array {
	field, buffers := ctx.loadCommon(2)

	switch field.Length() {
	case 0:
		buffers = append(buffers, nil)
		ctx.ibuffer++
	default:
		buffers = append(buffers, ctx.buffer())
	}

	defer releaseBuffers(buffers)

	data := array.NewData(dt, int(field.Length()), buffers, nil, int(field.NullCount()), 0)
	defer data.Release()

	return array.MakeFromData(data)
}

func (ctx *arrayLoaderContext) loadBinary(dt arrow.DataType) arrow.Array {
	field, buffers := ctx.loadCommon(3)
	buffers = append(buffers, ctx.buffer(), ctx.buffer())
	defer releaseBuffers(buffers)

	data := array.NewData(dt, int(field.Length()), buffers, nil, int(field.NullCount()), 0)
	defer data.Release()

	return array.MakeFromData(data)
}

func (ctx *arrayLoaderContext) loadFixedSizeBinary(dt *arrow.FixedSizeBinaryType) arrow.Array {
	field, buffers := ctx.loadCommon(2)
	buffers = append(buffers, ctx.buffer())
	defer releaseBuffers(buffers)

	data := array.NewData(dt, int(field.Length()), buffers, nil, int(field.NullCount()), 0)
	defer data.Release()

	return array.MakeFromData(data)
}

func (ctx *arrayLoaderContext) loadMap(dt *arrow.MapType) arrow.Array {
	field, buffers := ctx.loadCommon(2)
	buffers = append(buffers, ctx.buffer())
	defer releaseBuffers(buffers)

	sub := ctx.loadChild(dt.ValueType())
	defer sub.Release()

	data := array.NewData(dt, int(field.Length()), buffers, []arrow.ArrayData{sub.Data()}, int(field.NullCount()), 0)
	defer data.Release()

	return array.NewMapData(data)
}

func (ctx *arrayLoaderContext) loadList(dt *arrow.ListType) arrow.Array {
	field, buffers := ctx.loadCommon(2)
	buffers = append(buffers, ctx.buffer())
	defer releaseBuffers(buffers)

	sub := ctx.loadChild(dt.Elem())
	defer sub.Release()

	data := array.NewData(dt, int(field.Length()), buffers, []arrow.ArrayData{sub.Data()}, int(field.NullCount()), 0)
	defer data.Release()

	return array.NewListData(data)
}

func (ctx *arrayLoaderContext) loadFixedSizeList(dt *arrow.FixedSizeListType) arrow.Array {
	field, buffers := ctx.loadCommon(1)
	defer releaseBuffers(buffers)

	sub := ctx.loadChild(dt.Elem())
	defer sub.Release()

	data := array.NewData(dt, int(field.Length()), buffers, []arrow.ArrayData{sub.Data()}, int(field.NullCount()), 0)
	defer data.Release()

	return array.NewFixedSizeListData(data)
}

func (ctx *arrayLoaderContext) loadStruct(dt *arrow.StructType) arrow.Array {
	field, buffers := ctx.loadCommon(1)
	defer releaseBuffers(buffers)

	arrs := make([]arrow.Array, len(dt.Fields()))
	subs := make([]arrow.ArrayData, len(dt.Fields()))
	for i, f := range dt.Fields() {
		arrs[i] = ctx.loadChild(f.Type)
		subs[i] = arrs[i].Data()
	}
	defer func() {
		for i := range arrs {
			arrs[i].Release()
		}
	}()

	data := array.NewData(dt, int(field.Length()), buffers, subs, int(field.NullCount()), 0)
	defer data.Release()

	return array.NewStructData(data)
}

func readDictionary(meta *memory.Buffer, types dictTypeMap, r ReadAtSeeker) (int64, arrow.Array, error) {
	//	msg := flatbuf.GetRootAsMessage(meta.Bytes(), 0)
	//	var dictBatch flatbuf.DictionaryBatch
	//	initFB(&dictBatch, msg.Header)
	//
	//	id := dictBatch.Id()
	//	v, ok := types[id]
	//	if !ok {
	//		return id, nil, errors.Errorf("arrow/ipc: no type metadata for dictionary with ID=%d", id)
	//	}
	//
	//	fields := []arrow.Field{v}
	//
	//	// we need a schema for the record batch.
	//	schema := arrow.NewSchema(fields, nil)
	//
	//	// the dictionary is embedded in a record batch with a single column.
	//	recBatch := dictBatch.Data(nil)
	//
	//	var (
	//		batchMeta *memory.Buffer
	//		body      *memory.Buffer
	//	)
	//
	//
	//	ctx := &arrayLoaderContext{
	//		src: ipcSource{
	//			meta: &md,
	//			r:    bytes.NewReader(body.Bytes()),
	//		},
	//		max: kMaxNestingDepth,
	//	}
	//
	//	cols := make([]arrow.Array, len(schema.Fields()))
	//	for i, field := range schema.Fields() {
	//		cols[i] = ctx.loadArray(field.Type)
	//	}
	//
	//	batch := array.NewRecord(schema, cols, rows)

	panic("not implemented")
}

func releaseBuffers(buffers []*memory.Buffer) {
	for _, b := range buffers {
		if b != nil {
			b.Release()
		}
	}
}
