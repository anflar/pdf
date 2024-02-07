// Copyright 205 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package pdf implements reading of PDF files.
//
// Overview
//
// PDF is Adobe's Portable Document Format, ubiquitous on the internet.
// A PDF document is a complex data format built on a fairly simple structure.
// This package exposes the simple structure along with some wrappers to
// extract basic information. If more complex information is needed, it is
// possible to extract that information by interpreting the structure exposed
// by this package.
//
// Specifically, a PDF is a data structure built from Values, each of which has
// one of the following Kinds:
//
//	Null, for the null object.
//	Integer, for an integer.
//	Real, for a floating-point number.
//	Bool, for a boolean value.
//	Name, for a name constant (as in /Helvetica).
//	String, for a string constant.
//	Dict, for a dictionary of name-value pairs.
//	Array, for an array of values.
//	Stream, for an opaque data stream and associated header dictionary.
//
// The accessors on Value—Int64, Float64, Bool, Name, and so on—return
// a view of the data as the given type. When there is no appropriate view,
// the accessor returns a zero result. For example, the Name accessor returns
// the empty string if called on a Value v for which v.Kind() != Name.
// Returning zero values this way, especially from the Dict and Array accessors,
// which themselves return Values, makes it possible to traverse a PDF quickly
// without writing any error checking. On the other hand, it means that mistakes
// can go unreported.
//
// The basic structure of the PDF file is exposed as the graph of Values.
//
// Most richer data structures in a PDF file are dictionaries with specific interpretations
// of the name-value pairs. The Font and Page wrappers make the interpretation
// of a specific Value as the corresponding type easier. They are only helpers, though:
// they are implemented only in terms of the Value API and could be moved outside
// the package. Equally important, traversal of other PDF data structures can be implemented
// in other packages as needed.
//
package pdf // import "rsc.io/pdf"

// BUG(rsc): The package is incomplete, although it has been used successfully on some
// large real-world PDF files.

// BUG(rsc): There is no support for closing open PDF files. If you drop all references to a Reader,
// the underlying reader will eventually be garbage collected.

// BUG(rsc): The library makes no attempt at efficiency. A value cache maintained in the Reader
// would probably help significantly.

// BUG(rsc): The support for reading encrypted files ir weak.

// BUG(rsc): The Value API does not support error reporting. The intent is to allow users to
// set an error reporting callback in Reader, but that code has not been implemented.

import (
	"bytes"
    "errors"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rc4"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
)

// A Reader is a single PDF file open for reading.
type Reader struct {
	f          io.ReaderAt
	end        int64
	xref       []xref
	//trailer    dict
	//trailerptr objptr
    Trailer    Value
	key        []byte
	useAES     bool
}

type xref struct {
	ptr      pdfobjptr
	inStream bool
	stream   pdfobjptr
	offset   int64
}


// Open opens a file for reading.
func Open(file string) (*Reader, error) {
	// TODO: Deal with closing file.
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return NewReader(f, fi.Size())
}

// NewReader opens a file for reading, using the data in f with the given total size.
func NewReader(f io.ReaderAt, size int64) (*Reader, error) {
	return NewReaderEncrypted(f, size, nil)
}

// NewReaderEncrypted opens a file for reading, using the data in f with the given total size.
// If the PDF is encrypted, NewReaderEncrypted calls pw repeatedly to obtain passwords
// to try. If pw returns the empty string, NewReaderEncrypted stops trying to decrypt
// the file and returns an error.
func NewReaderEncrypted(f io.ReaderAt, size int64, pw func() string) (*Reader,error) {
	buf := make([]byte, 10)
	f.ReadAt(buf, 0)
	if !bytes.HasPrefix(buf, []byte("%PDF-1.")) || buf[7] < '0' || buf[7] > '7' || buf[8] != '\r' && buf[8] != '\n' {
		return nil, fmt.Errorf("not a PDF file: invalid header")
	}
	end := size
	const endChunk = 100
	buf = make([]byte, endChunk)
	f.ReadAt(buf, end-endChunk)
	for len(buf) > 0 && buf[len(buf)-1] == '\n' || buf[len(buf)-1] == '\r' {
		buf = buf[:len(buf)-1]
	}
	buf = bytes.TrimRight(buf, "\r\n\t ")
	if !bytes.HasSuffix(buf, []byte("%%EOF")) {
		return nil, fmt.Errorf("not a PDF file: missing %%%%EOF")
	}
	i := findLastLine(buf, "startxref")
	if i < 0 {
		return nil, fmt.Errorf("malformed PDF file: missing final startxref")
	}

	r := &Reader{
		f:   f,
		end: end,
	}
	pos := end - endChunk + int64(i)
	b := newPdfBuffer(io.NewSectionReader(f, pos, end-pos), pos)
	if b.readToken() != pdfkeyword("startxref") {
		return nil, fmt.Errorf("malformed PDF file: missing startxref")
	}
	startxref, ok := b.readToken().(int64)
	if !ok {
		return nil, fmt.Errorf("malformed PDF file: startxref not followed by integer")
	}
	b = newPdfBuffer(io.NewSectionReader(r.f, startxref, r.end-startxref), startxref)
	xref, trailerptr, trailer, err := readXref(r, b)
	if err != nil {
		return nil, err
	}
	r.xref = xref
    r.Trailer = Value{r, trailerptr, trailer} 
	//r.trailer = trailer
	//r.trailerptr = trailerptr
	if trailer["Encrypt"] == nil {
		return r, nil
	}
	err = r.initEncrypt("")
	if err == nil {
		return r, nil
	}
	if pw == nil || err != ErrInvalidPassword {
		return nil, err
	}
	for {
		next := pw()
		if next == "" {
			break
		}
		if r.initEncrypt(next) == nil {
			return r, nil
		}
	}
	return nil, err
}


