// Copyright 2014 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Reading of PDF tokens and objects from a raw byte stream.

package pdf

import (
	"fmt"
	"io"
	"strconv"
)

// A token is a PDF token in the input stream, one of the following Go types:
//
//	bool, a PDF boolean
//	int64, a PDF integer
//	float64, a PDF real
//	string, a PDF string literal
//	keyword, a PDF keyword
//	name, a PDF name without the leading slash
//
type token interface{}

// A pdfname is a PDF pdfname, without the leading slash.
type pdfname string

// A pdfkeyword is a PDF pdfkeyword.
// Delimiter tokens used in higher-level syntax,
// such as "<<", ">>", "[", "]", "{", "}", are also treated as keywords.
type pdfkeyword string

// A pdfbuffer holds buffered input bytes from the PDF file.
type pdfbuffer struct {
	r           io.Reader // source of data
	buf         []byte    // buffered data
	pos         int       // read index in buf
	offset      int64     // offset at end of buf; aka offset of next read
	tmp         []byte    // scratch space for accumulating token
	unread      []token   // queue of read but then unread tokens
	allowEOF    bool
	allowObjptr bool
	allowStream bool
	eof         bool
	key         []byte
	useAES      bool
	objptr      pdfobjptr
}

// newPdfBuffer returns a new buffer reading from r at the given offset.
func newPdfBuffer(r io.Reader, offset int64) *pdfbuffer {
	return &pdfbuffer{
		r:           r,
		offset:      offset,
		buf:         make([]byte, 0, 4096),
		allowObjptr: true,
		allowStream: true,
	}
}

func (b *pdfbuffer) seek(offset int64) {
	b.offset = offset
	b.buf = b.buf[:0]
	b.pos = 0
	b.unread = b.unread[:0]
}

func (b *pdfbuffer) readByte() byte {
	if b.pos >= len(b.buf) {
		b.reload()
		if b.pos >= len(b.buf) {
			return '\n'
		}
	}
	c := b.buf[b.pos]
	b.pos++
	return c
}

func (b *pdfbuffer) errorf(format string, args ...interface{}) {
	panic(fmt.Errorf(format, args...))
}

func (b *pdfbuffer) reload() bool {
	n := cap(b.buf) - int(b.offset%int64(cap(b.buf)))
	n, err := b.r.Read(b.buf[:n])
	if n == 0 && err != nil {
		b.buf = b.buf[:0]
		b.pos = 0
		if b.allowEOF && err == io.EOF {
			b.eof = true
			return false
		}
		b.errorf("malformed PDF: reading at offset %d: %v", b.offset, err)
		return false
	}
	b.offset += int64(n)
	b.buf = b.buf[:n]
	b.pos = 0
	return true
}

func (b *pdfbuffer) seekForward(offset int64) {
	for b.offset < offset {
		if !b.reload() {
			return
		}
	}
	b.pos = len(b.buf) - int(b.offset-offset)
}

func (b *pdfbuffer) readOffset() int64 {
	return b.offset - int64(len(b.buf)) + int64(b.pos)
}

func (b *pdfbuffer) unreadByte() {
	if b.pos > 0 {
		b.pos--
	}
}

func (b *pdfbuffer) unreadToken(t token) {
	b.unread = append(b.unread, t)
}

func (b *pdfbuffer) readToken() token {
	
	if n := len(b.unread); n > 0 {
		t := b.unread[n-1]
		b.unread = b.unread[:n-1]
		return t
	}
	
	// Find first non-space, non-comment byte.
	c := b.readByte()
	
	for {
		if isSpace(c) {
			if b.eof {
				return io.EOF
			}
			c = b.readByte()
		} else if c == '%' {
			for c != '\r' && c != '\n' {
				c = b.readByte()
			}
		} else {
			break
		}
	}
	

	switch c {
	case '<':
		if b.readByte() == '<' {
			return pdfkeyword("<<")
		}
		b.unreadByte()
		return b.readHexString()

	case '(':
		return b.readLiteralString()

	case '[', ']', '{', '}':
		return pdfkeyword(string(c))

	case '/':
		return b.readName()

	case '>':
		if b.readByte() == '>' {
			return pdfkeyword(">>")
		}
		b.unreadByte()
		fallthrough

	default:
		if isDelim(c) {
			b.errorf("unexpected delimiter %#q", rune(c))
			return nil
		}
		b.unreadByte()
		
		return b.readKeyword()
	}
}

