package remarkable

// Just enough PDF to draw the user's handwriting on the PDF they wrote on:
// a reader for the original (cross-reference tables and streams, object
// streams, and a scan of the whole file when those are broken) and a writer
// that copies its pages in the tablet's order, each with its strokes drawn
// over it, and adds pages of its own for those added on the tablet and for
// notebooks. Content streams, fonts and images are copied as they are, never
// decoded; images pasted onto pages on the tablet are added. Encrypted PDFs
// aren't read.

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
)

type (
	pdfName   string
	pdfStr    []byte
	pdfArray  []any
	pdfDict   map[pdfName]any
	pdfRef    struct{ Num, Gen int }
	pdfStream struct {
		Dict pdfDict
		Data []byte // still encoded
	}
	pdfKeyword string
)

type pdfXref struct {
	inStream bool
	off      int64 // byte offset, or the object stream's number
}

type pdfFile struct {
	data    []byte
	xref    map[int]pdfXref
	trailer pdfDict
	cache   map[int]any
	loading map[int]bool
}

// readPDF opens a PDF, falling back to a scan of the whole file for its
// objects when its cross-reference data is missing or wrong.
func readPDF(data []byte) (*pdfFile, error) {
	if !bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF-")) {
		return nil, errors.New("not a PDF")
	}
	f := &pdfFile{data: data}
	if err := f.loadXref(); err != nil || f.catalog() == nil || len(f.pages()) == 0 {
		f.scan()
	}
	if f.trailer["Encrypt"] != nil {
		return nil, errors.New("the PDF is encrypted")
	}
	if f.catalog() == nil || len(f.pages()) == 0 {
		return nil, errors.New("can't find the PDF's pages")
	}
	return f, nil
}

func (f *pdfFile) reset() {
	f.xref, f.trailer = map[int]pdfXref{}, pdfDict{}
	f.cache, f.loading = map[int]any{}, map[int]bool{}
}

func (f *pdfFile) catalog() pdfDict {
	d, _ := f.resolve(f.trailer["Root"]).(pdfDict)
	if d == nil || f.resolve(d["Pages"]) == nil {
		return nil
	}
	return d
}

func (f *pdfFile) loadXref() error {
	f.reset()
	i := bytes.LastIndex(f.data, []byte("startxref"))
	if i < 0 {
		return errors.New("no startxref")
	}
	l := &pdfLexer{b: f.data, pos: i + len("startxref")}
	off, ok := l.int()
	if !ok {
		return errors.New("bad startxref")
	}
	seen := map[int64]bool{}
	first := true
	for off > 0 && off < int64(len(f.data)) && !seen[off] {
		seen[off] = true
		trailer, err := f.xrefSection(off)
		if err != nil {
			return err
		}
		if first {
			f.trailer, first = trailer, false
		}
		if x, ok := trailer["XRefStm"].(int64); ok && !seen[x] {
			seen[x] = true
			if _, err := f.xrefSection(x); err != nil {
				return err
			}
		}
		off, _ = trailer["Prev"].(int64)
	}
	return nil
}

// xrefSection reads one cross-reference section at off, a table or a
// stream, adding entries not already known (later sections come first).
func (f *pdfFile) xrefSection(off int64) (pdfDict, error) {
	l := &pdfLexer{b: f.data, pos: int(off)}
	l.ws()
	if bytes.HasPrefix(f.data[l.pos:], []byte("xref")) {
		l.pos += 4
		for {
			l.ws()
			if bytes.HasPrefix(f.data[l.pos:], []byte("trailer")) {
				l.pos += len("trailer")
				break
			}
			start, ok1 := l.int()
			count, ok2 := l.int()
			if !ok1 || !ok2 {
				return nil, errors.New("bad xref table")
			}
			for n := range count {
				o, ok := l.int()
				l.int() // generation
				l.ws()
				if !ok || l.pos >= len(f.data) {
					return nil, errors.New("bad xref entry")
				}
				kind := f.data[l.pos]
				l.pos++
				num := int(start + n)
				if _, known := f.xref[num]; known {
					continue
				}
				if kind == 'n' && num > 0 {
					f.xref[num] = pdfXref{off: o}
				} else if kind == 'f' {
					f.xref[num] = pdfXref{off: -1}
				}
			}
		}
		v, err := l.value(f)
		if err != nil {
			return nil, err
		}
		d, ok := v.(pdfDict)
		if !ok {
			return nil, errors.New("bad trailer")
		}
		return d, nil
	}
	_, v, err := f.parseObjectAt(int(off))
	if err != nil {
		return nil, err
	}
	s, ok := v.(*pdfStream)
	if !ok || s.Dict["Type"] != pdfName("XRef") {
		return nil, errors.New("startxref points at neither a table nor a stream")
	}
	data, err := f.decode(s)
	if err != nil {
		return nil, err
	}
	var w [3]int
	ws, _ := s.Dict["W"].(pdfArray)
	if len(ws) != 3 {
		return nil, errors.New("bad xref stream")
	}
	for i := range w {
		n, _ := ws[i].(int64)
		w[i] = int(n)
	}
	size, _ := s.Dict["Size"].(int64)
	index := pdfArray{int64(0), size}
	if a, ok := s.Dict["Index"].(pdfArray); ok {
		index = a
	}
	field := func(b []byte, def int64) int64 {
		if len(b) == 0 {
			return def
		}
		var v int64
		for _, c := range b {
			v = v<<8 | int64(c)
		}
		return v
	}
	row := w[0] + w[1] + w[2]
	pos := 0
	for i := 0; i+1 < len(index); i += 2 {
		start, _ := index[i].(int64)
		count, _ := index[i+1].(int64)
		for n := range count {
			if pos+row > len(data) {
				break
			}
			r := data[pos : pos+row]
			pos += row
			num := int(start + n)
			if _, known := f.xref[num]; known {
				continue
			}
			a := field(r[w[0]:w[0]+w[1]], 0)
			switch field(r[:w[0]], 1) {
			case 0:
				f.xref[num] = pdfXref{off: -1}
			case 1:
				f.xref[num] = pdfXref{off: a}
			case 2:
				f.xref[num] = pdfXref{inStream: true, off: a}
			}
		}
	}
	return s.Dict, nil
}