func readXref(r *Reader, b *pdfbuffer) ([]xref, pdfobjptr, pdfdict, error) {
	tok := b.readToken()
	if tok == pdfkeyword("xref") {
		return readXrefTable(r, b)
	}
	if _, ok := tok.(int64); ok {
		b.unreadToken(tok)
		return readXrefStream(r, b)
	}
	return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: cross-reference table not found: %v", tok)
}

func readXrefStream(r *Reader, b *pdfbuffer) ([]xref, pdfobjptr, pdfdict, error) {
	obj1 := b.readObject()
	obj, ok := obj1.(pdfobjdef)
	if !ok {
		return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: cross-reference table not found: %v", objfmt(obj1))
	}
	strmptr := obj.ptr
	strm, ok := obj.obj.(pdfstream)
	if !ok {
		return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: cross-reference table not found: %v", objfmt(obj))
	}
	if strm.hdr["Type"] != pdfname("XRef") {
		return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: xref stream does not have type XRef")
	}
	size, ok := strm.hdr["Size"].(int64)
	if !ok {
		return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: xref stream missing Size")
	}
	table := make([]xref, size)

	table, err := readXrefStreamData(r, strm, table, size)
	if err != nil {
		return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: %v", err)
	}

	for prevoff := strm.hdr["Prev"]; prevoff != nil; {
		off, ok := prevoff.(int64)
		if !ok {
			return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: xref Prev is not integer: %v", prevoff)
		}
		b := newPdfBuffer(io.NewSectionReader(r.f, off, r.end-off), off)
		obj1 := b.readObject()
		obj, ok := obj1.(pdfobjdef)
		if !ok {
			return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: xref prev stream not found: %v", objfmt(obj1))
		}
		prevstrm, ok := obj.obj.(pdfstream)
		if !ok {
			return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: xref prev stream not found: %v", objfmt(obj))
		}
		prevoff = prevstrm.hdr["Prev"]
		prev := Value{r, pdfobjptr{}, prevstrm}
		if prev.Kind() != Stream {
			return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: xref prev stream is not stream: %v", prev)
		}
        name, _ := prev.Name("Type")
		if name != "XRef" {
			return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: xref prev stream does not have type XRef")
		}
		psize, err := prev.Int64("Size")
        if err != nil {
            return nil, pdfobjptr{}, nil, err
        }
		if psize > size {
			return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: xref prev stream larger than last stream")
		}
		if table, err = readXrefStreamData(r, prev.data.(pdfstream), table, psize); err != nil {
			return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: reading xref prev stream: %v", err)
		}
	}

	return table, strmptr, strm.hdr, nil
}

