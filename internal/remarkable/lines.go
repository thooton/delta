package remarkable

// Reading the reMarkable's page files (<page-id>.rm), version 6, the format
// of firmware 3.x. A page is a list of blocks; the ones read here are the
// strokes (scene line items), typed text (the root text block), highlights
// on PDFs and EPUBs (scene glyph items), images pasted onto the page (scene
// image items, and the table naming their files) and the layers (tree
// nodes), whose visibility decides whether their strokes and images are
// drawn. The format is
// reverse-engineered; this follows rmscene (github.com/ricklupton/rmscene).
//
// Inside a block, values are tagged: a varuint whose low 4 bits say the
// value's kind and the rest its index within the block. A kind of
// tagSub is a length-prefixed subblock, which may hold more tagged values.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

const rmHeaderV6 = "reMarkable .lines file, version=6"

// The page's size in screen pixels. x runs from -rmWidth/2 to rmWidth/2,
// centered on the page; y from 0 at the top down. Notebook pages that were
// scrolled past the screen go further.
const (
	rmWidth  = 1404
	rmHeight = 1872
)

type rmID struct {
	a uint8
	b uint64
}

func (id rmID) less(o rmID) bool { return id.a < o.a || id.a == o.a && id.b < o.b }

type rmPoint struct {
	X, Y      float64
	Speed     float64
	Width     float64 // in screen pixels
	Direction float64
	Pressure  float64 // 0 to 1
}

type rmStroke struct {
	Layer     rmID
	Tool      int
	Color     int
	ARGB      uint32 // set when Color is rmColorARGB
	Thickness float64
	Points    []rmPoint
}

type rmHighlight struct {
	Color int
	Text  string
	Rects [][4]float64 // x, y, w, h
}

// rmImage is an image pasted onto the page: a mesh of triangles, each
// corner a page position and the point of the image (u and v, from 0 to 1,
// v down) drawn there. The image itself is a file beside the page file,
// <doc>/<page>/<File>.
type rmImage struct {
	Layer    rmID
	ref      [16]byte // its key in the page's image file table
	File     string
	Data     []byte       // the file, filled in by render
	Vertices [][4]float64 // x, y, u, v
	Indices  []int        // three per triangle
}

type rmPage struct {
	Strokes    []rmStroke
	Text       string // typed text, paragraphs separated by newlines
	TextX      float64
	TextY      float64
	TextWidth  float64
	Highlights []rmHighlight
	Images     []rmImage
}

type rmNode struct {
	parent  rmID
	hidden  bool
	anchor  *rmID
	anchorX float64
}

// place moves the strokes and images of anchored groups to where their
// text is, and drops those of hidden layers.
func (p *rmPage) place(nodes map[rmID]*rmNode, anchors map[rmID]float64) {
	// where is a layer's offset, and whether it is hidden.
	where := func(id rmID) (dx, dy float64, hidden bool) {
		for depth := 0; depth < 32; depth++ {
			n := nodes[id]
			if n == nil {
				break
			}
			hidden = hidden || n.hidden
			if n.anchor != nil {
				dx += n.anchorX
				dy += anchors[*n.anchor]
			}
			if n.parent == id || n.parent == (rmID{}) {
				break
			}
			id = n.parent
		}
		return
	}
	kept := p.Strokes[:0]
	for _, s := range p.Strokes {
		dx, dy, hidden := where(s.Layer)
		if hidden {
			continue
		}
		if dx != 0 || dy != 0 {
			for i := range s.Points {
				s.Points[i].X += dx
				s.Points[i].Y += dy
			}
		}
		kept = append(kept, s)
	}
	p.Strokes = kept
	images := p.Images[:0]
	for _, im := range p.Images {
		dx, dy, hidden := where(im.Layer)
		if hidden {
			continue
		}
		for i := range im.Vertices {
			im.Vertices[i][0] += dx
			im.Vertices[i][1] += dy
		}
		images = append(images, im)
	}
	p.Images = images
}