var pdfObjHeader = regexp.MustCompile(`(\d+)\s+(\d+)\s+obj\b`)

// scan finds every object by its "n g obj" header, for files whose
// cross-reference data is broken. Later definitions win, as they would in
// an updated file.
func (f *pdfFile) scan() {
	f.reset()
	for _, m := range pdfObjHeader.FindAllSubmatchIndex(f.data, -1) {
		if m[0] > 0 && !pdfSpace(f.data[m[0]-1]) {
			continue
		}
		num, _ := strconv.Atoi(string(f.data[m[2]:m[3]]))
		f.xref[num] = pdfXref{off: int64(m[2])}
	}
	var nums []int
	for n := range f.xref {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	// Objects inside object streams.
	for _, n := range nums {
		if s, ok := f.object(n).(*pdfStream); ok && s.Dict["Type"] == pdfName("ObjStm") {
			for _, member := range f.objStmMembers(n) {
				if _, known := f.xref[member]; !known {
					f.xref[member] = pdfXref{inStream: true, off: int64(n)}
				}
			}
		}
	}
	f.cache = map[int]any{}
	for i := bytes.LastIndex(f.data, []byte("trailer")); i >= 0 && f.catalog() == nil; i = bytes.LastIndex(f.data[:i], []byte("trailer")) {
		l := &pdfLexer{b: f.data, pos: i + len("trailer")}
		if d, err := l.value(f); err == nil {
			if d, ok := d.(pdfDict); ok && d["Root"] != nil {
				f.trailer = d
			}
		}
	}
	if f.catalog() == nil {
		for _, n := range nums {
			if d, ok := f.object(n).(pdfDict); ok && d["Type"] == pdfName("Catalog") {
				f.trailer["Root"] = pdfRef{n, 0}
				break
			}
		}
	}
}

func (f *pdfFile) resolve(v any) any {
	if f == nil {
		return v
	}
	for range 32 {
		r, ok := v.(pdfRef)
		if !ok {
			return v
		}
		v = f.object(r.Num)
	}
	return nil
}

func (f *pdfFile) object(num int) any {
	if v, ok := f.cache[num]; ok {
		return v
	}
	x, ok := f.xref[num]
	if !ok || x.off < 0 || f.loading[num] {
		return nil
	}
	f.loading[num] = true
	defer delete(f.loading, num)
	var v any
	if x.inStream {
		v = f.objStmObject(int(x.off), num)
	} else if n, val, err := f.parseObjectAt(int(x.off)); err == nil && n == num {
		v = val
	}
	f.cache[num] = v
	return v
}

// objStm decodes an object stream and returns its data, its header (pairs
// of object number and offset) and where the objects start.
func (f *pdfFile) objStm(stm int) ([]byte, [][2]int64, int64) {
	s, ok := f.object(stm).(*pdfStream)
	if !ok {
		return nil, nil, 0
	}
	data, err := f.decode(s)
	if err != nil {
		return nil, nil, 0
	}
	n, _ := s.Dict["N"].(int64)
	first, _ := s.Dict["First"].(int64)
	l := &pdfLexer{b: data}
	var header [][2]int64
	for range n {
		num, ok1 := l.int()
		off, ok2 := l.int()
		if !ok1 || !ok2 {
			break
		}
		header = append(header, [2]int64{num, off})
	}
	return data, header, first
}

func (f *pdfFile) objStmMembers(stm int) []int {
	_, header, _ := f.objStm(stm)
	var out []int
	for _, h := range header {
		out = append(out, int(h[0]))
	}
	return out
}

func (f *pdfFile) objStmObject(stm, num int) any {
	data, header, first := f.objStm(stm)
	for _, h := range header {
		if int(h[0]) == num {
			v, err := (&pdfLexer{b: data, pos: int(first + h[1])}).value(f)
			if err != nil {
				return nil
			}
			return v
		}
	}
	return nil
}

func (f *pdfFile) parseObjectAt(off int) (int, any, error) {
	if off < 0 || off >= len(f.data) {
		return 0, nil, errors.New("object out of range")
	}
	l := &pdfLexer{b: f.data, pos: off}
	num, ok1 := l.int()
	_, ok2 := l.int()
	l.ws()
	if !ok1 || !ok2 || !bytes.HasPrefix(f.data[l.pos:], []byte("obj")) {
		return 0, nil, errors.New("no object here")
	}
	l.pos += 3
	v, err := l.value(f)
	return int(num), v, err
}

// decode undoes a stream's filters. Only Flate, with PNG predictors, is
// needed: it is what cross-reference and object streams use.
func (f *pdfFile) decode(s *pdfStream) ([]byte, error) {
	data := s.Data
	var filters, parms pdfArray
	switch v := f.resolve(s.Dict["Filter"]).(type) {
	case pdfName:
		filters = pdfArray{v}
	case pdfArray:
		filters = v
	}
	switch v := f.resolve(s.Dict["DecodeParms"]).(type) {
	case pdfDict:
		parms = pdfArray{v}
	case pdfArray:
		parms = v
	}
	for i, fl := range filters {
		if fl != pdfName("FlateDecode") && fl != pdfName("Fl") {
			return nil, fmt.Errorf("unsupported filter %v", fl)
		}
		var err error
		if data, err = inflate(data); err != nil {
			return nil, err
		}
		if i < len(parms) {
			if p, ok := f.resolve(parms[i]).(pdfDict); ok {
				data = unpredict(data, p)
			}
		}
	}
	return data, nil
}

// inflate decompresses zlib data, keeping what it could read of a stream
// cut short.
func inflate(data []byte) ([]byte, error) {
	var r io.Reader
	if zr, err := zlib.NewReader(bytes.NewReader(data)); err == nil {
		r = zr
	} else {
		r = flate.NewReader(bytes.NewReader(data))
	}
	out, err := io.ReadAll(r)
	if err != nil && len(out) == 0 {
		return nil, err
	}
	return out, nil
}

// unpredict undoes PNG predictors.
func unpredict(data []byte, p pdfDict) []byte {
	pred, _ := p["Predictor"].(int64)
	if pred < 10 {
		return data
	}
	cols, colors, bpc := int64(1), int64(1), int64(8)
	if v, ok := p["Columns"].(int64); ok {
		cols = v
	}
	if v, ok := p["Colors"].(int64); ok {
		colors = v
	}
	if v, ok := p["BitsPerComponent"].(int64); ok {
		bpc = v
	}
	bpp := max(int(colors*bpc/8), 1)
	rowLen := int((colors*bpc*cols + 7) / 8)
	var out []byte
	prev := make([]byte, rowLen)
	for i := 0; i < len(data); i += rowLen + 1 {
		ft := data[i]
		row := make([]byte, rowLen)
		copy(row, data[i+1:min(i+1+rowLen, len(data))])
		for j := range row {
			var a, c byte
			if j >= bpp {
				a, c = row[j-bpp], prev[j-bpp]
			}
			b := prev[j]
			switch ft {
			case 1:
				row[j] += a
			case 2:
				row[j] += b
			case 3:
				row[j] += byte((int(a) + int(b)) / 2)
			case 4:
				pa, pb, pc := absInt(int(b)-int(c)), absInt(int(a)-int(c)), absInt(int(a)+int(b)-2*int(c))
				switch {
				case pa <= pb && pa <= pc:
					row[j] += a
				case pb <= pc:
					row[j] += b
				default:
					row[j] += c
				}
			}
		}
		out = append(out, row...)
		prev = row
	}
	return out
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

type pdfLexer struct {
	b   []byte
	pos int
}

func pdfSpace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == '\f' || c == 0
}

func pdfDelim(c byte) bool {
	return pdfSpace(c) || bytes.IndexByte([]byte("()<>[]{}/%"), c) >= 0
}

func (l *pdfLexer) ws() {
	for l.pos < len(l.b) {
		c := l.b[l.pos]
		if pdfSpace(c) {
			l.pos++
		} else if c == '%' {
			for l.pos < len(l.b) && l.b[l.pos] != '\n' && l.b[l.pos] != '\r' {
				l.pos++
			}
		} else {
			return
		}
	}
}

// int reads a non-negative integer, if one is next.
func (l *pdfLexer) int() (int64, bool) {
	l.ws()
	start := l.pos
	for l.pos < len(l.b) && l.b[l.pos] >= '0' && l.b[l.pos] <= '9' {
		l.pos++
	}
	if l.pos == start || l.pos < len(l.b) && !pdfDelim(l.b[l.pos]) {
		l.pos = start
		return 0, false
	}
	n, err := strconv.ParseInt(string(l.b[start:l.pos]), 10, 64)
	return n, err == nil
}

var errPDFEnd = errors.New("PDF object ends early")

func (l *pdfLexer) value(f *pdfFile) (any, error) {
	l.ws()
	if l.pos >= len(l.b) {
		return nil, errPDFEnd
	}
	c := l.b[l.pos]
	switch {
	case c == '/':
		l.pos++
		start := l.pos
		for l.pos < len(l.b) && !pdfDelim(l.b[l.pos]) {
			l.pos++
		}
		return pdfName(unescapeName(l.b[start:l.pos])), nil
	case c == '(':
		return l.literal(), nil
	case c == '<' && l.pos+1 < len(l.b) && l.b[l.pos+1] == '<':
		l.pos += 2
		d := pdfDict{}
		for {
			l.ws()
			if l.pos >= len(l.b) {
				return nil, errPDFEnd
			}
			if l.pos+1 < len(l.b) && l.b[l.pos] == '>' && l.b[l.pos+1] == '>' {
				l.pos += 2
				break
			}
			k, err := l.value(f)
			if err != nil {
				return nil, err
			}
			name, ok := k.(pdfName)
			if !ok {
				continue // junk where a key should be
			}
			v, err := l.value(f)
			if err != nil {
				return nil, err
			}
			if kw, ok := v.(pdfKeyword); ok && kw == ">>" {
				break
			}
			d[name] = v
		}
		// A stream follows its dictionary.
		save := l.pos
		l.ws()
		if bytes.HasPrefix(l.b[l.pos:], []byte("stream")) {
			l.pos += len("stream")
			if l.pos < len(l.b) && l.b[l.pos] == '\r' {
				l.pos++
			}
			if l.pos < len(l.b) && l.b[l.pos] == '\n' {
				l.pos++
			}
			return &pdfStream{Dict: d, Data: l.streamData(f, d)}, nil
		}
		l.pos = save
		return d, nil
	case c == '<':
		l.pos++
		var hex []byte
		for l.pos < len(l.b) && l.b[l.pos] != '>' {
			if h := l.b[l.pos]; !pdfSpace(h) {
				hex = append(hex, h)
			}
			l.pos++
		}
		l.pos++
		if len(hex)%2 == 1 {
			hex = append(hex, '0')
		}
		out := make([]byte, len(hex)/2)
		for i := range out {
			v, _ := strconv.ParseUint(string(hex[2*i:2*i+2]), 16, 8)
			out[i] = byte(v)
		}
		return pdfStr(out), nil
	case c == '[':
		l.pos++
		a := pdfArray{}
		for {
			l.ws()
			if l.pos >= len(l.b) {
				return nil, errPDFEnd
			}
			if l.b[l.pos] == ']' {
				l.pos++
				return a, nil
			}
			v, err := l.value(f)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
	case c == '>' && l.pos+1 < len(l.b) && l.b[l.pos+1] == '>':
		l.pos += 2
		return pdfKeyword(">>"), nil
	case c == ']' || c == ')' || c == '>' || c == '{' || c == '}':
		l.pos++
		return pdfKeyword(string(c)), nil
	case c >= '0' && c <= '9' || c == '-' || c == '+' || c == '.':
		start := l.pos
		l.pos++
		for l.pos < len(l.b) && !pdfDelim(l.b[l.pos]) {
			l.pos++
		}
		tok := string(l.b[start:l.pos])
		if n, err := strconv.ParseInt(tok, 10, 64); err == nil {
			// "n g R" is a reference.
			if n >= 0 {
				save := l.pos
				if g, ok := l.int(); ok {
					l.ws()
					if l.pos < len(l.b) && l.b[l.pos] == 'R' && (l.pos+1 == len(l.b) || pdfDelim(l.b[l.pos+1])) {
						l.pos++
						return pdfRef{int(n), int(g)}, nil
					}
				}
				l.pos = save
			}
			return n, nil
		}
		v, err := strconv.ParseFloat(tok, 64)
		if err != nil {
			return int64(0), nil
		}
		return v, nil
	default:
		start := l.pos
		for l.pos < len(l.b) && !pdfDelim(l.b[l.pos]) {
			l.pos++
		}
		if l.pos == start {
			l.pos++
		}
		switch kw := string(l.b[start:l.pos]); kw {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return pdfKeyword(kw), nil
		}
	}
}

func unescapeName(b []byte) string {
	if bytes.IndexByte(b, '#') < 0 {
		return string(b)
	}
	var out []byte
	for i := 0; i < len(b); i++ {
		if b[i] == '#' && i+2 < len(b) {
			if v, err := strconv.ParseUint(string(b[i+1:i+3]), 16, 8); err == nil {
				out = append(out, byte(v))
				i += 2
				continue
			}
		}
		out = append(out, b[i])
	}
	return string(out)
}

func (l *pdfLexer) literal() pdfStr {
	l.pos++ // (
	var out []byte
	depth := 1
	for l.pos < len(l.b) {
		c := l.b[l.pos]
		l.pos++
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return out
			}
		case '\\':
			if l.pos >= len(l.b) {
				return out
			}
			e := l.b[l.pos]
			l.pos++
			switch e {
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 't':
				c = '\t'
			case 'b':
				c = '\b'
			case 'f':
				c = '\f'
			case '\r':
				if l.pos < len(l.b) && l.b[l.pos] == '\n' {
					l.pos++
				}
				continue
			case '\n':
				continue
			default:
				if e >= '0' && e <= '7' {
					v := int(e - '0')
					for k := 0; k < 2 && l.pos < len(l.b) && l.b[l.pos] >= '0' && l.b[l.pos] <= '7'; k++ {
						v = v*8 + int(l.b[l.pos]-'0')
						l.pos++
					}
					c = byte(v)
				} else {
					c = e
				}
			}
		}
		out = append(out, c)
	}
	return out
}

