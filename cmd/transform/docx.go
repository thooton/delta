package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image"
	"image/png"
	"sort"
	"strings"
	"time"
)

// makeDocx lays the form out as a Word document, by its format's fixed
// template: US Letter, 1-inch margins, and for MLA and APA everything their
// student papers ask for. figure gives a figure's picture, cropped from its
// page.
func makeDocx(f *form, now time.Time, figure func(page int, box []float64) (image.Image, error)) ([]byte, error) {
	w := &docWriter{f: f, figure: figure}
	switch f.Format {
	case "mla":
		w.style = docStyle{font: "Times New Roman", size: 24, line: 480, indent: 720}
		last := "[Last name]"
		if a := strings.Fields(f.Author); len(a) > 0 {
			last = a[len(a)-1]
		}
		w.header = last + " "
		for _, line := range []string{placeholder(f.Author, "Name"), placeholder(f.Instructor, "Instructor"), placeholder(f.Course, "Course"), f.date(now)} {
			w.para("", inline(line, ""))
		}
		w.para(`<w:jc w:val="center"/>`, inline(placeholder(f.Heading, "Title"), ""))
	case "apa":
		w.style = docStyle{font: "Times New Roman", size: 24, line: 480, indent: 720}
		w.header = ""
		title := placeholder(f.Heading, "Title")
		for range 3 {
			w.para("", "")
		}
		w.para(`<w:jc w:val="center"/>`, inline(title, "<w:b/>"))
		w.para("", "")
		for _, line := range []string{placeholder(f.Author, "Name"), placeholder(f.School, "School"), placeholder(f.Course, "Course"), placeholder(f.Instructor, "Instructor"), f.date(now)} {
			w.para(`<w:jc w:val="center"/>`, inline(line, ""))
		}
		w.pageBreak()
		w.para(`<w:jc w:val="center"/>`, inline(title, "<w:b/>"))
	default:
		w.style = docStyle{font: "Times New Roman", size: 24, line: 276, after: 160}
		if f.Font == "sans" {
			w.style.font, w.style.size = "Arial", 22
		}
		w.footer = true
		if h := strings.TrimSpace(f.Heading); h != "" {
			w.para(`<w:spacing w:after="240"/>`, inline(h, `<w:b/><w:sz w:val="40"/>`))
		}
		var by []string
		for _, s := range []string{f.Author, f.Course, f.Instructor, strings.TrimSpace(f.Date)} {
			if s = strings.TrimSpace(s); s != "" {
				by = append(by, s)
			}
		}
		if len(by) > 0 {
			w.para("", inline(strings.Join(by, " · "), "<w:i/>"))
		}
	}
	for _, b := range f.Blocks {
		if err := w.block(b); err != nil {
			return nil, err
		}
	}
	if len(f.Sources) > 0 {
		w.pageBreak()
		name, props := "Works Cited", ""
		if f.Format == "apa" {
			name, props = "References", "<w:b/>"
		}
		w.para(`<w:jc w:val="center"/>`, inline(name, props))
		sources := append([]string(nil), f.Sources...)
		sort.SliceStable(sources, func(i, j int) bool {
			return sortKey(sources[i]) < sortKey(sources[j])
		})
		for _, s := range sources {
			w.para(`<w:ind w:left="720" w:hanging="720"/>`, inline(s, ""))
		}
	}
	return w.zip()
}

type docStyle struct {
	font   string
	size   int // half-points
	line   int // 240ths of a line
	after  int // twentieths of a point
	indent int // a body paragraph's first line, in twentieths of a point
}

type docWriter struct {
	f       *form
	figure  func(page int, box []float64) (image.Image, error)
	style   docStyle
	header  string // text before the page number at the top right; "" for the number alone
	footer  bool   // the page number at the bottom, centered, instead
	body    strings.Builder
	lists   []bool // each list's numbering: ordered or not
	images  [][]byte
	figures int
}

func (w *docWriter) para(props, runs string) {
	w.body.WriteString("<w:p>")
	if props != "" {
		w.body.WriteString("<w:pPr>" + props + "</w:pPr>")
	}
	w.body.WriteString(runs + "</w:p>")
}

func (w *docWriter) pageBreak() {
	w.para("", `<w:r><w:br w:type="page"/></w:r>`)
}