func readXrefStreamData(r *Reader, strm pdfstream, table []xref, size int64) ([]xref, error) {
	index, _ := strm.hdr["Index"].(pdfarray)
	if index == nil {
		index = pdfarray{int64(0), size}
	}
	if len(index)%2 != 0 {
		return nil, fmt.Errorf("invalid Index array %v", objfmt(index))
	}
	ww, ok := strm.hdr["W"].(pdfarray)
	if !ok {
		return nil, fmt.Errorf("xref stream missing W array")
	}

	var w []int
	for _, x := range ww {
		i, ok := x.(int64)
		if !ok || int64(int(i)) != i {
			return nil, fmt.Errorf("invalid W array %v", objfmt(ww))
		}
		w = append(w, int(i))
	}
	if len(w) < 3 {
		return nil, fmt.Errorf("invalid W array %v", objfmt(ww))
	}

	v := Value{r, pdfobjptr{}, strm}
	wtotal := 0
	for _, wid := range w {
		wtotal += wid
	}
	buf := make([]byte, wtotal)
	data := v.Reader()
	for len(index) > 0 {
		start, ok1 := index[0].(int64)
		n, ok2 := index[1].(int64)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("malformed Index pair %v %v %T %T", objfmt(index[0]), objfmt(index[1]), index[0], index[1])
		}
		index = index[2:]
		for i := 0; i < int(n); i++ {
			_, err := io.ReadFull(data, buf)
			if err != nil {
				return nil, fmt.Errorf("error reading xref stream: %v", err)
			}
			v1 := decodeInt(buf[0:w[0]])
			if w[0] == 0 {
				v1 = 1
			}
			v2 := decodeInt(buf[w[0] : w[0]+w[1]])
			v3 := decodeInt(buf[w[0]+w[1] : w[0]+w[1]+w[2]])
			x := int(start) + i
			for cap(table) <= x {
				table = append(table[:cap(table)], xref{})
			}
			if table[x].ptr != (pdfobjptr{}) {
				continue
			}
			switch v1 {
			case 0:
				table[x] = xref{ptr: pdfobjptr{0, 65535}}
			case 1:
				table[x] = xref{ptr: pdfobjptr{uint32(x), uint16(v3)}, offset: int64(v2)}
			case 2:
				table[x] = xref{ptr: pdfobjptr{uint32(x), 0}, inStream: true, stream: pdfobjptr{uint32(v2), 0}, offset: int64(v3)}
			default:
				fmt.Printf("invalid xref stream type %d: %x\n", v1, buf)
			}
		}
	}
	return table, nil
}

func decodeInt(b []byte) int {
	x := 0
	for _, c := range b {
		x = x<<8 | int(c)
	}
	return x
}

func readXrefTable(r *Reader, b *pdfbuffer) ([]xref, pdfobjptr, pdfdict, error) {
	var table []xref

	table, err := readXrefTableData(b, table)
	if err != nil {
		return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: %v", err)
	}

	trailer, ok := b.readObject().(pdfdict)
	if !ok {
		return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: xref table not followed by trailer dictionary")
	}

	for prevoff := trailer["Prev"]; prevoff != nil; {
		off, ok := prevoff.(int64)
		if !ok {
			return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: xref Prev is not integer: %v", prevoff)
		}
		b := newPdfBuffer(io.NewSectionReader(r.f, off, r.end-off), off)
		tok := b.readToken()
		if tok != pdfkeyword("xref") {
			return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: xref Prev does not point to xref")
		}
		table, err = readXrefTableData(b, table)
		if err != nil {
			return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: %v", err)
		}

		trailer, ok := b.readObject().(pdfdict)
		if !ok {
			return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: xref Prev table not followed by trailer dictionary")
		}
		prevoff = trailer["Prev"]
	}

	size, ok := trailer[pdfname("Size")].(int64)
	if !ok {
		return nil, pdfobjptr{}, nil, fmt.Errorf("malformed PDF: trailer missing /Size entry")
	}

	if size < int64(len(table)) {
		table = table[:size]
	}

	return table, pdfobjptr{}, trailer, nil
}

func readXrefTableData(b *pdfbuffer, table []xref) ([]xref, error) {
	for {
		tok := b.readToken()
		if tok == pdfkeyword("trailer") {
			break
		}
		start, ok1 := tok.(int64)
		n, ok2 := b.readToken().(int64)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("malformed xref table")
		}
		for i := 0; i < int(n); i++ {
			off, ok1 := b.readToken().(int64)
			gen, ok2 := b.readToken().(int64)
			alloc, ok3 := b.readToken().(pdfkeyword)
			if !ok1 || !ok2 || !ok3 || alloc != pdfkeyword("f") && alloc != pdfkeyword("n") {
				return nil, fmt.Errorf("malformed xref table")
			}
			x := int(start) + i
			for cap(table) <= x {
				table = append(table[:cap(table)], xref{})
			}
			if len(table) <= x {
				table = table[:x+1]
			}
			if alloc == "n" && table[x].offset == 0 {
				table[x] = xref{ptr: pdfobjptr{uint32(x), uint16(gen)}, offset: int64(off)}
			}
		}
	}
	return table, nil
}