// streamData reads a stream's bytes, trusting /Length only when
// "endstream" follows it.
func (l *pdfLexer) streamData(f *pdfFile, d pdfDict) []byte {
	start := l.pos
	n, _ := f.resolve(d["Length"]).(int64)
	if n > 0 && start+int(n) <= len(l.b) {
		e := &pdfLexer{b: l.b, pos: start + int(n)}
		e.ws()
		if bytes.HasPrefix(l.b[e.pos:], []byte("endstream")) {
			l.pos = e.pos + len("endstream")
			return l.b[start : start+int(n)]
		}
	}
	i := bytes.Index(l.b[start:], []byte("endstream"))
	if i < 0 {
		l.pos = len(l.b)
		return l.b[start:]
	}
	end := start + i
	l.pos = end + len("endstream")
	if end > start && l.b[end-1] == '\n' {
		end--
	}
	if end > start && l.b[end-1] == '\r' {
		end--
	}
	return l.b[start:end]
}

// A page of the original, with what it inherits from the page tree.
type pdfPage struct {
	ref       pdfRef
	dict      pdfDict
	resources any
	mediaBox  [4]float64
	cropBox   *[4]float64
	rotate    int
}

func (f *pdfFile) pages() []pdfPage {
	var out []pdfPage
	seen := map[pdfRef]bool{}
	var walk func(v any, inherit pdfDict, depth int)
	walk = func(v any, inherit pdfDict, depth int) {
		ref, _ := v.(pdfRef)
		if depth > 64 || ref != (pdfRef{}) && seen[ref] {
			return
		}
		seen[ref] = true
		d, ok := f.resolve(v).(pdfDict)
		if !ok {
			return
		}
		attrs := pdfDict{}
		for k, v := range inherit {
			attrs[k] = v
		}
		for _, k := range []pdfName{"Resources", "MediaBox", "CropBox", "Rotate"} {
			if v, ok := d[k]; ok {
				attrs[k] = v
			}
		}
		if kids, ok := f.resolve(d["Kids"]).(pdfArray); ok && d["Type"] != pdfName("Page") {
			for _, k := range kids {
				walk(k, attrs, depth+1)
			}
			return
		}
		p := pdfPage{ref: ref, dict: d, resources: attrs["Resources"], mediaBox: [4]float64{0, 0, 612, 792}}
		if b, ok := f.box(attrs["MediaBox"]); ok {
			p.mediaBox = b
		}
		if b, ok := f.box(attrs["CropBox"]); ok {
			p.cropBox = &b
		}
		switch r := f.resolve(attrs["Rotate"]).(type) {
		case int64:
			p.rotate = int((r%360 + 360) % 360)
		case float64:
			p.rotate = (int(r)%360 + 360) % 360
		}
		p.rotate -= p.rotate % 90
		out = append(out, p)
	}
	if c := f.catalog(); c != nil {
		walk(c["Pages"], pdfDict{}, 0)
	}
	return out
}

