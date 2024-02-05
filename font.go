// Copyright 2014 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pdf

import (
	"fmt"
	"unicode/utf16"
)

type WidthGrabber interface {
	Width(uint32) float64
}

// A Font represent a font in a PDF file.
// The methods interpret a Font dictionary stored in V.
type Font struct {
	V   Value
	enc TextEncoding
}

func FontFromValue(v Value) Font {
	f := Font{V: v}

	wg, ok := CreateCIDWidthGrabber(f)
	if !ok {
		wg, ok = CreateDefaultWidthGrabber(f)
	}

	f.enc = Encoder(f, wg)
	return f
}

type DefaultWidthGrabber struct {
	first uint32
	last uint32
	widths []float64
}

func CreateDefaultWidthGrabber(f Font) (WidthGrabber, bool){
	first := uint32(f.FirstChar())
	last := uint32(f.LastChar())
	w := f.V.Key("Widths")	
		  widths := make([]float64, w.Len())	
	for i := 0; i < w.Len(); i += 1 {
		widths[i] = w.Index(i).Float64()
	}
	return DefaultWidthGrabber{first, last, widths}, true 

}

func (wg DefaultWidthGrabber) Width(code uint32) float64 {
	if code < wg.first || code >= wg.last{
		return 0
	}
	return wg.widths[code-wg.first]
}

type WidthRange1 struct {
	start  uint32
	end    uint32
	widths []float64
}

type WidthRange2 struct {
	start uint32
	end   uint32
	width float64
}

type CIDWidthGrabber struct {
	wmap1 []WidthRange1 
	wmap2 []WidthRange2
	defaultwidth float64
}

func CreateCIDWidthGrabber(f Font) (WidthGrabber, bool) {
	df := f.V.Key("DescendantFonts")

	if df.Kind() != 0 {
		return nil, false
	}

	w := df.Index(0).Key("W")
	if w.Kind() != 0 {
		return nil, false
	}

	dw := f.V.Key("DescendantFonts").Index(0).Key("DW").Float64()
	cw := CIDWidthGrabber{[]WidthRange1{}, []WidthRange2{}, dw}
	sz := 3
	for i := 0; i < w.Len(); i += sz {
		glyph := uint32(w.Index(i).Int64())

		unk := w.Index(i + 1)
		if unk.Kind() == Array {
			sz = 2
		  widths := make([]float64, unk.Len())	
			for j := 0; j < unk.Len(); i += 1 {
					widths = append(widths, unk.Index(j).Float64())
			}
			wr1 := WidthRange1{glyph, glyph+uint32(unk.Len()), widths}
			cw.wmap1 = append(cw.wmap1, wr1)
			/*if code >= glyph && code < glyph+uint32(widths.Len()) {
				return widths.Index(int(code - glyph)).Float64(), true
			}*/
		} else {
			sz = 3
			endglyph := uint32(unk.Int64())
			width := w.Index(i + 2).Float64()
			wr := WidthRange2{glyph, endglyph, width}
			cw.wmap2 = append(cw.wmap2, wr)
			/*if code >= glyph && code < endglyph {
				return width, true
			}*/
		}
	}

	return cw, true
}

// Width returns the width of the given code point.
func (wg CIDWidthGrabber) Width(code uint32) float64 {
	for _, wr1 := range wg.wmap1{
		if code >= wr1.start && code < wr1.end{
			return wr1.widths[code-wr1.start]
		}
	}
	for _, wr2 := range wg.wmap2{
		if code >= wr2.start && code < wr2.end{
			return wr2.width
		}
	}
	return wg.defaultwidth 
}

// BaseFont returns the font's name (BaseFont property).
func (f Font) BaseFont() string {
	return f.V.Key("BaseFont").Name()
}

func (f Font) FontWeight() float64 {
	fd := f.V.Key("FontDescriptor")

	if fd.Kind() == 0 {
		fd = f.V.Key("DescendantFonts").Index(0).Key("FontDescriptor")

	}

	return fd.Key("FontWeight").Float64()
}

// FirstChar returns the code point of the first character in the font.
func (f Font) FirstChar() int {
	return int(f.V.Key("FirstChar").Int64())
}