func (w *docWriter) block(b block) error {
	switch b.Type {
	case "paragraph":
		ind := ""
		if w.style.indent > 0 {
			ind = fmt.Sprintf(`<w:ind w:firstLine="%d"/>`, w.style.indent)
		}
		w.para(ind, inline(b.Text, ""))
	case "heading":
		w.heading(b.Level, b.Text)
	case "quote":
		props := ""
		if w.f.Format == "plain" {
			props = "<w:i/>"
		}
		w.para(`<w:ind w:left="720"/>`, inline(b.Text, props))
	case "list":
		w.lists = append(w.lists, b.Ordered)
		w.listItems(len(w.lists), 0, b.Items)
	case "table":
		w.table(b)
	case "figure":
		return w.picture(b)
	case "page_break":
		w.pageBreak()
	}
	return nil
}

func (w *docWriter) heading(level int, text string) {
	outline := fmt.Sprintf(`<w:keepNext/><w:outlineLvl w:val="%d"/>`, level-1)
	switch w.f.Format {
	case "apa": // APA 7's levels
		switch level {
		case 1:
			w.para(outline+`<w:jc w:val="center"/>`, inline(text, "<w:b/>"))
		case 2:
			w.para(outline, inline(text, "<w:b/>"))
		default:
			w.para(outline, inline(text, "<w:b/><w:i/>"))
		}
	case "mla":
		props := map[int]string{1: "<w:b/>", 2: "<w:i/>", 3: "<w:b/><w:i/>"}[level]
		w.para(outline, inline(text, props))
	default:
		size := map[int]int{1: 32, 2: 26, 3: w.style.size}[level]
		w.para(outline+`<w:spacing w:before="240" w:after="120"/>`, inline(text, fmt.Sprintf(`<w:b/><w:sz w:val="%d"/>`, size)))
	}
}

func (w *docWriter) listItems(num, level int, items []listItem) {
	for _, it := range items {
		w.para(fmt.Sprintf(`<w:numPr><w:ilvl w:val="%d"/><w:numId w:val="%d"/></w:numPr>`, level, num), inline(it.Text, ""))
		w.listItems(num, level+1, it.Items)
	}
}

func (w *docWriter) table(b block) {
	cols := 0
	for _, r := range b.Rows {
		cols = max(cols, len(r))
	}
	width := 9360 / cols
	cell := `<w:spacing w:line="240" w:lineRule="auto" w:after="0"/>`
	w.body.WriteString(`<w:tbl><w:tblPr><w:tblW w:w="9360" w:type="dxa"/><w:tblBorders>`)
	for _, side := range []string{"top", "left", "bottom", "right", "insideH", "insideV"} {
		w.body.WriteString(`<w:` + side + ` w:val="single" w:sz="4" w:space="0" w:color="000000"/>`)
	}
	w.body.WriteString(`</w:tblBorders><w:tblCellMar><w:left w:w="100" w:type="dxa"/><w:right w:w="100" w:type="dxa"/></w:tblCellMar></w:tblPr><w:tblGrid>`)
	for range cols {
		fmt.Fprintf(&w.body, `<w:gridCol w:w="%d"/>`, width)
	}
	w.body.WriteString(`</w:tblGrid>`)
	for i, r := range b.Rows {
		head := b.Header && i == 0
		w.body.WriteString("<w:tr>")
		if head {
			w.body.WriteString("<w:trPr><w:tblHeader/></w:trPr>")
		}
		for c := range cols {
			text := ""
			if c < len(r) {
				text = r[c]
			}
			props := ""
			if head {
				props = "<w:b/>"
			}
			fmt.Fprintf(&w.body, `<w:tc><w:tcPr><w:tcW w:w="%d" w:type="dxa"/></w:tcPr>`, width)
			w.para(cell, inline(text, props))
			w.body.WriteString("</w:tc>")
		}
		w.body.WriteString("</w:tr>")
	}
	w.body.WriteString("</w:tbl>")
	w.para(cell, "") // Word needs a paragraph after a table
}