func (f *pdfFile) box(v any) ([4]float64, bool) {
	a, ok := f.resolve(v).(pdfArray)
	if !ok || len(a) != 4 {
		return [4]float64{}, false
	}
	var b [4]float64
	for i, x := range a {
		switch n := f.resolve(x).(type) {
		case int64:
			b[i] = float64(n)
		case float64:
			b[i] = n
		default:
			return b, false
		}
	}
	b = [4]float64{min(b[0], b[2]), min(b[1], b[3]), max(b[0], b[2]), max(b[1], b[3])}
	return b, b[2]-b[0] > 0 && b[3]-b[1] > 0
}

// visibleBox is the part of the page that is shown.
func (p pdfPage) visibleBox() [4]float64 {
	if p.cropBox != nil {
		c, m := *p.cropBox, p.mediaBox
		b := [4]float64{max(c[0], m[0]), max(c[1], m[1]), min(c[2], m[2]), min(c[3], m[3])}
		if b[2] > b[0] && b[3] > b[1] {
			return b
		}
	}
	return p.mediaBox
}

// shownSize is the page's width and height as shown, after /Rotate.
func (p pdfPage) shownSize() (w, h float64) {
	b := p.visibleBox()
	w, h = b[2]-b[0], b[3]-b[1]
	if p.rotate == 90 || p.rotate == 270 {
		w, h = h, w
	}
	return w, h
}