func (b *pdfbuffer) readHexString() token {
	tmp := b.tmp[:0]
	for {
	Loop:
		c := b.readByte()
		if c == '>' {
			break
		}
		if isSpace(c) {
			goto Loop
		}
	Loop2:
		c2 := b.readByte()
		if isSpace(c2) {
			goto Loop2
		}
		x := unhex(c)<<4 | unhex(c2)
		if x < 0 {
			b.errorf("malformed hex string %c %c %s", c, c2, b.buf[b.pos:])
			break
		}
		tmp = append(tmp, byte(x))
	}
	b.tmp = tmp
	return string(tmp)
}

func unhex(b byte) int {
	switch {
	case '0' <= b && b <= '9':
		return int(b) - '0'
	case 'a' <= b && b <= 'f':
		return int(b) - 'a' + 10
	case 'A' <= b && b <= 'F':
		return int(b) - 'A' + 10
	}
	return -1
}

func (b *pdfbuffer) readLiteralString() token {
	tmp := b.tmp[:0]
	depth := 1
Loop:
	for {
		c := b.readByte()
		switch c {
		default:
			tmp = append(tmp, c)
		case '(':
			depth++
			tmp = append(tmp, c)
		case ')':
			if depth--; depth == 0 {
				break Loop
			}
			tmp = append(tmp, c)
		case '\\':
			switch c = b.readByte(); c {
			default:
				b.errorf("invalid escape sequence \\%c", c)
				tmp = append(tmp, '\\', c)
			case 'n':
				tmp = append(tmp, '\n')
			case 'r':
				tmp = append(tmp, '\r')
			case 'b':
				tmp = append(tmp, '\b')
			case 't':
				tmp = append(tmp, '\t')
			case 'f':
				tmp = append(tmp, '\f')
			case '(', ')', '\\':
				tmp = append(tmp, c)
			case '\r':
				if b.readByte() != '\n' {
					b.unreadByte()
				}
				fallthrough
			case '\n':
				// no append
			case '0', '1', '2', '3', '4', '5', '6', '7':
				x := int(c - '0')
				for i := 0; i < 2; i++ {
					c = b.readByte()
					if c < '0' || c > '7' {
						b.unreadByte()
						break
					}
					x = x*8 + int(c-'0')
				}
				if x > 255 {
					b.errorf("invalid octal escape \\%03o", x)
				}
				tmp = append(tmp, byte(x))
			}
		}
	}
	b.tmp = tmp
	return string(tmp)
}

func (b *pdfbuffer) readName() token {
	tmp := b.tmp[:0]
	for {
		c := b.readByte()
		if isDelim(c) || isSpace(c) {
			b.unreadByte()
			break
		}
		if c == '#' {
			x := unhex(b.readByte())<<4 | unhex(b.readByte())
			if x < 0 {
				b.errorf("malformed name")
			}
			tmp = append(tmp, byte(x))
			continue
		}
		tmp = append(tmp, c)
	}
	b.tmp = tmp
	return pdfname(string(tmp))
}

func (b *pdfbuffer) readKeyword() token {
	tmp := b.tmp[:0]
	for {
		c := b.readByte()
		if isDelim(c) || isSpace(c) {
			b.unreadByte()
			break
		}
		tmp = append(tmp, c)
	}
	b.tmp = tmp
	s := string(tmp)
	switch {
	case s == "true":
		return true
	case s == "false":
		return false
	case isInteger(s):
		x, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			b.errorf("invalid integer %s", s)
		}
		return x
	case isReal(s):
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			b.errorf("invalid real %s", s)
		}
		return x
	}
	return pdfkeyword(string(tmp))
}

func isInteger(s string) bool {
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if c < '0' || '9' < c {
			return false
		}
	}
	return true
}