// LastChar returns the code point of the last character in the font.
func (f Font) LastChar() int {
	return int(f.V.Key("LastChar").Int64())
}

// Encoder returns the encoding between font code point sequences and UTF-8.
func Encoder(f Font, wg WidthGrabber) TextEncoding {
	enc := f.V.Key("Encoding")
	switch enc.Kind() {
	case Name:
		switch enc.Name() {
		case "WinAnsiEncoding":
			return &byteEncoder{f, wg, &winAnsiEncoding}
		case "MacRomanEncoding":
			return &byteEncoder{f, wg, &macRomanEncoding}
		case "Identity-H", "Identity-V":
			// TODO: Should be big-endian UCS-2 decoder
		default:
			println("unknown encoding", enc.Name())
			return &nopEncoder{f, wg}
		}
	case Dict:
		return &dictEncoder{f, wg, enc.Key("Differences")}
	case Null:
		// ok, try ToUnicode
	default:
		println("unexpected encoding", enc.String())
		return &nopEncoder{f, wg}
	}

	toUnicode := f.V.Key("ToUnicode")

	if toUnicode.Kind() == Stream {
		m := readCmap(f, wg, toUnicode)
		if m == nil {
			return &nopEncoder{f, wg}
		}
		return m
	}

	return &byteEncoder{f, wg, &pdfDocEncoding}
}

type dictEncoder struct {
	f  Font
	wg WidthGrabber
	v  Value
}

func (f Font) Decode(raw string) (text []PositionedChar) {
	return f.enc.Decode(raw)
}

func (e *dictEncoder) Decode(raw string) (text []PositionedChar) {
	r := []PositionedChar{}
	for i := 0; i < len(raw); i++ {
		ch := rune(raw[i])
		n := -1
		for j := 0; j < e.v.Len(); j++ {
			x := e.v.Index(j)
			if x.Kind() == Integer {
				n = int(x.Int64())
				continue
			}
			if x.Kind() == Name {
				if int(raw[i]) == n {
					r := nameToRune[x.Name()]
					if r != 0 {
						ch = r
						break
					}
				}
				n++
			}
		}
		r = append(r, PositionedChar{[]rune{ch}, e.wg.Width(uint32(ch))})
	}
	return r
}

type PositionedChar struct {
	Text  []rune
	Width float64
}

// A TextEncoding represents a mapping between
// font code points and UTF-8 text.
type TextEncoding interface {
	// Decode returns the UTF-8 text corresponding to
	// the sequence of code points in raw.
	Decode(raw string) (text []PositionedChar)
}

type nopEncoder struct {
	f  Font
	wg WidthGrabber
}

func (e *nopEncoder) Decode(raw string) (text []PositionedChar) {
	r := []PositionedChar{}
	for i := 0; i < len(raw); i++ {
		r = append(r, PositionedChar{[]rune{rune(raw[i])}, e.wg.Width(uint32(raw[i]))})
	}
	return r
}

type byteEncoder struct {
	f     Font
	wg    WidthGrabber
	table *[256]rune
}

func (e *byteEncoder) Decode(raw string) (text []PositionedChar) {
	r := []PositionedChar{}
	for i := 0; i < len(raw); i++ {
		r = append(r, PositionedChar{[]rune{e.table[raw[i]]}, e.wg.Width(uint32(raw[i]))})
	}
	return r
}

type cmap struct {
	f       Font
	wg      WidthGrabber
	space   [4][][2]string
	bfrange []bfrange
}

func arraydecode(utf16Strings Value) []rune {
	var utf16CodePoints []uint16
	for n := 0; n < utf16Strings.Len(); n++ {
		for i := 0; i < len(utf16Strings.Index(n).RawString()); i += 2 {
			// Assuming little-endian encoding for UTF-16
			codePoint := uint16(utf16Strings.Index(n).RawString()[i]) + uint16(utf16Strings.Index(n).RawString()[i+1])<<8
			utf16CodePoints = append(utf16CodePoints, codePoint)
		}
	}

	// Decode UTF-16 to UTF-8
	return utf16.Decode(utf16CodePoints)
}