// picture crops a figure from its page and puts it in, centered, with its
// caption: "Figure n" and the caption above it for APA, "Fig. n." and the
// caption under it otherwise.
func (w *docWriter) picture(b block) error {
	img, err := w.figure(b.Page, b.Box)
	if err != nil {
		return fmt.Errorf("the figure on page %d: %w", b.Page, err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	w.images = append(w.images, buf.Bytes())
	w.figures++
	n, id := w.figures, len(w.images)
	// Pages are rendered at renderDPI; fit in 6.5 by 7 inches.
	const emu = 914400
	px := img.Bounds().Size()
	cx, cy := float64(px.X)/renderDPI*emu, float64(px.Y)/renderDPI*emu
	if s := min(1, 6.5*emu/cx, 7*emu/cy); s < 1 {
		cx, cy = cx*s, cy*s
	}
	center := `<w:jc w:val="center"/>`
	caption := strings.TrimSpace(b.Caption)
	if w.f.Format == "apa" {
		w.para(`<w:keepNext/>`, inline(fmt.Sprintf("Figure %d", n), "<w:b/>"))
		if caption != "" {
			w.para(`<w:keepNext/>`, inline(caption, "<w:i/>"))
		}
	} else if caption != "" {
		center += `<w:keepNext/>` // with its caption
	}
	w.para(center, fmt.Sprintf(`<w:r><w:drawing><wp:inline distT="0" distB="0" distL="0" distR="0"><wp:extent cx="%d" cy="%d"/><wp:docPr id="%d" name="Figure %d"/><a:graphic xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:pic xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:nvPicPr><pic:cNvPr id="%d" name="figure%d.png"/><pic:cNvPicPr/></pic:nvPicPr><pic:blipFill><a:blip r:embed="rImg%d"/><a:stretch><a:fillRect/></a:stretch></pic:blipFill><pic:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="%d" cy="%d"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></pic:spPr></pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r>`,
		int(cx), int(cy), id, n, id, id, id, int(cx), int(cy)))
	if w.f.Format != "apa" && caption != "" {
		label := fmt.Sprintf("Fig. %d. ", n)
		if w.f.Format == "plain" {
			label = ""
		}
		w.para(`<w:jc w:val="center"/>`, inline(label+caption, ""))
	}
	return nil
}

// inline turns text into runs with props: **bold**, *italic*, \* for an
// asterisk, and a line break for each newline.
func inline(text, props string) string {
	var out, cur strings.Builder
	bold, italic := false, false
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		p := props
		if bold && !strings.Contains(p, "<w:b/>") {
			p += "<w:b/>"
		}
		if italic && !strings.Contains(p, "<w:i/>") {
			p += "<w:i/>"
		}
		for i, line := range strings.Split(cur.String(), "\n") {
			out.WriteString("<w:r>")
			if p != "" {
				out.WriteString("<w:rPr>" + p + "</w:rPr>")
			}
			if i > 0 {
				out.WriteString("<w:br/>")
			}
			out.WriteString(`<w:t xml:space="preserve">` + xmlText(line) + "</w:t></w:r>")
		}
		cur.Reset()
	}
	r := []rune(text)
	for i := 0; i < len(r); i++ {
		switch {
		case r[i] == '\\' && i+1 < len(r) && r[i+1] == '*':
			cur.WriteRune('*')
			i++
		case r[i] == '*' && i+1 < len(r) && r[i+1] == '*':
			flush()
			bold = !bold
			i++
		case r[i] == '*':
			flush()
			italic = !italic
		default:
			cur.WriteRune(r[i])
		}
	}
	flush()
	return out.String()
}

// sortKey orders works cited and references: by their text, without
// markup, quotation marks or case.
func sortKey(s string) string {
	return strings.ToLower(strings.TrimLeft(strings.NewReplacer("*", "", "\\", "").Replace(s), "\"“‘' "))
}

func xmlText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '\t':
			b.WriteString(" ")
		case r < 0x20 || r == 0xFFFE || r == 0xFFFF:
			// not allowed in XML
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (w *docWriter) zip() ([]byte, error) {
	s := w.style
	const ns = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing"`
	pageNum := func(jc, before string) string {
		return `<w:p><w:pPr><w:jc w:val="` + jc + `"/><w:spacing w:line="240" w:lineRule="auto" w:after="0"/></w:pPr>` + inline(before, "") +
			`<w:fldSimple w:instr=" PAGE "><w:r><w:t>1</w:t></w:r></w:fldSimple></w:p>`
	}
	files := map[string]string{}
	rels := `<Relationship Id="rStyles" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>` +
		`<Relationship Id="rSettings" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/settings" Target="settings.xml"/>`
	types := `<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>` +
		`<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>` +
		`<Override PartName="/word/settings.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.settings+xml"/>`
	sect := ""
	if w.footer {
		files["word/footer1.xml"] = `<w:ftr ` + ns + `>` + pageNum("center", "") + `</w:ftr>`
		rels += `<Relationship Id="rFooter" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/footer" Target="footer1.xml"/>`
		types += `<Override PartName="/word/footer1.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.footer+xml"/>`
		sect += `<w:footerReference w:type="default" r:id="rFooter"/>`
	} else {
		files["word/header1.xml"] = `<w:hdr ` + ns + `>` + pageNum("right", w.header) + `</w:hdr>`
		rels += `<Relationship Id="rHeader" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/header" Target="header1.xml"/>`
		types += `<Override PartName="/word/header1.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.header+xml"/>`
		sect += `<w:headerReference w:type="default" r:id="rHeader"/>`
	}
	if len(w.lists) > 0 {
		var n strings.Builder
		n.WriteString(`<w:numbering ` + ns + `>`)
		bullets := []string{"•", "◦", "▪"}
		ordered := []string{"decimal", "lowerLetter", "lowerRoman"}
		for a := range 2 {
			fmt.Fprintf(&n, `<w:abstractNum w:abstractNumId="%d"><w:multiLevelType w:val="hybridMultilevel"/>`, a)
			for l := range 3 {
				format, text := "bullet", bullets[l]
				if a == 1 {
					format, text = ordered[l], fmt.Sprintf("%%%d.", l+1)
				}
				fmt.Fprintf(&n, `<w:lvl w:ilvl="%d"><w:start w:val="1"/><w:numFmt w:val="%s"/><w:lvlText w:val="%s"/><w:lvlJc w:val="left"/><w:pPr><w:ind w:left="%d" w:hanging="360"/></w:pPr></w:lvl>`,
					l, format, text, 720*(l+1))
			}
			n.WriteString(`</w:abstractNum>`)
		}
		for i, o := range w.lists {
			a := 0
			if o {
				a = 1
			}
			fmt.Fprintf(&n, `<w:num w:numId="%d"><w:abstractNumId w:val="%d"/><w:lvlOverride w:ilvl="0"><w:startOverride w:val="1"/></w:lvlOverride></w:num>`, i+1, a)
		}
		n.WriteString(`</w:numbering>`)
		files["word/numbering.xml"] = n.String()
		rels += `<Relationship Id="rNumbering" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/numbering" Target="numbering.xml"/>`
		types += `<Override PartName="/word/numbering.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.numbering+xml"/>`
	}
	for i, img := range w.images {
		files[fmt.Sprintf("word/media/figure%d.png", i+1)] = string(img)
		rels += fmt.Sprintf(`<Relationship Id="rImg%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="media/figure%d.png"/>`, i+1, i+1)
	}
	files["[Content_Types].xml"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Default Extension="png" ContentType="image/png"/>` + types + `</Types>`
	files["_rels/.rels"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rDoc" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`
	files["word/_rels/document.xml.rels"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + rels + `</Relationships>`
	files["word/settings.xml"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:settings xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:defaultTabStop w:val="720"/><w:compat><w:compatSetting w:name="compatibilityMode" w:uri="http://schemas.microsoft.com/office/word" w:val="15"/></w:compat></w:settings>`
	files["word/styles.xml"] = fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="%[1]s" w:hAnsi="%[1]s" w:eastAsia="%[1]s" w:cs="%[1]s"/><w:sz w:val="%[2]d"/><w:szCs w:val="%[2]d"/><w:lang w:val="en-US"/></w:rPr></w:rPrDefault><w:pPrDefault><w:pPr><w:spacing w:before="0" w:after="%[3]d" w:line="%[4]d" w:lineRule="auto"/></w:pPr></w:pPrDefault></w:docDefaults><w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/><w:qFormat/></w:style></w:styles>`,
		s.font, s.size, s.after, s.line)
	files["word/document.xml"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document ` + ns + `><w:body>` + w.body.String() +
		`<w:sectPr>` + sect + `<w:pgSz w:w="12240" w:h="15840"/><w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440" w:header="720" w:footer="720" w:gutter="0"/></w:sectPr></w:body></w:document>`

	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { // [Content_Types].xml first, as Word likes
		return names[i] == "[Content_Types].xml" || names[j] != "[Content_Types].xml" && names[i] < names[j]
	})
	for _, name := range names {
		fw, err := z.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := fw.Write([]byte(files[name])); err != nil {
			return nil, err
		}
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
