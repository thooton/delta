package remarkable

// Drawing reMarkable pages as PDF: each page's images, its strokes as
// vector paths, its highlights, and its typed text in Helvetica.

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"
)

// Pens (the tool ids in a page file).
const (
	rmPaintbrush1   = 0
	rmPencil1       = 1
	rmBallpoint1    = 2
	rmMarker1       = 3
	rmFineliner1    = 4
	rmHighlighter1  = 5
	rmEraser        = 6
	rmMechPencil1   = 7
	rmEraserArea    = 8
	rmPaintbrush2   = 12
	rmMechPencil2   = 13
	rmPencil2       = 14
	rmBallpoint2    = 15
	rmMarker2       = 16
	rmFineliner2    = 17
	rmHighlighter2  = 18
	rmCalligraphy   = 21
	rmShader        = 23
	rmColorARGB     = 9 // newer firmware: the color is in the stroke
	rmColorDefaultH = 3 // yellow, for highlighters
)

var rmColors = map[int]color.RGBA{
	0:  {0, 0, 0, 255},       // black
	1:  {144, 144, 144, 255}, // gray
	2:  {255, 255, 255, 255}, // white
	3:  {251, 247, 25, 255},  // yellow
	4:  {0, 255, 0, 255},     // green
	5:  {255, 192, 203, 255}, // pink
	6:  {78, 105, 201, 255},  // blue
	7:  {179, 62, 57, 255},   // red
	8:  {125, 125, 125, 255}, // gray (overlap)
	10: {161, 216, 125, 255}, // green
	11: {139, 208, 229, 255}, // cyan
	12: {183, 130, 205, 255}, // magenta
	13: {247, 232, 81, 255},  // yellow
}

// rmInk is how a stroke is drawn.
type rmInk struct {
	color    color.RGBA
	alpha    float64
	multiply bool // highlighters: darken what is under them
	scale    float64
}

// inkFor says how to draw a stroke; ok is false for strokes not drawn
// (erasers).
func inkFor(s rmStroke) (rmInk, bool) {
	c, known := rmColors[s.Color]
	if s.Color == rmColorARGB && s.ARGB != 0 {
		c = color.RGBA{uint8(s.ARGB >> 16), uint8(s.ARGB >> 8), uint8(s.ARGB), 255}
		known = true
	}
	ink := rmInk{color: c, alpha: 1, scale: 1}
	switch s.Tool {
	case rmEraser, rmEraserArea:
		return ink, false
	case rmHighlighter1, rmHighlighter2:
		if !known || c == rmColors[0] {
			ink.color = rmColors[rmColorDefaultH]
		}
		ink.alpha, ink.multiply = 0.45, true
	case rmShader:
		ink.alpha, ink.multiply = 0.3, true
	case rmPencil1, rmPencil2, rmMechPencil1, rmMechPencil2:
		ink.alpha = 0.85
	}
	if !known {
		ink.color = rmColors[0]
	}
	return ink, true
}

// pointWidth is the width of the stroke at a point, in screen pixels.
func pointWidth(s rmStroke, p rmPoint) float64 {
	w := p.Width
	if w <= 0 {
		w = 2 * s.Thickness
	}
	switch s.Tool {
	case rmPencil1, rmPencil2, rmMechPencil1, rmMechPencil2:
		// Pencils draw lighter and thinner the less they are pressed.
		w *= 0.6 + 0.4*p.Pressure
	}
	return max(w, 1)
}

// rmBounds is the area a page covers, in page coordinates: at least the
// screen, and more where it was scrolled.
func rmBounds(pages ...*rmPage) (minX, minY, maxX, maxY float64) {
	minX, minY, maxX, maxY = -rmWidth/2, 0, rmWidth/2, rmHeight
	for _, p := range pages {
		if p == nil {
			continue
		}
		for _, s := range p.Strokes {
			for _, q := range s.Points {
				w := q.Width
				minX, maxX = min(minX, q.X-w), max(maxX, q.X+w)
				minY, maxY = min(minY, q.Y-w), max(maxY, q.Y+w)
			}
		}
		for _, im := range p.Images {
			for _, v := range im.Vertices {
				minX, maxX = min(minX, v[0]), max(maxX, v[0])
				minY, maxY = min(minY, v[1]), max(maxY, v[1])
			}
		}
	}
	return
}

// pdfInk is a page's images, strokes and text as PDF drawing commands,
// with the graphics states, font and images they use.
type pdfInk struct {
	ops     bytes.Buffer
	alphas  map[string]float64 // graphics state name -> opacity
	blend   map[string]bool    // graphics states that multiply
	useFont bool
	images  map[string][]byte // image name -> its file (PNG or JPEG)
}

const pdfInkFont = "DeltaF1"