// written is true when there is something on the page to draw.
func (p *rmPage) written() bool {
	return len(p.Strokes) > 0 || p.Text != "" || len(p.Highlights) > 0 || len(p.Images) > 0
}

const (
	tagID    = 0xF
	tagSub   = 0xC
	tagByte8 = 0x8
	tagByte4 = 0x4
	tagByte1 = 0x1
)

type rmReader struct {
	b   []byte
	off int
	err error
}

func (r *rmReader) left() int { return len(r.b) - r.off }

func (r *rmReader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf(format, args...)
	}
	r.off = len(r.b)
}

func (r *rmReader) bytes(n int) []byte {
	if n < 0 || r.left() < n {
		r.fail("page file is cut short")
		return make([]byte, n&0xffff)
	}
	b := r.b[r.off : r.off+n]
	r.off += n
	return b
}

func (r *rmReader) u8() uint8   { return r.bytes(1)[0] }
func (r *rmReader) u16() uint16 { return binary.LittleEndian.Uint16(r.bytes(2)) }
func (r *rmReader) u32() uint32 { return binary.LittleEndian.Uint32(r.bytes(4)) }
func (r *rmReader) f32() float64 {
	return float64(math.Float32frombits(binary.LittleEndian.Uint32(r.bytes(4))))
}
func (r *rmReader) f64() float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(r.bytes(8)))
}

func (r *rmReader) varuint() uint64 {
	var v uint64
	for shift := 0; shift < 64; shift += 7 {
		c := r.u8()
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v
		}
	}
	r.fail("bad number in page file")
	return 0
}

func (r *rmReader) id() rmID { return rmID{r.u8(), r.varuint()} }

// peekTag returns the next tag without reading it; ok is false at the end.
func (r *rmReader) peekTag() (index int, kind int, ok bool) {
	if r.left() == 0 || r.err != nil {
		return 0, 0, false
	}
	save := r.off
	v := r.varuint()
	r.off = save
	return int(v >> 4), int(v & 0xF), r.err == nil
}

func (r *rmReader) has(index, kind int) bool {
	i, k, ok := r.peekTag()
	return ok && i == index && k == kind
}

func (r *rmReader) tag(index, kind int) {
	i, k, ok := r.peekTag()
	if !ok || i != index || k != kind {
		r.fail("unexpected value in page file (wanted %d/%x, found %d/%x)", index, kind, i, k)
		return
	}
	r.varuint()
}

func (r *rmReader) tagID(i int) rmID     { r.tag(i, tagID); return r.id() }
func (r *rmReader) tagU32(i int) uint32  { r.tag(i, tagByte4); return r.u32() }
func (r *rmReader) tagF32(i int) float64 { r.tag(i, tagByte4); return r.f32() }
func (r *rmReader) tagF64(i int) float64 { r.tag(i, tagByte8); return r.f64() }
func (r *rmReader) tagBool(i int) bool   { r.tag(i, tagByte1); return r.u8() != 0 }
func (r *rmReader) sub(i int) *rmReader  { r.tag(i, tagSub); return &rmReader{b: r.bytes(int(r.u32()))} }
func (r *rmReader) optSub(i int) *rmReader {
	if !r.has(i, tagSub) {
		return nil
	}
	return r.sub(i)
}

// skip reads past one tagged value of any kind.
func (r *rmReader) skip() {
	_, k, ok := r.peekTag()
	if !ok {
		return
	}
	r.varuint()
	switch k {
	case tagID:
		r.id()
	case tagSub:
		r.bytes(int(r.u32()))
	case tagByte8:
		r.bytes(8)
	case tagByte4:
		r.bytes(4)
	case tagByte1:
		r.bytes(1)
	default:
		r.fail("unknown value kind %x in page file", k)
	}
}

// find reads past values until the one with this index and kind, if any.
func (r *rmReader) find(index, kind int) bool {
	for {
		i, k, ok := r.peekTag()
		if !ok {
			return false
		}
		if i == index && k == kind {
			return true
		}
		r.skip()
	}
}