func isReal(s string) bool {
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	if len(s) == 0 {
		return false
	}
	ndot := 0
	for _, c := range s {
		if c == '.' {
			ndot++
			continue
		}
		if c < '0' || '9' < c {
			return false
		}
	}
	return ndot == 1
}

// An pdfobject is a PDF syntax pdfobject, one of the following Go types:
//
//	bool, a PDF boolean
//	int64, a PDF integer
//	float64, a PDF real
//	string, a PDF string literal
//	name, a PDF name without the leading slash
//	dict, a PDF dictionary
//	array, a PDF array
//	stream, a PDF stream
//	objptr, a PDF pdfobject reference
//	objdef, a PDF pdfobject definition
//
// An pdfobject may also be nil, to represent the PDF null.
type pdfobject interface{}

type pdfdict map[pdfname]pdfobject

type pdfarray []pdfobject

type pdfstream struct {
	hdr    pdfdict
	ptr    pdfobjptr
	offset int64
}

type pdfobjptr struct {
	id  uint32
	gen uint16
}

type pdfobjdef struct {
	ptr pdfobjptr
	obj pdfobject
}

func (b *pdfbuffer) readObject() pdfobject {
	tok := b.readToken()
	if kw, ok := tok.(pdfkeyword); ok {
		switch kw {
		case "null":
			return nil
		case "<<":
			return b.readDict()
		case "[":
			return b.readArray()
		}
		b.errorf("unexpected keyword %q parsing object", kw)
		return nil
	}

	if str, ok := tok.(string); ok && b.key != nil && b.objptr.id != 0 {
		tok = decryptString(b.key, b.useAES, b.objptr, str)
	}

	if !b.allowObjptr {
		return tok
	}

	if t1, ok := tok.(int64); ok && int64(uint32(t1)) == t1 {
		tok2 := b.readToken()
		if t2, ok := tok2.(int64); ok && int64(uint16(t2)) == t2 {
			tok3 := b.readToken()
			switch tok3 {
			case pdfkeyword("R"):
				return pdfobjptr{uint32(t1), uint16(t2)}
			case pdfkeyword("obj"):
				old := b.objptr
				b.objptr = pdfobjptr{uint32(t1), uint16(t2)}
				obj := b.readObject()
				if _, ok := obj.(pdfstream); !ok {
					tok4 := b.readToken()
					if tok4 != pdfkeyword("endobj") {
						b.errorf("missing endobj after indirect object definition")
						b.unreadToken(tok4)
					}
				}
				b.objptr = old
				return pdfobjdef{pdfobjptr{uint32(t1), uint16(t2)}, obj}
			}
			b.unreadToken(tok3)
		}
		b.unreadToken(tok2)
	}
	return tok
}

func (b *pdfbuffer) readArray() pdfobject {
	var x pdfarray
	for {
		tok := b.readToken()
		if tok == nil || tok == pdfkeyword("]") {
			break
		}
		b.unreadToken(tok)
		x = append(x, b.readObject())
	}
	return x
}

func (b *pdfbuffer) readDict() pdfobject {
	x := make(pdfdict)
	for {
		tok := b.readToken()
		if tok == nil || tok == pdfkeyword(">>") {
			break
		}
		n, ok := tok.(pdfname)
		if !ok {
			b.errorf("unexpected non-name key %T(%v) parsing dictionary", tok, tok)
			continue
		}
		x[n] = b.readObject()
	}

	if !b.allowStream {
		return x
	}

	tok := b.readToken()
	if tok != pdfkeyword("stream") {
		b.unreadToken(tok)
		return x
	}

	switch b.readByte() {
	case '\r':
		if b.readByte() != '\n' {
			b.unreadByte()
		}
	case '\n':
		// ok
	default:
		b.errorf("stream keyword not followed by newline")
	}

	return pdfstream{x, b.objptr, b.readOffset()}
}

func isSpace(b byte) bool {
	switch b {
	case '\x00', '\t', '\n', '\f', '\r', ' ':
		return true
	}
	return false
}

func isDelim(b byte) bool {
	switch b {
	case '<', '>', '(', ')', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}