// inkOps writes a page's strokes, and its typed text, as PDF drawing
// commands under the transform m, which takes page coordinates (screen
// pixels, y down) to the PDF page's.
func inkOps(p *rmPage, m [6]float64) *pdfInk {
	out := &pdfInk{alphas: map[string]float64{}, blend: map[string]bool{}, images: map[string][]byte{}}
	b := &out.ops
	b.WriteString("q\n")
	fmt.Fprintf(b, "%s %s %s %s %s %s cm\n", pdfNum(m[0]), pdfNum(m[1]), pdfNum(m[2]), pdfNum(m[3]), pdfNum(m[4]), pdfNum(m[5]))
	b.WriteString("1 J 1 j\n")
	// Images first, so what is written over them shows.
	for i, im := range p.Images {
		if _, _, err := image.DecodeConfig(bytes.NewReader(im.Data)); err != nil {
			continue // not a PNG or JPEG
		}
		name := fmt.Sprintf("%s%d", pdfImagePrefix, i+1)
		if imageOps(b, im, name) {
			out.images[name] = im.Data
		}
	}
	// Highlights that snapped to a PDF's text are rectangles, not strokes.
	for _, h := range p.Highlights {
		c, ok := rmColors[h.Color]
		if !ok || h.Color == 0 {
			c = rmColors[rmColorDefaultH]
		}
		out.alphas["DeltaGS45m"], out.blend["DeltaGS45m"] = 0.45, true
		fmt.Fprintf(b, "q /DeltaGS45m gs %s %s %s rg\n", pdfNum(float64(c.R)/255), pdfNum(float64(c.G)/255), pdfNum(float64(c.B)/255))
		for _, r := range h.Rects {
			fmt.Fprintf(b, "%s %s %s %s re\n", pdfNum(r[0]), pdfNum(r[1]), pdfNum(r[2]), pdfNum(r[3]))
		}
		b.WriteString("f Q\n")
	}
	for _, s := range p.Strokes {
		ink, ok := inkFor(s)
		if !ok {
			continue
		}
		b.WriteString("q\n")
		if ink.alpha < 1 || ink.multiply {
			name := fmt.Sprintf("DeltaGS%d", int(math.Round(ink.alpha*100)))
			if ink.multiply {
				name += "m"
			}
			out.alphas[name] = ink.alpha
			out.blend[name] = ink.multiply
			fmt.Fprintf(b, "/%s gs\n", name)
		}
		fmt.Fprintf(b, "%s %s %s RG\n", pdfNum(float64(ink.color.R)/255), pdfNum(float64(ink.color.G)/255), pdfNum(float64(ink.color.B)/255))
		// One path per run of points of about the same width.
		pts := s.Points
		if len(pts) == 1 {
			pts = append(pts, pts[0])
		}
		width := -1.0
		open := false
		for i := 1; i < len(pts); i++ {
			w := (pointWidth(s, pts[i-1]) + pointWidth(s, pts[i])) / 2
			if !open || math.Abs(w-width) > 0.15*width {
				if open {
					b.WriteString("S\n")
				}
				width = w
				fmt.Fprintf(b, "%s w %s %s m ", pdfNum(w), pdfNum(pts[i-1].X), pdfNum(pts[i-1].Y))
				open = true
			}
			fmt.Fprintf(b, "%s %s l ", pdfNum(pts[i].X), pdfNum(pts[i].Y))
		}
		if open {
			b.WriteString("S\n")
		}
		b.WriteString("Q\n")
	}
	if p.Text != "" {
		out.useFont = true
		textOps(b, p)
	}
	b.WriteString("Q\n")
	return out
}

const pdfImagePrefix = "DeltaIm"

// imageOps draws an image, clipped to its mesh, under the transform its
// mesh gives it: the one taking the image's u and v to page positions,
// worked out from the corners of its first triangle. (A mesh the tablet
// warped into something no transform makes comes out flat.) It is false
// for a mesh too thin to draw on.
func imageOps(b *bytes.Buffer, im rmImage, name string) bool {
	v0, v1, v2 := im.Vertices[im.Indices[0]], im.Vertices[im.Indices[1]], im.Vertices[im.Indices[2]]
	du1, dv1, du2, dv2 := v1[2]-v0[2], v1[3]-v0[3], v2[2]-v0[2], v2[3]-v0[3]
	det := du1*dv2 - du2*dv1
	if math.Abs(det) < 1e-9 {
		return false
	}
	// Page offset per unit of u (ux, uy) and of v (vx, vy).
	dx1, dy1, dx2, dy2 := v1[0]-v0[0], v1[1]-v0[1], v2[0]-v0[0], v2[1]-v0[1]
	ux, uy := (dx1*dv2-dx2*dv1)/det, (dy1*dv2-dy2*dv1)/det
	vx, vy := (dx2*du1-dx1*du2)/det, (dy2*du1-dy1*du2)/det
	// Where u = v = 0, the image's top left corner, is.
	ox, oy := v0[0]-ux*v0[2]-vx*v0[3], v0[1]-uy*v0[2]-vy*v0[3]
	b.WriteString("q\n")
	for i := 0; i+2 < len(im.Indices); i += 3 {
		a, c, d := im.Vertices[im.Indices[i]], im.Vertices[im.Indices[i+1]], im.Vertices[im.Indices[i+2]]
		fmt.Fprintf(b, "%s %s m %s %s l %s %s l h\n", pdfNum(a[0]), pdfNum(a[1]), pdfNum(c[0]), pdfNum(c[1]), pdfNum(d[0]), pdfNum(d[1]))
	}
	b.WriteString("W n\n")
	// A PDF image fills the unit square from its bottom left, so v runs
	// from the top: (s, t) is u = s, v = 1 - t.
	fmt.Fprintf(b, "%s %s %s %s %s %s cm /%s Do Q\n", pdfNum(ux), pdfNum(uy), pdfNum(-vx), pdfNum(-vy), pdfNum(ox+vx), pdfNum(oy+vy), name)
	return true
}