func findLastLine(buf []byte, s string) int {
	bs := []byte(s)
	max := len(buf)
	for {
		i := bytes.LastIndex(buf[:max], bs)
		if i <= 0 || i+len(bs) >= len(buf) {
			return -1
		}
		if (buf[i-1] == '\n' || buf[i-1] == '\r') && (buf[i+len(bs)] == '\n' || buf[i+len(bs)] == '\r') {
			return i
		}
		max = i
	}
}

// A Value is a single PDF value, such as an integer, dictionary, or array.
// The zero Value is a PDF null (Kind() == Null, IsNull() = true).
type Value struct {
	r    *Reader
	ptr  pdfobjptr
	data interface{}
}


// IsNull reports whether the value is a null. It is equivalent to Kind() == Null.
func (v Value) IsNull() bool {
	return v.data == nil
}

// A ValueKind specifies the kind of data underlying a Value.
type ValueKind int

// The PDF value kinds.
const (
	Null ValueKind = iota
	Bool
	Integer
	Real
	String
	Name
	Dict
	Array
	Stream
)

// Kind reports the kind of value underlying v.
func (v Value) Kind() ValueKind {
	switch v.data.(type) {
	default:
		return Null
	case bool:
		return Bool
	case int64:
		return Integer
	case float64:
		return Real
	case string:
		return String
	case pdfname:
		return Name
	case pdfdict:
		return Dict
	case pdfarray:
		return Array
	case pdfstream:
		return Stream
	}
}

// String returns a textual representation of the value v.
// Note that String is not the accessor for values with Kind() == String.
// To access such values, see RawString, Text, and TextFromUTF16.
func (v Value) String() string {
	return objfmt(v.data)
}

func objfmt(x interface{}) string {
	switch x := x.(type) {
	default:
		return fmt.Sprint(x)
	case string:
		if isPDFDocEncoded(x) {
			return strconv.Quote(pdfDocDecode(x))
		}
		if isUTF16(x) {
			return strconv.Quote(utf16Decode(x[2:]))
		}
		return strconv.Quote(x)
	case pdfname:
		return "/" + string(x)
	case pdfdict:
		var keys []string
		for k := range x {
			keys = append(keys, string(k))
		}
		sort.Strings(keys)
		var buf bytes.Buffer
		buf.WriteString("<<")
		for i, k := range keys {
			elem := x[pdfname(k)]
			if i > 0 {
				buf.WriteString(" ")
			}
			buf.WriteString("/")
			buf.WriteString(k)
			buf.WriteString(" ")
			buf.WriteString(objfmt(elem))
		}
		buf.WriteString(">>")
		return buf.String()

	case pdfarray:
		var buf bytes.Buffer
		buf.WriteString("[")
		for i, elem := range x {
			if i > 0 {
				buf.WriteString(" ")
			}
			buf.WriteString(objfmt(elem))
		}
		buf.WriteString("]")
		return buf.String()

	case pdfstream:
		return fmt.Sprintf("%v@%d", objfmt(x.hdr), x.offset)

	case pdfobjptr:
		return fmt.Sprintf("%d %d R", x.id, x.gen)

	case pdfobjdef:
		return fmt.Sprintf("{%d %d obj}%v", x.ptr.id, x.ptr.gen, objfmt(x.obj))
	}
}

// Bool returns v's boolean value.
// If v.Kind() != Bool, Bool returns false.
func (v Value) Bool() bool {
	x, ok := v.data.(bool)
	if !ok {
		return false
	}
	return x
}

// Int64 returns v's int64 value.
// If v.Kind() != Int64, Int64 returns 0.
func (v Value) Int(path ...interface{}) (int, error) {
    var err error
    v2 := v
    if len(path) > 0{
        v2, err = v.Walk(path...)
        if err != nil {
            return 0, err 
        }
    }
    val, ok := v2.data.(int)
    if !ok {
        return 0, fmt.Errorf("Could not cast into int64")
    }
    return val, nil
}

// Int64 returns v's int64 value.
// If v.Kind() != Int64, Int64 returns 0.
func (v Value) Int64(path ...interface{}) (int64, error) {
    var err error
    v2 := v
    if len(path) > 0{
        v2, err = v.Walk(path...)
        if err != nil {
            return 0, err 
        }
    }
    val, ok := v2.data.(int64)
    if !ok {
        return 0, fmt.Errorf("Could not cast into int64")
    }
    return val, nil
}

