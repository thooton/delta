package remarkable

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"strings"
	"testing"
)

// rmBlock builds page file values as the tablet writes them.
type rmBlock struct{ bytes.Buffer }

func (b *rmBlock) varuint(v uint64) {
	for v >= 0x80 {
		b.WriteByte(byte(v) | 0x80)
		v >>= 7
	}
	b.WriteByte(byte(v))
}
func (b *rmBlock) tag(i, kind int) { b.varuint(uint64(i<<4 | kind)) }
func (b *rmBlock) id(i int, a uint8, n uint64) {
	b.tag(i, tagID)
	b.WriteByte(a)
	b.varuint(n)
}
func (b *rmBlock) u32(v uint32) { binary.Write(b, binary.LittleEndian, v) }
func (b *rmBlock) f32(v float64) {
	binary.Write(b, binary.LittleEndian, math.Float32bits(float32(v)))
}
func (b *rmBlock) sub(i int, body []byte) {
	b.tag(i, tagSub)
	b.u32(uint32(len(body)))
	b.Write(body)
}

func rmFile(blocks ...[2]any) []byte {
	var out bytes.Buffer
	out.WriteString(rmHeaderV6)
	out.WriteString(strings.Repeat(" ", 43-len(rmHeaderV6)))
	for _, bl := range blocks {
		body := bl[1].([]byte)
		binary.Write(&out, binary.LittleEndian, uint32(len(body)))
		out.Write([]byte{0, 1, 1, bl[0].(byte)})
		out.Write(body)
	}
	return out.Bytes()
}

// A page with an image pasted onto it, as firmware 3 writes one: the table
// naming its file (block 0x0E) and the item drawing it on a quad (0x0F).
func imagePage(file string) []byte {
	key := [16]byte{0xdc, 0xa6, 0x52, 0xb7}
	var name, lww, entry, table, files rmBlock
	name.varuint(uint64(len(file)))
	name.WriteByte(1)
	name.WriteString(file)
	lww.id(1, 1, 15)
	lww.sub(2, name.Bytes())
	entry.Write(key[:])
	entry.sub(1, lww.Bytes())
	table.varuint(1)
	table.sub(0, entry.Bytes())
	files.sub(1, table.Bytes())

	var ref, refValue, verts, idx, value, item rmBlock
	ref.Write(key[:])
	refValue.id(1, 1, 18)
	refValue.sub(2, ref.Bytes())
	verts.varuint(16)
	for _, v := range [][4]float64{{-500, 200, 0, 0}, {500, 200, 1, 0}, {500, 700, 1, 1}, {-500, 700, 0, 1}} {
		for _, f := range v {
			verts.f32(f)
		}
	}
	idx.varuint(6)
	for _, i := range []uint32{0, 1, 2, 2, 3, 0} {
		idx.u32(i)
	}
	value.WriteByte(7)
	value.sub(1, refValue.Bytes())
	value.id(2, 1, 17)
	value.sub(3, verts.Bytes())
	value.sub(4, idx.Bytes())
	item.id(1, 0, 11)
	item.id(2, 1, 18)
	item.id(3, 1, 17)
	item.id(4, 0, 0)
	item.tag(5, tagByte4)
	item.u32(0)
	item.sub(6, value.Bytes())
	return rmFile([2]any{byte(0x0E), files.Bytes()}, [2]any{byte(0x0F), item.Bytes()})
}

func TestImages(t *testing.T) {
	page, err := parseRM(imagePage("pic.png"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Images) != 1 || page.Images[0].File != "pic.png" || len(page.Images[0].Vertices) != 4 || len(page.Images[0].Indices) != 6 {
		t.Fatalf("images: got %+v", page.Images)
	}

	// Drawn under the transform its corners give it: u across 1000 pixels,
	// v down 500, so the PDF's unit square is flipped to put v = 0 on top.
	var b bytes.Buffer
	if !imageOps(&b, page.Images[0], "Im") {
		t.Fatal("imageOps refused a quad")
	}
	if !strings.Contains(b.String(), "1000 0 0 -500 -500 700 cm /Im Do") {
		t.Errorf("image transform: got %q", b.String())
	}

	// A notebook page with it comes out with the image, its transparency as
	// a soft mask.
	m := image.NewNRGBA(image.Rect(0, 0, 4, 2))
	m.Set(0, 0, color.NRGBA{200, 10, 10, 255})
	var pngData bytes.Buffer
	png.Encode(&pngData, m)
	files := map[string][]byte{
		"doc.content":     []byte(`{"fileType":"notebook","cPages":{"pages":[{"id":"p1","idx":{"value":"ba"}}]}}`),
		"doc/p1.rm":       imagePage("pic.png"),
		"doc/p1/pic.png":  pngData.Bytes(),
		"doc/p1/../x.png": []byte("not reached"),
	}
	out := Render("doc", files)
	if out.Written != 1 || len(out.Problems) > 0 {
		t.Fatalf("render: written %d, problems %v", out.Written, out.Problems)
	}
	pdf, err := readPDF(out.PDF)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := pdf.resolve(pdf.pages()[0].resources).(pdfDict)
	xobjs, _ := pdf.resolve(res["XObject"]).(pdfDict)
	img, _ := pdf.resolve(xobjs[pdfImagePrefix+"1"]).(*pdfStream)
	if img == nil || img.Dict["Width"] != int64(4) || img.Dict["ColorSpace"] != pdfName("DeviceRGB") || img.Dict["SMask"] == nil {
		t.Fatalf("image XObject: got %v", img)
	}

	// A file name that would leave the page's folder is not read.
	if page, _ := parseRM(imagePage("../x.png")); len(page.Images) != 0 {
		t.Errorf("a name with a slash was kept: %+v", page.Images)
	}
}
