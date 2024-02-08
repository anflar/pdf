// Copyright 2014 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pdf

import (
	"fmt"
	"math"
	"strings"
)

// A Page represent a single page in a PDF file.
// The methods interpret a Page dictionary stored in V.
type Page struct {
	V         Value
	fontcache map[string]Font
}



func (r *Reader) Page(num int) Page {
	num-- // now 0-indexed
	page := r.Trailer.Key("Root").Key("Pages")
    if page.err != nil{
        return Page {}
    }

    if page.Key("Type").CoerceString("") != "Pages"{
        return Page {}
    }

    //TODO: make this function recursive 
}
// Page returns the page for the given page number.
// Page numbers are indexed starting at 1, not 0.
// If the page is not found, Page returns a Page with p.V.IsNull().
func (r *Reader) Page_OLD(num int) Page {
	num-- // now 0-indexed
	page := r.Trailer.Key("Root").Key("Pages")
    if page.err != nil{
        return Page {}
    }
Search:
	for {
        if page.Key("Type").CoerceString("") != "Pages"{
            break
        }
		count := page.Key("Count").CoerceInt64(-1)
		if count < num {
			return Page{}
		}
		kids := page.Key("Kids")
        if kids.err != nil {
            return Page{}
        }
		for i := 0; i < kids.Len(); i++ {
			kid := kids.Index(i)
            if kid.err != nil {
               return Page{} 
            }
        
			if kid.Key("Type").Name() == "Pages" {
				c := int(kid.Key("Count").Int64())
				if num < c {
					page = kid
					continue Search
				}
				num -= c
				continue
			}
			if kid.Key("Type").Name() == "Page" {
				if num == 0 {
					return Page{kid, map[string]Font{}}
				}
				num--
			}
		}
		break
	}
	return Page{}
}

// NumPage returns the number of pages in the PDF file.
func (r *Reader) NumPage() int {
    num, _ := r.Trailer().Int("Root", "Pages", "Count")
	return num
}

func (p Page) findInherited(key string) (Value, error) {
	for v := p.V; v.Kind() != Null; v, _ = v.Key("Parent") {
        r, err := v.Key(key)
	    if err != nil {
            return Value{}, err
        }
        return r, nil
	}
	return Value{}, nil
}

func (p Page) MediaBox() Value {
	return p.findInherited("MediaBox")
}

func (p Page) CropBox() Value {
	return p.findInherited("CropBox")
}

// Resources returns the resources dictionary associated with the page.
func (p Page) Resources() Value {
	return p.findInherited("Resources")
}

// Fonts returns a list of the fonts associated with the page.
/*func (p Page) Fonts() []string {
	return p.Resources().Key("Font").Keys()
}*/

// Font returns the font with the given name associated with the page.
func (p Page) Font(name string) Font {

	var f Font
	f, ok := p.fontcache[name]
	if !ok {
		f = FontFromValue(p.Resources().Key("Font").Key(name))
		p.fontcache[name] = f
	}
	return f
}

type matrix [3][3]float64

var ident = matrix{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}

func (x matrix) mul(y matrix) matrix {
	var z matrix
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			for k := 0; k < 3; k++ {
				z[i][j] += x[i][k] * y[k][j]
			}
		}
	}
	return z
}

// A Text represents a single piece of text drawn on a page.
type Text struct {
	Font          string  // the font used
	FontSize      float64 // the font size, in points (1/72 of an inch)
	RotationAngle float64 //degrees
	FontWeight    float64
	X             float64          // the X coordinate, in points, increasing left to right
	Y             float64          // the Y coordinate, in points, increasing bottom to top
	W             float64          // the width of the text, in points
	S             []PositionedChar // the actual UTF-8 text
}

type Path struct {
	Kind      string
	Points    []Point
	EndPoint Point
	JoinStyle int
	CapStyle  int
	LineWidth float64
}

// A Point represents an X, Y pair.
type Point struct {
	X float64
	Y float64
}

// Content describes the basic content on a page: the text and any drawn rectangles.
type Content struct {
	Text []Text
	//Rect []Rect
	Paths []Path
}

type gstate struct {
	Tc        float64
	Tw        float64
	Th        float64
	Tl        float64
	Tf        Font
	Tfs       float64
	Tmode     int
	Trise     float64
	Tm        matrix
	Tlm       matrix
	Trm       matrix
	CTM       matrix
	Px        float64
	Py        float64
	JoinStyle int
	CapStyle  int
	LineWidth float64
}