// rmString reads a string subblock: its length, whether it is ASCII, and
// its bytes.
func (r *rmReader) rmString() string {
	n := r.varuint()
	r.u8()
	return string(r.bytes(int(n)))
}

type rmTextItem struct {
	id, left, right rmID
	deleted         int
	text            string
	marker          bool
}

// parseRM reads a page file. Blocks it doesn't know are skipped, and so is
// one it can't read, so a page from newer firmware still shows what can be
// read of it.
func parseRM(data []byte) (*rmPage, error) {
	if !bytes.HasPrefix(data, []byte(rmHeaderV6)) {
		if bytes.HasPrefix(data, []byte("reMarkable .lines file, version=")) {
			v, _, _ := strings.Cut(strings.TrimPrefix(string(data[:min(len(data), 43)]), "reMarkable .lines file, version="), " ")
			return nil, fmt.Errorf("this page is in an older format (version %s) that delta can't read", strings.TrimSpace(v))
		}
		return nil, errors.New("not a reMarkable page file")
	}
	page := &rmPage{}
	nodes := map[rmID]*rmNode{}
	node := func(id rmID) *rmNode {
		if nodes[id] == nil {
			nodes[id] = &rmNode{}
		}
		return nodes[id]
	}
	r := &rmReader{b: data, off: 43}
	var textItems []rmTextItem
	var styles map[rmID]int
	imageFiles := map[[16]byte]string{}
	var firstErr error
	for r.left() >= 8 {
		length := int(r.u32())
		r.u8() // unknown
		r.u8() // minimum version to read it
		version := r.u8()
		kind := r.u8()
		body := r.bytes(length)
		if r.err != nil {
			break
		}
		b := &rmReader{b: body}
		switch kind {
		case 0x01: // where a node sits in the tree
			id := b.tagID(1)
			b.tagID(2)
			b.tagBool(3)
			if sb := b.optSub(4); sb != nil {
				node(id).parent = sb.tagID(1)
			}
		case 0x02: // tree node: a layer, or a group of strokes
			n := node(b.tagID(1))
			b.optSub(2) // label
			if sb := b.optSub(3); sb != nil {
				sb.tagID(1)
				n.hidden = !sb.tagBool(2) && sb.err == nil
			}
			// Strokes written next to typed text move with it: their group
			// is anchored to a character, and its offset is that
			// character's line.
			if b.find(7, tagSub) {
				sb := b.sub(7)
				sb.tagID(1)
				a := sb.tagID(2)
				if sb.err == nil {
					n.anchor = &a
				}
			}
			if b.find(10, tagSub) {
				sb := b.sub(10)
				sb.tagID(1)
				n.anchorX = sb.tagF32(2)
			}
		case 0x04: // a group within a layer
			parent, v := sceneItem(b)
			if v != nil {
				if child := v.tagID(2); v.err == nil {
					node(child).parent = parent
				}
			}
		case 0x05: // a stroke
			if s, ok := parseRMStroke(b, version); ok {
				page.Strokes = append(page.Strokes, s)
			}
		case 0x03: // a highlight
			if h, ok := parseRMGlyph(b); ok {
				page.Highlights = append(page.Highlights, h)
			}
		case 0x07: // typed text
			textItems, styles = parseRMText(b, page)
		case 0x0E: // the names of the page's image files
			parseRMImageFiles(b, imageFiles)
		case 0x0F: // an image
			if im, ok := parseRMImage(b); ok {
				page.Images = append(page.Images, im)
			}
		}
		if b.err != nil && firstErr == nil {
			firstErr = b.err
		}
	}
	images := page.Images[:0]
	for _, im := range page.Images {
		if im.File = imageFiles[im.ref]; im.File != "" {
			images = append(images, im)
		}
	}
	page.Images = images
	var anchors map[rmID]float64
	page.Text, anchors = orderText(textItems, styles, page.TextY)
	page.place(nodes, anchors)
	if !page.written() && firstErr != nil {
		return page, firstErr
	}
	return page, nil
}