func (m *cmap) Decode(raw string) (text []PositionedChar) {
	r := []PositionedChar{}
Parse:
	for len(raw) > 0 { //Loop through raw string
		for n := 1; n <= 4 && n <= len(raw); n++ { //Loop through codespace lengths n
			for _, space := range m.space[n-1] { //Loop through codespaces
				if space[0] <= raw[:n] && raw[:n] <= space[1] { //Check if character inside codespace
					text := raw[:n]
					raw = raw[n:]
					for _, bf := range m.bfrange { //Loop through bfranges
						if len(bf.lo) == n && bf.lo <= text && text <= bf.hi {
							if bf.dst.Kind() == String {
								s := bf.dst.RawString()
								if bf.lo != text {
									b := []byte(s)
									b[len(b)-1] += text[len(text)-1] - bf.lo[len(bf.lo)-1]
									s = string(b)
								}
								code := uint32(0)
								for _, char := range s {
									code = code << 8
									code = code + uint32(char)
								}
								//fmt.Println("FOUND", s, code, m.wg.Width(code))

								r = append(r, PositionedChar{[]rune(utf16Decode(s)), m.wg.Width(code)})
								continue Parse
							}
							if bf.dst.Kind() == Array { //TODO this code doesn't work?
								q := text[len(text)-1] - bf.lo[len(bf.lo)-1]
								//TODO: make it work with multi-byte strings
								r = append(r, PositionedChar{[]rune(utf16Decode(bf.dst.Index(int(q)).RawString())), m.wg.Width(uint32(text[len(text)-1]))})
								//}
							} else {
								fmt.Printf("unknown dst %v\n", bf.dst)
							}
							r = append(r, PositionedChar{[]rune{noRune}, 0})
							continue Parse
						}
					}
					fmt.Println("no text for %q", text)
					r = append(r, PositionedChar{[]rune{noRune}, 0})
					continue Parse
				}
			}
		}
		println("no code space found")
		r = append(r, PositionedChar{[]rune{noRune}, 0})
		raw = raw[1:]
	}
	return r
}

type bfrange struct {
	lo  string
	hi  string
	dst Value
}

func readCmap(f Font, wg WidthGrabber, toUnicode Value) *cmap {
	n := -1
	var m cmap
	m.wg = wg
	m.f = f
	ok := true
	Interpret(toUnicode, func(stk *Stack, op string) {
		if !ok {
			return
		}
		switch op {
		case "findresource":
			_ = stk.Pop()
			_ = stk.Pop()
			//fmt.Println("findresource", key, category)
			stk.Push(newDict())
		case "begincmap":
			stk.Push(newDict())
		case "endcmap":
			stk.Pop()
		case "begincodespacerange":
			n = int(stk.Pop().Int64())
		case "endcodespacerange":
			if n < 0 {
				println("missing begincodespacerange")
				ok = false
				return
			}
			for i := 0; i < n; i++ {
				hi, lo := stk.Pop().RawString(), stk.Pop().RawString()
				if len(lo) == 0 || len(lo) != len(hi) {
					println("bad codespace range")
					ok = false
					return
				}
				m.space[len(lo)-1] = append(m.space[len(lo)-1], [2]string{lo, hi})
			}
			n = -1
		case "beginbfrange":
			n = int(stk.Pop().Int64())
		case "endbfrange":
			if n < 0 {
				panic("missing beginbfrange")
			}
			for i := 0; i < n; i++ {
				dst, srcHi, srcLo := stk.Pop(), stk.Pop().RawString(), stk.Pop().RawString()
				m.bfrange = append(m.bfrange, bfrange{srcLo, srcHi, dst})
			}
		case "defineresource":
			_ = stk.Pop().Name()
			value := stk.Pop()
			_ = stk.Pop().Name()
			stk.Push(value)
		case "CMapName":
			_ = stk.Pop().Name()
		case "beginbfchar":
			n = int(stk.Pop().Int64())
		case "endbfchar":
			if n < 0 {
				panic("missing beginbfchar")
			}
			for i := 0; i < n; i++ {
				dst, srcLo := stk.Pop(), stk.Pop().RawString()
				//fmt.Println(srcLo, dst)
				m.bfrange = append(m.bfrange, bfrange{srcLo, srcLo, dst})
			}
		default:
			println("interp\t", op)
		}
	})
	if !ok {
		return nil
	}
	return &m
}