// Content returns the page's content.
func (p Page) Content() Content {
	var text []Text

	var g = gstate{
		Th:  1,
		CTM: ident,
	}

	var paths []Path
	var gstack []gstate
	var streams []Value

	if p.V.Key("Contents").Kind() == Array {
		for i := 0; i < p.V.Key("Contents").Len(); i++ {
			streams = append(streams, p.V.Key("Contents").Index(i))
		}
	} else if p.V.Key("Contents").Kind() == Stream {
		streams = append(streams, p.V.Key("Contents"))
	}

	// Estimate amount of paths based on heuristic
	sl := int64(0)
	for i := 0; i < len(streams); i++ {
		sl += streams[len(streams)-1].Key("Length").CoerceInt64(0)
	}

	paths = make([]Path, sl/10)
	text = make([]Text, sl/100)

	for i := 0; i < len(streams); i++ {
		strm := streams[i]

		showText := func(s string) {
			//if g.Tf.V.Key("Name").Kind() == 0 {
			//	fmt.Println(g)
			//}
			decoded := g.Tf.Decode(s)

			for _, ch := range decoded {
				if string(ch.Text) != " " {
					break
				}
				w0 := ch.Width / 1000
				if w0 < 0.05 {
					//fmt.Println("Fonth width small?", w0, "\t", string(ch.Text), "\t", decoded)
				}
				//fmt.Println(ch.Length())
				tx := (w0*g.Tfs + g.Tc) * g.Th
				if string(ch.Text) == string(" ") {
					tx += g.Tw * g.Th
				}
				tx = tx * g.Th
				g.Tm = matrix{{1, 0, 0}, {0, 1, 0}, {tx, 0, 1}}.mul(g.Tm)
			}

			Trm := matrix{{g.Tfs * g.Th, 0, 0}, {0, g.Tfs, 0}, {0, g.Trise, 1}}.mul(g.Tm).mul(g.CTM)

			f := g.Tf.BaseFont()
			if i := strings.Index(f, "+"); i >= 0 {
				f = f[i+1:]
			}

			fw := g.Tf.FontWeight()

			fontsize := math.Sqrt(Trm[0][0]*Trm[0][0] + Trm[1][0]*Trm[1][0])
			rotationAngle := math.Atan2(Trm[1][0], Trm[0][0]) * 180 / math.Pi

			text = append(text, Text{f, fontsize, rotationAngle, fw, Trm[2][0], Trm[2][1], Trm[0][0], decoded})

			skip := true
			for _, ch := range decoded {
				if skip && string(ch.Text) == " " {
					continue
				} else {
					skip = false
				}
				w0 := ch.Width
				tx := w0/1000*g.Tfs + g.Tc
				for _, ch3 := range string(ch.Text) {
					if string(ch3) == " " {
						tx += g.Tw
					}
				}
				tx *= g.Th
				ty := 0.0
				g.Tm = matrix{{1, 0, 0}, {0, 1, 0}, {tx, ty, 1}}.mul(g.Tm)
			}

		}

		Interpret(strm, func(stk *Stack, op string) {
			var x, y, w, h float64
			var x1, x2, x3, x4, y1, y2, y3, y4 float64
			n := stk.Len()
			args := make([]Value, n)
			for i := n - 1; i >= 0; i-- {
				args[i] = stk.Pop()
			}
			//if true {
			//	fmt.Println(strings.Repeat("--", len(gstack)), op, "", args, "     ", g.CTM)
			//}

			switch op {
			default:
				fmt.Println(op, args)
				panic("bad g.Tm")
			case "y":
				fallthrough
			case "v":
				g.Px, g.Py = args[2].CoerceFloat64(0), args[3].CoerceFloat64(0)
			case "c":
				x1, y1, x2, y2, x3, y3, x4, y4 = g.Px, g.Py, args[0].Float64(), args[1].Float64(), args[2].Float64(), args[3].Float64(), args[4].Float64(), args[5].Float64()
				g.Px, g.Py = x4, y4

				loc1 := matrix{{1, 0, 0}, {0, 1, 0}, {x1, y1, 1}}.mul(g.CTM)
				loc2 := matrix{{1, 0, 0}, {0, 1, 0}, {x2, y2, 1}}.mul(g.CTM)
				loc3 := matrix{{1, 0, 0}, {0, 1, 0}, {x3, y3, 1}}.mul(g.CTM)
				loc4 := matrix{{1, 0, 0}, {0, 1, 0}, {x4, y4, 1}}.mul(g.CTM)

				pt1 := Point{loc1[2][0], loc1[2][1]}
				pt2 := Point{loc2[2][0], loc2[2][1]}
				pt3 := Point{loc3[2][0], loc3[2][1]}
				pt4 := Point{loc4[2][0], loc4[2][1]}

				lw := math.Sqrt(g.CTM[0][0]*g.CTM[0][0] + g.CTM[1][0]*g.CTM[1][0])
				paths = append(paths, Path{"bezier", []Point{pt1, pt2, pt3, pt4}, pt4, g.JoinStyle, g.CapStyle, lw * g.LineWidth})

			case "cm": // update g.CTM
				if len(args) != 6 {
					panic("bad g.Tm")
				}
				var m matrix
				for i := 0; i < 6; i++ {
					m[i/2][i%2] = args[i].Float64()
				}
				m[2][2] = 1
				g.CTM = m.mul(g.CTM)
			case "gs": // set parameters from graphics state resource
				gs := p.Resources().Key("ExtGState").Key(args[0].Name())
				font := gs.Key("Font")
				if font.Kind() == Array && font.Len() == 2 {
					//fmt.Println("FONT", font)
				}
			case "l": // lineto
				x, y = g.Px, g.Py
				g.Px, g.Py = args[0].Float64(), args[1].Float64()
				loc1 := matrix{{1, 0, 0}, {0, 1, 0}, {x, y, 1}}.mul(g.CTM)
				loc2 := matrix{{1, 0, 0}, {0, 1, 0}, {g.Px, g.Py, 1}}.mul(g.CTM)

				pt1 := Point{loc1[2][0], loc1[2][1]}
				pt2 := Point{loc2[2][0], loc2[2][1]}

				lw := math.Sqrt(g.CTM[0][0]*g.CTM[0][0] + g.CTM[1][0]*g.CTM[1][0])
				paths = append(paths, Path{"line", []Point{pt1, pt2}, pt2, g.JoinStyle, g.CapStyle, lw * g.LineWidth})

			case "m": // moveto
				g.Px, g.Py = args[0].Float64(), args[1].Float64()

			case "re": // append rectangle to path
				if len(args) != 4 {
					panic("bad re")
				}
				x, y, w, h = args[0].Float64(), args[1].Float64(), args[2].Float64(), args[3].Float64()
				lw := math.Sqrt(g.CTM[0][0]*g.CTM[0][0] + g.CTM[1][0]*g.CTM[1][0])
				paths = append(paths, Path{"rect", []Point{{x, y}, {x + w, y + h}}, Point{x, y}, g.JoinStyle, g.CapStyle, lw * g.LineWidth})

			case "q": // save graphics state
				gstack = append(gstack, g)

			case "Q": // restore graphics state
				n := len(gstack) - 1
				g = gstack[n]
				gstack = gstack[:n]

			case "BT": // begin text (reset text matrix and line matrix)
				g.Tm = ident
				g.Tlm = g.Tm
			case "ET": // end text

			case "T*": // move to start of next line
				x := matrix{{1, 0, 0}, {0, 1, 0}, {0, -g.Tl, 1}}
				g.Tlm = x.mul(g.Tlm)
				g.Tm = g.Tlm

			case "Tc": // set character spacing
				if len(args) != 1 {
					panic("bad g.Tc")
				}
				g.Tc = args[0].Float64()

			case "TD": // move text position and set leading
				if len(args) != 2 {
					panic("bad Td")
				}
				g.Tl = -args[1].Float64()

				fallthrough
			case "Td": // move text position
				if len(args) != 2 {
					panic("bad Td")
				}
				tx := args[0].Float64()
				ty := args[1].Float64()
				x := matrix{{1, 0, 0}, {0, 1, 0}, {tx, ty, 1}}
				g.Tlm = x.mul(g.Tlm)
				g.Tm = g.Tlm

			case "Tf": // set text font and size
				if len(args) != 2 {
					panic("bad TL")
				}
				f := args[0].Name()
				g.Tf = p.Font(f)
				g.Tfs = args[1].Float64()

			case "\"": // set spacing, move to next line, and show text
				if len(args) != 3 {
					panic("bad \" operator")
				}
				g.Tw = args[0].Float64()
				g.Tc = args[1].Float64()
				args = args[2:]
				fallthrough
			case "'": // move to next line and show text
				if len(args) != 1 {
					panic("bad ' operator")
				}
				x := matrix{{1, 0, 0}, {0, 1, 0}, {0, -g.Tl, 1}}
				g.Tlm = x.mul(g.Tlm)
				g.Tm = g.Tlm
				fallthrough
			case "Tj": // show text
				if len(args) != 1 {
					panic("bad Tj operator")
				}
				showText(args[0].RawString())

			case "TJ": // show text, allowing individual glyph positioning
				v := args[0]
				var tx float64
				var rs string
				w0 := 0.0
				for i := 0; i < v.Len(); i++ {
					x := v.Index(i)
					if x.Kind() == String {
						rs = x.RawString()
						showText(rs)
						w0 = 0.0
						//for _, runeValue := range rs {
						//	//fmt.Println("waaaa", string(runeValue), int(runeValue))
						//	w0 += g.Tf.Width(int(runeValue)) / 1000
						//}

						strs := g.Tf.Decode(rs)
						for _, s := range strs {
							fmt.Print(string(s.Text))
							//fmt.Print(s.Width)
							/*for _, ch3 := range string(s.Text) {
								if string(ch3) == " " {
									w0 += g.Tw
								}
							}*/
							//w0 += s.Width / 1000
							//fmt.Println(s.Width)
						}

					} else {
						tx = (w0 - x.Float64()/1000 + g.Tc) * g.Tfs * g.Th
						g.Tm = matrix{{1, 0, 0}, {0, 1, 0}, {tx, 0, 1}}.mul(g.Tm)
					}
				}

			case "TL": // set text leading
				if len(args) != 1 {
					panic("bad TL")
				}
				g.Tl = args[0].Float64()

			case "Tm": // set text matrix and line matrix
				if len(args) != 6 {
					panic("bad g.Tm")
				}
				var m matrix
				for i := 0; i < 6; i++ {
					m[i/2][i%2] = args[i].Float64()
				}
				m[2][2] = 1
				g.Tm = m
				g.Tlm = m

			case "Tr": // set text rendering mode
				if len(args) != 1 {
					panic("bad Tr")
				}
				g.Tmode = int(args[0].Int64())

			case "Ts": // set text rise
				if len(args) != 1 {
					panic("bad Ts")
				}
				g.Trise = args[0].Float64()

			case "Tw": // set word spacing
				if len(args) != 1 {
					panic("bad g.Tw")
				}
				g.Tw = args[0].Float64()

			case "Tz": // set horizontal text scaling
				if len(args) != 1 {
					panic("bad Tz")
				}
				g.Th = args[0].Float64() / 100
			case "W": // Set clipping path
			case "Do": //?
			case "W*": //?
			case "f*": //?
			case "": //something went wrong
			case "d": //?
			case "w": // Set line width
				g.LineWidth = args[0].Float64()
			case "j": // Set line join style
				g.JoinStyle = int(args[0].Int64())
			case "J": // Set line cap style
				g.CapStyle = int(args[0].Int64())
			case "n": //end path
			case "RG": //Set RGB color
			case "S": //stroke path
			case "rg": //Set RGB color
			case "M": //set miter limit
			case "h": //close path
			case "b": //close fill stroke path
			case "cs": // set colorspace non-stroking
			case "scn": // set color non-stroking
			case "f": // fill
			case "g": // setgray
			case "G": //?
			case "CS": //set color space
			case "BMC": //
			case "BDC": //marked content sequence
			case "EMC": //end marked content
			case "i": //??
			case "s": //??
			}
		})
	}
	return Content{text, paths}
}