// sceneItem reads the start of a scene item block, shared by strokes,
// highlights and groups: its ids, and the subblock holding its value, or
// nil for an item that was deleted.
func sceneItem(b *rmReader) (parent rmID, value *rmReader) {
	parent = b.tagID(1)
	b.tagID(2)  // item
	b.tagID(3)  // left
	b.tagID(4)  // right
	b.tagU32(5) // deleted length
	if !b.has(6, tagSub) {
		return parent, nil
	}
	v := b.sub(6)
	v.u8() // item type
	return parent, v
}

func parseRMStroke(b *rmReader, version uint8) (rmStroke, bool) {
	parent, v := sceneItem(b)
	if v == nil || b.err != nil {
		return rmStroke{}, false
	}
	s := rmStroke{Layer: parent}
	s.Tool = int(v.tagU32(1))
	s.Color = int(v.tagU32(2))
	s.Thickness = v.tagF64(3)
	v.tagF32(4) // starting length
	pts := v.sub(5)
	if v.err != nil {
		return rmStroke{}, false
	}
	size := 14
	if version < 2 {
		size = 24
	}
	n := len(pts.b) / size
	s.Points = make([]rmPoint, 0, n)
	for range n {
		var p rmPoint
		p.X, p.Y = pts.f32(), pts.f32()
		if version < 2 {
			p.Speed, p.Direction, p.Width, p.Pressure = pts.f32(), pts.f32(), pts.f32(), pts.f32()
		} else {
			p.Speed = float64(pts.u16()) / 4
			p.Width = float64(pts.u16()) / 4
			p.Direction = float64(pts.u8()) / 255 * 2 * math.Pi
			p.Pressure = float64(pts.u8()) / 255
		}
		s.Points = append(s.Points, p)
	}
	// After the timestamp (6) and an optional move id (7), newer firmware
	// adds the color itself (8) for its wider range of highlighter and pen
	// colors.
	if v.find(8, tagByte4) {
		s.ARGB = v.tagU32(8)
	}
	return s, len(s.Points) > 0
}

func parseRMGlyph(b *rmReader) (rmHighlight, bool) {
	_, v := sceneItem(b)
	if v == nil || b.err != nil {
		return rmHighlight{}, false
	}
	var h rmHighlight
	if v.has(2, tagByte4) {
		v.tagU32(2) // start
	}
	v.tagU32(3) // length
	h.Color = int(v.tagU32(4))
	if s := v.optSub(5); s != nil {
		h.Text = s.rmString()
	}
	if s := v.optSub(6); s != nil {
		n := s.varuint()
		for i := uint64(0); i < n && s.err == nil; i++ {
			h.Rects = append(h.Rects, [4]float64{s.f64(), s.f64(), s.f64(), s.f64()})
		}
	}
	return h, v.err == nil
}

// parseRMImageFiles reads the table of the page's image files: for each,
// the key images name it by, and its file name, as a last-writer-wins
// value (a timestamp, then the name).
func parseRMImageFiles(b *rmReader, files map[[16]byte]string) {
	t := b.sub(1)
	n := t.varuint()
	for i := uint64(0); i < n && t.err == nil; i++ {
		e := t.sub(0)
		var key [16]byte
		copy(key[:], e.bytes(16))
		if v := e.optSub(1); v != nil {
			v.tagID(1)
			name := v.sub(2).rmString()
			if v.err == nil && validImageFile(name) {
				files[key] = name
			}
		}
	}
}