// rmScale is the PDF points per screen pixel that makes a page of this
// size fit inside the screen, for pages added between a PDF's pages.
func rmScale(w, h float64) float64 {
	if h/w < rmHeight/rmWidth {
		return w / rmWidth
	}
	return h / rmHeight
}

// rmTransform takes the tablet's page coordinates to this page's: the
// tablet puts the page, as shown (after /Rotate), at its real size at the
// screen's 226 dpi, whatever its size and however it is zoomed, with its
// top at y = 0 and its middle at x = 0. (Not fitted to the screen: a
// letter page's strokes then come out a line low and too wide.)
func (p pdfPage) rmTransform() [6]float64 {
	b := p.visibleBox()
	llx, lly, urx, ury := b[0], b[1], b[2], b[3]
	w, _ := p.shownSize()
	s := pointsPerPixel
	switch p.rotate {
	case 90:
		return [6]float64{0, s, s, 0, llx, lly + w/2}
	case 180:
		return [6]float64{-s, 0, 0, s, urx - w/2, lly}
	case 270:
		return [6]float64{0, -s, -s, 0, urx, ury - w/2}
	}
	return [6]float64{s, 0, 0, -s, llx + w/2, ury}
}

// outPage is a page of the PDF being made: a page of the original (src),
// or one of its own of the given size, with ink drawn on it and an
// optional label in its corner.
type outPage struct {
	src           *pdfPage
	width, height float64
	ink           *pdfInk
	label         string
}