// Float64 returns v's float64 value, converting from integer if necessary.
// If v.Kind() != Float64 and v.Kind() != Int64, Float64 returns 0.
func (v Value) Float64() float64 {
	x, ok := v.data.(float64)
	if !ok {
		x, ok := v.data.(int64)
		if ok {
			return float64(x)
		}
		return 0
	}
	return x
}

// RawString returns v's string value.
// If v.Kind() != String, RawString returns the empty string.
func (v Value) RawString() string {
	x, ok := v.data.(string)
	if !ok {
		return ""
	}
	return x
}

// Text returns v's string value interpreted as a ``text string'' (defined in the PDF spec)
// and converted to UTF-8.
// If v.Kind() != String, Text returns the empty string.
func (v Value) Text() string {
	x, ok := v.data.(string)
	if !ok {
		return ""
	}
	if isPDFDocEncoded(x) {
		return pdfDocDecode(x)
	}
	if isUTF16(x) {
		return utf16Decode(x[2:])
	}
	return x
}

// TextFromUTF16 returns v's string value interpreted as big-endian UTF-16
// and then converted to UTF-8.
// If v.Kind() != String or if the data is not valid UTF-16, TextFromUTF16 returns
// the empty string.
func (v Value) TextFromUTF16() string {
	x, ok := v.data.(string)
	if !ok {
		return ""
	}
	if len(x)%2 == 1 {
		return ""
	}
	if x == "" {
		return ""
	}
	return utf16Decode(x)
}

// Name returns v's name value.
// If v.Kind() != Name, Name returns the empty string.
// The returned name does not include the leading slash:
// if v corresponds to the name written using the syntax /Helvetica,
// Name() == "Helvetica".
func (v Value) Name(path ...interface{}) (string, error) {
    var err error
    v2 := v
    if len(path) > 0{
        v2, err = v.Walk(path...)
        if err != nil {
            return "", err 
        }
    }
	x, ok := v2.data.(pdfname)
	if !ok {
		return "", fmt.Errorf("Object is not a valid name")
	}
	return string(x), nil
}

// Key returns the value associated with the given name key in the dictionary v.
// Like the result of the Name method, the key should not include a leading slash.
// If v is a stream, Key applies to the stream's header dictionary.
// If v.Kind() != Dict and v.Kind() != Stream, Key returns a null Value.
var ErrNotAValidStream = errors.New("Not a valid stream object")
func (v Value) Key(key string) (Value, error) {
	x, ok := v.data.(pdfdict)
	if !ok {
		strm, ok := v.data.(pdfstream)
		if !ok {
			return Value{}, ErrNotAValidStream
		}
		x = strm.hdr
	}
	return v.r.resolve(v.ptr, x[pdfname(key)])
}

type WalkType int

const (
    WalkChildren = iota
    WalkInherited
)


func (v Value) DoWalkChildren(path ...interface{}) (Value, error) {
    current := v
    for _, p := range path[:len(path)-1] { // Adjust loop to exclude the last path element for special handling
        switch p := p.(type) {
        case string:
            var err error
            current, err = current.Key(p)
            if err != nil {
                return Value{}, err
            }
        case int:
            var err error
            current, err = current.Index(p)
            if err != nil {
                return Value{}, err
            }
        default:
            return Value{}, fmt.Errorf("unsupported path element type %T", p)
        }
    }
    // Apply the type assertion function to the final Value
    return current, nil
}

func (v Value) Walk(path ...interface{}) (Value, error) {
    var wt WalkType = WalkChildren

    if len(path) == 0 {
        return v, nil
    }
    switch path[0].(type){
    case WalkType:
        extracted, ok := path[0].(WalkType)
        if ok{
            wt = extracted
            path = path[1:]
            switch (wt){
            case WalkChildren:
                return v.DoWalkChildren(path...)
            case WalkInherited:
                panic("WalkInherited not implemented yet!")
            }
        }
    default:
        //2 
    }
    return v.DoWalkChildren(path...)
}