// TextVertical implements sort.Interface for sorting
// a slice of Text values in vertical order, top to bottom,
// and then left to right within a line.
type TextVertical []Text

func (x TextVertical) Len() int      { return len(x) }
func (x TextVertical) Swap(i, j int) { x[i], x[j] = x[j], x[i] }
func (x TextVertical) Less(i, j int) bool {
	if x[i].Y != x[j].Y {
		return x[i].Y > x[j].Y
	}
	return x[i].X < x[j].X
}

// TextHorizontal implements sort.Interface for sorting
// a slice of Text values in horizontal order, left to right,
// and then top to bottom within a column.
type TextHorizontal []Text

func (x TextHorizontal) Len() int      { return len(x) }
func (x TextHorizontal) Swap(i, j int) { x[i], x[j] = x[j], x[i] }
func (x TextHorizontal) Less(i, j int) bool {
	if x[i].X != x[j].X {
		return x[i].X < x[j].X
	}
	return x[i].Y > x[j].Y
}

// An Outline is a tree describing the outline (also known as the table of contents)
// of a document.
type Outline struct {
	Title string    // title for this element
	Child []Outline // child elements
}

// Outline returns the document outline.
// The Outline returned is the root of the outline tree and typically has no Title itself.
// That is, the children of the returned root are the top-level entries in the outline.
func (r *Reader) Outline() Outline {
	return buildOutline(r.Trailer().Key("Root").Key("Outlines"))
}

func buildOutline(entry Value) Outline {
	var x Outline
	x.Title = entry.Key("Title").Text()
	for child := entry.Key("First"); child.Kind() == Dict; child = child.Key("Next") {
		x.Child = append(x.Child, buildOutline(child))
	}
	return x
}