type pdfWriter struct {
	src    *pdfFile
	objs   [][]byte
	copied map[int]pdfRef
	pages  map[pdfRef]pdfRef
}

func (w *pdfWriter) alloc() pdfRef {
	w.objs = append(w.objs, nil)
	return pdfRef{len(w.objs), 0}
}

func (w *pdfWriter) set(r pdfRef, v any) {
	var b bytes.Buffer
	pdfSerialize(&b, v)
	w.objs[r.Num-1] = b.Bytes()
}

// copy brings a value from the original over, with the objects it refers
// to. References to pages being copied point at their copies, and those to
// pages left out become null.
func (w *pdfWriter) copy(v any) any {
	switch v := v.(type) {
	case pdfRef:
		if p, ok := w.pages[v]; ok {
			return p
		}
		if r, ok := w.copied[v.Num]; ok {
			return r
		}
		obj := w.src.object(v.Num)
		if d, ok := obj.(pdfDict); ok && (d["Type"] == pdfName("Page") || d["Type"] == pdfName("Pages")) {
			return nil
		}
		r := w.alloc()
		w.copied[v.Num] = r
		w.set(r, w.copy(obj))
		return r
	case pdfDict:
		d := pdfDict{}
		for k, x := range v {
			d[k] = w.copy(x)
		}
		return d
	case pdfArray:
		a := make(pdfArray, len(v))
		for i, x := range v {
			a[i] = w.copy(x)
		}
		return a
	case *pdfStream:
		return &pdfStream{Dict: w.copy(v.Dict).(pdfDict), Data: v.Data}
	}
	return v
}

// direct copies a dictionary from the original (fonts, graphics states) as
// a direct one, so entries can be added to it.
func (w *pdfWriter) direct(v any) pdfDict {
	d := pdfDict{}
	if w.src == nil {
		return d
	}
	if src, ok := w.src.resolve(v).(pdfDict); ok {
		for k, x := range src {
			d[k] = w.copy(x)
		}
	}
	return d
}

func flateStream(data []byte) *pdfStream {
	var b bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&b, zlib.BestCompression)
	zw.Write(data)
	zw.Close()
	return &pdfStream{Dict: pdfDict{"Filter": pdfName("FlateDecode")}, Data: b.Bytes()}
}