// Keys returns a sorted list of the keys in the dictionary v.
// If v is a stream, Keys applies to the stream's header dictionary.
// If v.Kind() != Dict and v.Kind() != Stream, Keys returns nil.
func (v Value) Keys() []string {
	x, ok := v.data.(pdfdict)
	if !ok {
		strm, ok := v.data.(pdfstream)
		if !ok {
			return nil
		}
		x = strm.hdr
	}
	keys := []string{} // not nil
	for k := range x {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	return keys
}

// Index returns the i'th element in the array v.
// If v.Kind() != Array or if i is outside the array bounds,
// Index returns a null Value.
func (v Value) Index(i int) (Value, error) {
	x, ok := v.data.(pdfarray)
	if !ok || i < 0 || i >= len(x) {
		return Value{}, ErrNotAValidStream
	}
	return v.r.resolve(v.ptr, x[i])
}

// Len returns the length of the array v.
// If v.Kind() != Array, Len returns 0.
func (v Value) Len() int {
	x, ok := v.data.(pdfarray)
	if !ok {
		return 0
	}
	return len(x)
}

var ErrNotAStream = errors.New("Object is not a stream")
var ErrNotObjectStream = errors.New("Object is not an object stream")
var ErrMissingFirst = errors.New("Stream is missing property first")
var ErrExtendsNotValidStream = errors.New("Stream contains Extends property, but this is not a valid stream")
var ErrObjectOutOfBounds = errors.New("Object out of bounds")
var ErrUnexpectedValueType = errors.New("Unexpected value type %T in resolve")

func (r *Reader) resolve(parent pdfobjptr, x interface{}) (Value, error){
    //First handle easy cases
    ptr, ok := x.(pdfobjptr)
    if !ok {
        switch x := x.(type) {
        case nil, bool, int64, float64, pdfname, pdfdict, pdfarray, pdfstream:
            return Value{r, parent, x}, nil
        case string:
            return Value{r, parent, x}, nil
        default:
            return Value{}, ErrUnexpectedValueType
        }
    }
    
    if ptr.id >= uint32(len(r.xref)) {
        return Value{}, ErrObjectOutOfBounds
    }
    xref := r.xref[ptr.id]
    if xref.ptr != ptr || !xref.inStream && xref.offset == 0 {
        return Value{}, nil
    }
    var obj pdfobject
    if xref.inStream {
        strm, err := r.resolve(parent, xref.stream)
        if err != nil {
            return Value{}, err
        }
    Search:
        for {
            if strm.Kind() != Stream {
                return Value{}, ErrNotAStream
            }
            name, _ := strm.Name("Type")
            if name != "ObjStm" {
                panic("not an object stream")
            }
            n, _ := strm.Int("N")
            first, err := strm.Int64("First")
            if err != nil{
                panic("missing First")
            }
            b := newPdfBuffer(strm.Reader(), 0)
            b.allowEOF = true
            for i := 0; i < n; i++ {
                id, _ := b.readToken().(int64)
                off, _ := b.readToken().(int64)
                if uint32(id) == ptr.id {
                    b.seekForward(first + off)
                    x = b.readObject()
                    break Search
                }
            }
            ext, err := strm.Key("Extends")
            if err != nil {
                panic("error reading stream")
            }
            if ext.Kind() != Stream {
                panic("cannot find object in stream")
            }
            strm = ext
        }
    } else {
        b := newPdfBuffer(io.NewSectionReader(r.f, xref.offset, r.end-xref.offset), xref.offset)
        b.key = r.key
        b.useAES = r.useAES
        obj = b.readObject()
        def, ok := obj.(pdfobjdef)
        if !ok {
            panic(fmt.Errorf("loading %v: found %T instead of objdef", ptr, obj))
            //return Value{}
        }
        if def.ptr != ptr {
            panic(fmt.Errorf("loading %v: found %v", ptr, def.ptr))
        }
        x = def.obj
    }
    parent = ptr

    switch x := x.(type) {
    case nil, bool, int64, float64, pdfname, pdfdict, pdfarray, pdfstream:
        return Value{r, parent, x}, nil
    case string:
        return Value{r, parent, x}, nil
    default:
        return Value{}, ErrUnexpectedValueType
    }
}

type errorReadCloser struct {
	err error
}

func (e *errorReadCloser) Read([]byte) (int, error) {
	return 0, e.err
}

func (e *errorReadCloser) Close() error {
	return e.err
}

// Reader returns the data contained in the stream v.
// If v.Kind() != Stream, Reader returns a ReadCloser that
// responds to all reads with a ``stream not present'' error.
func (v Value) Reader() io.ReadCloser {
	x, ok := v.data.(pdfstream)
	if !ok {
		return &errorReadCloser{fmt.Errorf("stream not present")}
	}
	var rd io.Reader
    length, _ :=  v.Int64("Length")
	rd = io.NewSectionReader(v.r.f, x.offset, length)
	if v.r.key != nil {
		rd = decryptStream(v.r.key, v.r.useAES, x.ptr, rd)
	}
	filter, _ := v.Key("Filter")
	param, _ := v.Key("DecodeParms")
	switch filter.Kind() {
	default:
		panic(fmt.Errorf("unsupported filter %v", filter))
	case Null:
		// ok
	case Name:
        name, _ := filter.Name()
		rd = applyFilter(rd, name, param)
	case Array:
		for i := 0; i < filter.Len(); i++ {
            flt, _ := filter.Index(i)
            name, _ := flt.Name()
			rd = applyFilter(rd, name, flt)
		}
	}

	return io.NopCloser(rd)
}

func applyFilter(rd io.Reader, name string, param Value) io.Reader {
	switch name {
	default:
		panic("unknown filter " + name)
	case "FlateDecode":
		zr, err := zlib.NewReader(rd)
		if err != nil {
			panic(err)
		}
		pred, err := param.Int64("Predictor")
        if err != nil {
            return zr
        }
		columns, err := param.Int64("Columns")
        if err != nil{
            columns = 1
        }
        
		switch pred {
		default:
			fmt.Println("unknown predictor", pred)
			panic("pred")
		case 1:
			return zr
		case 12:
			return &pngUpReader{r: zr, hist: make([]byte, 1+columns), tmp: make([]byte, 1+columns)}
		}
	}
}

type pngUpReader struct {
	r    io.Reader
	hist []byte
	tmp  []byte
	pend []byte
}

func (r *pngUpReader) Read(b []byte) (int, error) {
	n := 0
	for len(b) > 0 {
		if len(r.pend) > 0 {
			m := copy(b, r.pend)
			n += m
			b = b[m:]
			r.pend = r.pend[m:]
			continue
		}
		_, err := io.ReadFull(r.r, r.tmp)
		if err != nil {
			return n, err
		}
		if r.tmp[0] != 2 {
			return n, fmt.Errorf("malformed PNG-Up encoding")
		}
		for i, b := range r.tmp {
			r.hist[i] += b
		}
		r.pend = r.hist[1:]
	}
	return n, nil
}

var passwordPad = []byte{
	0x28, 0xBF, 0x4E, 0x5E, 0x4E, 0x75, 0x8A, 0x41, 0x64, 0x00, 0x4E, 0x56, 0xFF, 0xFA, 0x01, 0x08,
	0x2E, 0x2E, 0x00, 0xB6, 0xD0, 0x68, 0x3E, 0x80, 0x2F, 0x0C, 0xA9, 0xFE, 0x64, 0x53, 0x69, 0x7A,
}

func (r *Reader) initEncrypt(password string) error {
	// See PDF 32000-1:2008, §7.6.
    e, err := r.Trailer.Key("Encrypt") //r.resolve(objptr{}, r.trailer["Encrypt"])
    if err != nil {
        return fmt.Errorf("Failed to resolve Encrypt key")
    }
	encrypt, _ := e.data.(pdfdict)
	if encrypt["Filter"] != pdfname("Standard") {
		return fmt.Errorf("unsupported PDF: encryption filter %v", objfmt(encrypt["Filter"]))
	}
	n, _ := encrypt["Length"].(int64)
	if n == 0 {
		n = 40
	}
	if n%8 != 0 || n > 128 || n < 40 {
		return fmt.Errorf("malformed PDF: %d-bit encryption key", n)
	}
	V, _ := encrypt["V"].(int64)
	if V != 1 && V != 2 && (V != 4 || !okayV4(encrypt)) {
		return fmt.Errorf("unsupported PDF: encryption version V=%d; %v", V, objfmt(encrypt))
	}

	ids, err2 := r.Trailer.Key("ID")
	if err2 != nil || ids.Len() < 1 {
		return fmt.Errorf("malformed PDF: missing ID in trailer")
	}
	idstr1, err2 := ids.Index(0)
    idstr := idstr1.RawString()
	if err2 != nil {
		return fmt.Errorf("malformed PDF: missing ID in trailer")
	}
	ID := []byte(idstr)

	R, _ := encrypt["R"].(int64)
	if R < 2 {
		return fmt.Errorf("malformed PDF: encryption revision R=%d", R)
	}
	if R > 4 {
		return fmt.Errorf("unsupported PDF: encryption revision R=%d", R)
	}
	O, _ := encrypt["O"].(string)
	U, _ := encrypt["U"].(string)
	if len(O) != 32 || len(U) != 32 {
		return fmt.Errorf("malformed PDF: missing O= or U= encryption parameters")
	}
	p, _ := encrypt["P"].(int64)
	P := uint32(p)

	// TODO: Password should be converted to Latin-1.
	pw := []byte(password)
	h := md5.New()
	if len(pw) >= 32 {
		h.Write(pw[:32])
	} else {
		h.Write(pw)
		h.Write(passwordPad[:32-len(pw)])
	}
	h.Write([]byte(O))
	h.Write([]byte{byte(P), byte(P >> 8), byte(P >> 16), byte(P >> 24)})
	h.Write([]byte(ID))
	key := h.Sum(nil)

	if R >= 3 {
		for i := 0; i < 50; i++ {
			h.Reset()
			h.Write(key[:n/8])
			key = h.Sum(key[:0])
		}
		key = key[:n/8]
	} else {
		key = key[:40/8]
	}

	c, err := rc4.NewCipher(key)
	if err != nil {
		return fmt.Errorf("malformed PDF: invalid RC4 key: %v", err)
	}

	var u []byte
	if R == 2 {
		u = make([]byte, 32)
		copy(u, passwordPad)
		c.XORKeyStream(u, u)
	} else {
		h.Reset()
		h.Write(passwordPad)
		h.Write([]byte(ID))
		u = h.Sum(nil)
		c.XORKeyStream(u, u)

		for i := 1; i <= 19; i++ {
			key1 := make([]byte, len(key))
			copy(key1, key)
			for j := range key1 {
				key1[j] ^= byte(i)
			}
			c, _ = rc4.NewCipher(key1)
			c.XORKeyStream(u, u)
		}
	}

	if !bytes.HasPrefix([]byte(U), u) {
		return ErrInvalidPassword
	}

	r.key = key
	r.useAES = V == 4

	return nil
}

var ErrInvalidPassword = fmt.Errorf("encrypted PDF: invalid password")

func okayV4(encrypt pdfdict) bool {
	cf, ok := encrypt["CF"].(pdfdict)
	if !ok {
		return false
	}
	stmf, ok := encrypt["StmF"].(pdfname)
	if !ok {
		return false
	}
	strf, ok := encrypt["StrF"].(pdfname)
	if !ok {
		return false
	}
	if stmf != strf {
		return false
	}
	cfparam, ok := cf[stmf].(pdfdict)
	if cfparam["AuthEvent"] != nil && cfparam["AuthEvent"] != pdfname("DocOpen") {
		return false
	}
	if cfparam["Length"] != nil && cfparam["Length"] != int64(16) {
		return false
	}
	if cfparam["CFM"] != pdfname("AESV2") {
		return false
	}
	return true
}

func cryptKey(key []byte, useAES bool, ptr pdfobjptr) []byte {
	h := md5.New()
	h.Write(key)
	h.Write([]byte{byte(ptr.id), byte(ptr.id >> 8), byte(ptr.id >> 16), byte(ptr.gen), byte(ptr.gen >> 8)})
	if useAES {
		h.Write([]byte("sAlT"))
	}
	return h.Sum(nil)
}

func decryptString(key []byte, useAES bool, ptr pdfobjptr, x string) string {
	key = cryptKey(key, useAES, ptr)
	if useAES {
		panic("AES not implemented")
	} else {
		c, _ := rc4.NewCipher(key)
		data := []byte(x)
		c.XORKeyStream(data, data)
		x = string(data)
	}
	return x
}

func decryptStream(key []byte, useAES bool, ptr pdfobjptr, rd io.Reader) io.Reader {
	key = cryptKey(key, useAES, ptr)
	if useAES {
		cb, err := aes.NewCipher(key)
		if err != nil {
			panic("AES: " + err.Error())
		}
		iv := make([]byte, 16)
		io.ReadFull(rd, iv)
		cbc := cipher.NewCBCDecrypter(cb, iv)
		rd = &cbcReader{cbc: cbc, rd: rd, buf: make([]byte, 16)}
	} else {
		c, _ := rc4.NewCipher(key)
		rd = &cipher.StreamReader{S: c, R: rd}
	}
	return rd
}

type cbcReader struct {
	cbc  cipher.BlockMode
	rd   io.Reader
	buf  []byte
	pend []byte
}

func (r *cbcReader) Read(b []byte) (n int, err error) {
	if len(r.pend) == 0 {
		_, err = io.ReadFull(r.rd, r.buf)
		if err != nil {
			return 0, err
		}
		r.cbc.CryptBlocks(r.buf, r.buf)
		r.pend = r.buf
	}
	n = copy(b, r.pend)
	r.pend = r.pend[n:]
	return n, nil
}