// textOps writes typed text where the tablet shows it, in Helvetica, at
// the line heights used to place strokes anchored to it (see orderText).
// Lines wrap at the text's width.
func textOps(b *bytes.Buffer, p *rmPage) {
	x, y, width := p.TextX, p.TextY, p.TextWidth
	if width <= 0 {
		x, y, width = -468, 234, 936
	}
	for _, para := range strings.Split(p.Text, "\n") {
		size, height := 32.0, 71.0
		if t, ok := strings.CutPrefix(para, "# "); ok {
			para, size, height = t, 52, 150
		} else if strings.HasPrefix(para, "- ") || strings.HasPrefix(para, "  - ") || strings.HasPrefix(para, "[") {
			height = 71
		}
		for i, line := range wrapText(para, width, size) {
			if i > 0 {
				y += size * 1.3
			}
			if line != "" {
				// The page's y axis points down, so the text is flipped back.
				fmt.Fprintf(b, "BT /%s %s Tf 1 0 0 -1 %s %s Tm (%s) Tj ET\n", pdfInkFont, pdfNum(size), pdfNum(x), pdfNum(y+size), pdfWinAnsi(line))
			}
		}
		y += height
	}
}

// wrapText breaks a paragraph into lines no wider than width at this font
// size, measuring with Helvetica's average character width.
func wrapText(s string, width, size float64) []string {
	maxChars := max(int(width/(size*0.5)), 10)
	var lines []string
	for len([]rune(s)) > maxChars {
		r := []rune(s)
		cut := maxChars
		for i := maxChars; i > maxChars/2; i-- {
			if r[i] == ' ' {
				cut = i
				break
			}
		}
		lines = append(lines, string(r[:cut]))
		s = strings.TrimLeft(string(r[cut:]), " ")
	}
	return append(lines, s)
}

// pdfWinAnsi escapes text for a PDF string in WinAnsiEncoding, the
// standard fonts' encoding; characters outside it become "?".
func pdfWinAnsi(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '(' || r == ')' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r >= 32 && r < 127:
			b.WriteRune(r)
		case r >= 160 && r <= 255:
			fmt.Fprintf(&b, "\\%03o", r)
		default:
			if c, ok := winAnsiExtra[r]; ok {
				fmt.Fprintf(&b, "\\%03o", c)
			} else {
				b.WriteByte('?')
			}
		}
	}
	return b.String()
}

var winAnsiExtra = map[rune]byte{
	'€': 0x80, '‚': 0x82, 'ƒ': 0x83, '„': 0x84, '…': 0x85, '†': 0x86, '‡': 0x87,
	'ˆ': 0x88, '‰': 0x89, 'Š': 0x8A, '‹': 0x8B, 'Œ': 0x8C, 'Ž': 0x8E, '‘': 0x91,
	'’': 0x92, '“': 0x93, '”': 0x94, '•': 0x95, '–': 0x96, '—': 0x97, '˜': 0x98,
	'™': 0x99, 'š': 0x9A, '›': 0x9B, 'œ': 0x9C, 'ž': 0x9E, 'Ÿ': 0x9F,
}

// notebookPage is how a page of a notebook, or one added to a PDF, comes
// out: s points per screen pixel, as tall and wide as its strokes need
// (at least the screen). A notebook's pages are the tablet's real size.
func notebookPage(p *rmPage, s float64) outPage {
	minX, minY, maxX, maxY := rmBounds(p)
	w, h := (maxX-minX)*s, (maxY-minY)*s
	return outPage{width: w, height: h, ink: inkOps(p, [6]float64{s, 0, 0, -s, -minX * s, h + minY*s})}
}

// pointsPerPixel is the tablet's screen resolution, 226 dpi.
const pointsPerPixel = 72.0 / 226

// pdfNum writes a number briefly, as PDF wants it (no exponents).
func pdfNum(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "0"
	}
	s := fmt.Sprintf("%.3f", f)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "-0" || s == "" {
		return "0"
	}
	return s
}