// writePDF makes a PDF of pages, copying those that come from the original
// src (nil if none do).
func writePDF(src *pdfFile, pages []outPage) []byte {
	w := &pdfWriter{src: src, copied: map[int]pdfRef{}, pages: map[pdfRef]pdfRef{}}
	catalog, root := w.alloc(), w.alloc()
	refs := make([]pdfRef, len(pages))
	for i, p := range pages {
		refs[i] = w.alloc()
		if p.src != nil && p.src.ref != (pdfRef{}) {
			if _, ok := w.pages[p.src.ref]; !ok {
				w.pages[p.src.ref] = refs[i]
			}
		}
	}
	var qRef, bigQRef, fontRef pdfRef
	for i, p := range pages {
		d := pdfDict{"Type": pdfName("Page"), "Parent": root}
		res := pdfDict{}
		var contents pdfArray
		if p.src != nil {
			for k, v := range p.src.dict {
				switch k {
				case "Type", "Parent", "Contents", "Resources", "MediaBox", "CropBox", "Rotate", "B", "Thumb", "StructParents":
				default:
					d[k] = w.copy(v)
				}
			}
			d["MediaBox"] = boxArray(p.src.mediaBox)
			if p.src.cropBox != nil {
				d["CropBox"] = boxArray(*p.src.cropBox)
			}
			if p.src.rotate != 0 {
				d["Rotate"] = int64(p.src.rotate)
			}
			res = w.direct(p.src.resources)
			switch c := p.src.dict["Contents"].(type) {
			case pdfRef:
				if a, ok := w.src.resolve(c).(pdfArray); ok {
					for _, x := range a {
						contents = append(contents, w.copy(x))
					}
				} else {
					contents = append(contents, w.copy(c))
				}
			case pdfArray:
				for _, x := range c {
					contents = append(contents, w.copy(x))
				}
			}
		} else {
			d["MediaBox"] = boxArray([4]float64{0, 0, p.width, p.height})
		}
		var extra bytes.Buffer
		usesFont := false
		if p.ink != nil {
			extra.Write(p.ink.ops.Bytes())
			usesFont = p.ink.useFont
		}
		if p.label != "" {
			fmt.Fprintf(&extra, "BT /%s 9 Tf 0.5 g 12 %s Td (%s) Tj ET\n", pdfInkFont, pdfNum(p.height-18), pdfWinAnsi(p.label))
			usesFont = true
		}
		if extra.Len() > 0 {
			// The original's drawing is wrapped in q/Q so whatever state it
			// leaves doesn't move the ink.
			if len(contents) > 0 {
				if qRef == (pdfRef{}) {
					qRef, bigQRef = w.alloc(), w.alloc()
					w.set(qRef, &pdfStream{Dict: pdfDict{}, Data: []byte("q\n")})
					w.set(bigQRef, &pdfStream{Dict: pdfDict{}, Data: []byte("\nQ\n")})
				}
				contents = append(append(pdfArray{qRef}, contents...), bigQRef)
			}
			ink := w.alloc()
			w.set(ink, flateStream(extra.Bytes()))
			contents = append(contents, ink)
			if p.ink != nil && len(p.ink.alphas) > 0 {
				gs := w.direct(w.resourceEntry(p.src, "ExtGState"))
				for name, a := range p.ink.alphas {
					g := pdfDict{"Type": pdfName("ExtGState"), "CA": a, "ca": a}
					if p.ink.blend[name] {
						g["BM"] = pdfName("Multiply")
					}
					gs[pdfName(name)] = g
				}
				res["ExtGState"] = gs
			}
			if p.ink != nil && len(p.ink.images) > 0 {
				xobjs := w.direct(w.resourceEntry(p.src, "XObject"))
				for name, data := range p.ink.images {
					img, mask, err := pdfImage(data)
					if err != nil {
						continue // cut short past its header; readers skip a missing image
					}
					if mask != nil {
						m := w.alloc()
						w.set(m, mask)
						img.Dict["SMask"] = m
					}
					r := w.alloc()
					w.set(r, img)
					xobjs[pdfName(name)] = r
				}
				res["XObject"] = xobjs
			}
			if usesFont {
				if fontRef == (pdfRef{}) {
					fontRef = w.alloc()
					w.set(fontRef, pdfDict{"Type": pdfName("Font"), "Subtype": pdfName("Type1"), "BaseFont": pdfName("Helvetica"), "Encoding": pdfName("WinAnsiEncoding")})
				}
				fonts := w.direct(w.resourceEntry(p.src, "Font"))
				fonts[pdfInkFont] = fontRef
				res["Font"] = fonts
			}
		}
		d["Resources"] = res
		if len(contents) == 1 {
			d["Contents"] = contents[0]
		} else if len(contents) > 1 {
			d["Contents"] = contents
		}
		w.set(refs[i], d)
	}
	kids := make(pdfArray, len(refs))
	for i, r := range refs {
		kids[i] = r
	}
	w.set(root, pdfDict{"Type": pdfName("Pages"), "Kids": kids, "Count": int64(len(refs))})
	w.set(catalog, pdfDict{"Type": pdfName("Catalog"), "Pages": root})

	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
	offsets := make([]int, len(w.objs))
	for i, body := range w.objs {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", i+1)
		if body == nil {
			body = []byte("null")
		}
		out.Write(body)
		out.WriteString("\nendobj\n")
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(w.objs)+1)
	for _, o := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(w.objs)+1, catalog.Num, xref)
	return out.Bytes()
}