// validImageFile is true for a bare file name, which can't reach outside
// the page's folder.
func validImageFile(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

// parseRMImage reads an image item: which file it shows (by its key in the
// image file table, as a last-writer-wins value), and the mesh it is drawn
// on.
func parseRMImage(b *rmReader) (rmImage, bool) {
	parent, v := sceneItem(b)
	if v == nil || b.err != nil {
		return rmImage{}, false
	}
	im := rmImage{Layer: parent}
	ref := v.sub(1)
	ref.tagID(1)
	copy(im.ref[:], ref.sub(2).bytes(16))
	if !v.find(3, tagSub) {
		return rmImage{}, false
	}
	verts := v.sub(3)
	n := verts.varuint() // floats, four to a corner
	for i := uint64(0); i+4 <= n && verts.err == nil; i += 4 {
		im.Vertices = append(im.Vertices, [4]float64{verts.f32(), verts.f32(), verts.f32(), verts.f32()})
	}
	if idx := v.optSub(4); idx != nil {
		n := idx.varuint()
		for i := uint64(0); i < n && idx.err == nil; i++ {
			if k := int(idx.u32()); k < len(im.Vertices) {
				im.Indices = append(im.Indices, k)
			}
		}
	}
	im.Indices = im.Indices[:len(im.Indices)/3*3]
	if len(im.Indices) == 0 {
		for i := 2; i < len(im.Vertices); i++ { // a fan, failing a list
			im.Indices = append(im.Indices, 0, i-1, i)
		}
	}
	ok := ref.err == nil && verts.err == nil && len(im.Indices) > 0
	return im, ok
}

func parseRMText(b *rmReader, page *rmPage) ([]rmTextItem, map[rmID]int) {
	b.tagID(1)
	all := b.sub(2)
	itemsBlock := all.sub(1).sub(1)
	n := itemsBlock.varuint()
	var items []rmTextItem
	for i := uint64(0); i < n && itemsBlock.err == nil; i++ {
		it := itemsBlock.sub(0)
		var t rmTextItem
		if it.has(1, tagID) { // older files carry a parent id first
			it.tagID(1)
		}
		t.id = it.tagID(2)
		t.left = it.tagID(3)
		t.right = it.tagID(4)
		t.deleted = int(it.tagU32(5))
		if s := it.optSub(6); s != nil {
			t.text = s.rmString()
			// An item with no text but a formatting code (bold, italic) still
			// takes a place in the sequence that others are placed against.
			t.marker = t.text == ""
		}
		if it.err != nil {
			break
		}
		items = append(items, t)
	}
	// Paragraph styles, each keyed by the newline that starts its paragraph
	// (the zero id for the first one).
	styles := map[rmID]int{}
	if fb := all.optSub(2); fb != nil {
		f := fb.sub(1)
		n := f.varuint()
		for i := uint64(0); i < n && f.err == nil; i++ {
			char := f.id()
			f.tagID(1)
			s := f.sub(2)
			s.u8()
			styles[char] = int(s.u8())
		}
	}
	if pos := b.optSub(3); pos != nil {
		page.TextX, page.TextY = pos.f64(), pos.f64()
	}
	if b.has(4, tagByte4) {
		page.TextWidth = b.tagF32(4)
	}
	return items, styles
}

// Paragraph styles.
const (
	rmStylePlain    = 1
	rmStyleHeading  = 2
	rmStyleBold     = 3
	rmStyleBullet   = 4
	rmStyleBullet2  = 5
	rmStyleCheckbox = 6
	rmStyleChecked  = 7
)

// orderText puts typed text in order and writes it as Markdown: headings
// with "#", bullets with "-" and checkboxes with "[ ]" or "[x]". Text is a
// CRDT sequence: each character sits between a left and a right neighbour,
// named by id (an item of several characters gives them consecutive ids), so
// the order is a topological sort of those relations, ties broken by id, as
// rmscene does.
func orderText(items []rmTextItem, styles map[rmID]int, top float64) (string, map[rmID]float64) {
	type char struct {
		id, left, right rmID
		r               string
	}
	var chars []char
	for _, it := range items {
		runes := []rune(it.text)
		n := len(runes)
		if n == 0 {
			n = it.deleted
		}
		if n == 0 && it.marker {
			n = 1
		}
		for i := range n {
			c := char{id: rmID{it.id.a, it.id.b + uint64(i)}, left: it.left, right: it.right}
			if i > 0 {
				c.left = rmID{it.id.a, it.id.b + uint64(i-1)}
			}
			if i < n-1 {
				c.right = rmID{it.id.a, it.id.b + uint64(i+1)}
			}
			if i < len(runes) {
				c.r = string(runes[i])
			}
			chars = append(chars, c)
		}
	}
	// Two ids anchor strokes without being characters; rmc puts them here.
	anchors := map[rmID]float64{{0, 1<<48 - 2}: 100, {0, 1<<48 - 1}: 100}
	if len(chars) == 0 {
		return "", anchors
	}
	// Node keys: real ids, plus the start and end markers.
	start, end := rmID{255, math.MaxUint64 - 1}, rmID{255, math.MaxUint64}
	side := func(id rmID, isLeft bool) rmID {
		if id == (rmID{}) {
			if isLeft {
				return start
			}
			return end
		}
		return id
	}
	byID := map[rmID]char{}
	before := map[rmID]map[rmID]bool{}
	need := func(k rmID) {
		if before[k] == nil {
			before[k] = map[rmID]bool{}
		}
	}
	for _, c := range chars {
		byID[c.id] = c
	}
	for _, c := range chars {
		l, r := side(c.left, true), side(c.right, false)
		need(c.id)
		need(l)
		need(r)
		before[c.id][l] = true
		before[r][c.id] = true
	}
	var ordered []char
	done := map[rmID]bool{}
	for len(done) < len(before) {
		var ready []rmID
		for k, deps := range before {
			if done[k] {
				continue
			}
			ok := true
			for d := range deps {
				if !done[d] {
					ok = false
					break
				}
			}
			if ok {
				ready = append(ready, k)
			}
		}
		if len(ready) == 0 {
			// A cycle: give up on ordering the rest and take it by id.
			for k := range before {
				if !done[k] {
					ready = append(ready, k)
				}
			}
		}
		sort.Slice(ready, func(i, j int) bool { return ready[i].less(ready[j]) })
		for _, k := range ready {
			done[k] = true
			if c, ok := byID[k]; ok {
				ordered = append(ordered, c)
			}
		}
	}

	var out strings.Builder
	paragraph := func(style int) {
		switch style {
		case rmStyleHeading:
			out.WriteString("# ")
		case rmStyleBullet:
			out.WriteString("- ")
		case rmStyleBullet2:
			out.WriteString("  - ")
		case rmStyleCheckbox:
			out.WriteString("[ ] ")
		case rmStyleChecked:
			out.WriteString("[x] ")
		}
	}
	// Each paragraph is taken to be one line, of a height that depends on
	// its style, which is how rmc places anchored strokes. (A paragraph that
	// wraps puts the strokes below it too high.)
	lineHeight := func(style int) float64 {
		switch style {
		case rmStyleHeading:
			return 150
		case rmStyleBullet, rmStyleBullet2, rmStyleCheckbox, rmStyleChecked:
			return 35
		case rmStylePlain:
			return 71
		}
		return 70
	}
	y := top
	style := styles[rmID{}]
	if _, ok := styles[rmID{}]; !ok {
		style = rmStylePlain
	}
	paragraph(style)
	for _, c := range ordered {
		if c.r == "\n" {
			y += lineHeight(style)
			if st, ok := styles[c.id]; ok {
				style = st
			} else {
				style = rmStylePlain
			}
		}
		anchors[c.id] = y
		switch c.r {
		case "\u2028": // a line break within a paragraph
			out.WriteString("\n")
		case "\n":
			out.WriteString("\n")
			paragraph(style)
		default:
			out.WriteString(c.r)
		}
	}
	text := strings.TrimSpace(out.String())
	if strings.Trim(text, "#-[]x \n") == "" {
		text = ""
	}
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}
	return text, anchors
}