// pdfImage makes an image XObject of a PNG or JPEG file, with a soft mask
// for its transparency when it has any. A JPEG in gray or YCbCr goes in as
// it is; anything else is decoded and stored losslessly, in gray when every
// pixel is.
func pdfImage(data []byte) (img, mask *pdfStream, err error) {
	if cfg, format, err := image.DecodeConfig(bytes.NewReader(data)); err == nil && format == "jpeg" {
		cs := map[color.Model]pdfName{color.GrayModel: "DeviceGray", color.YCbCrModel: "DeviceRGB"}[cfg.ColorModel]
		if cs != "" {
			return &pdfStream{Dict: pdfDict{
				"Type": pdfName("XObject"), "Subtype": pdfName("Image"), "Filter": pdfName("DCTDecode"),
				"Width": int64(cfg.Width), "Height": int64(cfg.Height), "ColorSpace": cs, "BitsPerComponent": int64(8),
			}, Data: data}, nil, nil
		}
	}
	m, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	r := m.Bounds()
	if r.Empty() {
		return nil, nil, errors.New("the image is empty")
	}
	rgb := make([]byte, 0, r.Dx()*r.Dy()*3)
	alpha := make([]byte, 0, r.Dx()*r.Dy())
	gray, opaque := true, true
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c := color.NRGBAModel.Convert(m.At(x, y)).(color.NRGBA)
			if c.A == 0 {
				// Often black, which readers blend into the edges when
				// they scale the image: white, like the paper, instead.
				c.R, c.G, c.B = 255, 255, 255
			}
			rgb = append(rgb, c.R, c.G, c.B)
			alpha = append(alpha, c.A)
			gray = gray && c.R == c.G && c.G == c.B
			opaque = opaque && c.A == 255
		}
	}
	cs := pdfName("DeviceRGB")
	if gray {
		cs = "DeviceGray"
		for i := range alpha {
			rgb[i] = rgb[3*i]
		}
		rgb = rgb[:len(alpha)]
	}
	dict := func(cs pdfName) pdfDict {
		return pdfDict{"Type": pdfName("XObject"), "Subtype": pdfName("Image"), "Filter": pdfName("FlateDecode"),
			"Width": int64(r.Dx()), "Height": int64(r.Dy()), "ColorSpace": cs, "BitsPerComponent": int64(8)}
	}
	img = flateStream(rgb)
	img.Dict = dict(cs)
	if !opaque {
		mask = flateStream(alpha)
		mask.Dict = dict("DeviceGray")
	}
	return img, mask, nil
}

func (w *pdfWriter) resourceEntry(p *pdfPage, key pdfName) any {
	if p == nil || w.src == nil {
		return nil
	}
	if r, ok := w.src.resolve(p.resources).(pdfDict); ok {
		return r[key]
	}
	return nil
}

func boxArray(b [4]float64) pdfArray {
	return pdfArray{b[0], b[1], b[2], b[3]}
}

func pdfSerialize(b *bytes.Buffer, v any) {
	switch v := v.(type) {
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case int64:
		b.WriteString(strconv.FormatInt(v, 10))
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1e15 {
			b.WriteString(strconv.FormatInt(int64(v), 10))
		} else {
			b.WriteString(pdfNum(v))
		}
	case pdfName:
		b.WriteByte('/')
		for _, c := range []byte(v) {
			if c < 33 || c > 126 || c == '#' || pdfDelim(c) {
				fmt.Fprintf(b, "#%02X", c)
			} else {
				b.WriteByte(c)
			}
		}
	case pdfStr:
		fmt.Fprintf(b, "<%X>", []byte(v))
	case pdfRef:
		fmt.Fprintf(b, "%d %d R", v.Num, v.Gen)
	case pdfArray:
		b.WriteByte('[')
		for i, x := range v {
			if i > 0 {
				b.WriteByte(' ')
			}
			pdfSerialize(b, x)
		}
		b.WriteByte(']')
	case pdfDict:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, string(k))
		}
		sort.Strings(keys)
		b.WriteString("<<")
		for _, k := range keys {
			pdfSerialize(b, pdfName(k))
			b.WriteByte(' ')
			pdfSerialize(b, v[pdfName(k)])
			b.WriteByte(' ')
		}
		b.WriteString(">>")
	case *pdfStream:
		d := pdfDict{}
		for k, x := range v.Dict {
			d[k] = x
		}
		d["Length"] = int64(len(v.Data))
		delete(d, "DL")
		pdfSerialize(b, d)
		b.WriteString("\nstream\n")
		b.Write(v.Data)
		b.WriteString("\nendstream")
	default: // nil, and keywords where a value should be
		b.WriteString("null")
	}
}
