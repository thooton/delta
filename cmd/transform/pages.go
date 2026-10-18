package main

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
)

// renderDPI is what pages are drawn at, for the agent to read and for
// figures to be cropped from.
const renderDPI = 150

// renderPages draws each page of a PDF as dir/page-N.png, with pdftoppm or
// else PyMuPDF (which the session's setup installs), and returns how many.
func renderPages(pdf, dir string) (int, error) {
	if err := os.RemoveAll(dir); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	var out []byte
	var err error
	if _, e := exec.LookPath("pdftoppm"); e == nil {
		out, err = exec.Command("pdftoppm", "-r", strconv.Itoa(renderDPI), "-png", pdf, filepath.Join(dir, "page")).CombinedOutput()
	} else {
		const script = `import sys, fitz
for i, page in enumerate(fitz.open(sys.argv[1])):
    page.get_pixmap(dpi=int(sys.argv[3])).save(f"{sys.argv[2]}/page-{i + 1}.png")`
		out, err = exec.Command("python3", "-c", script, pdf, dir, strconv.Itoa(renderDPI)).CombinedOutput()
	}
	if err != nil {
		return 0, fmt.Errorf("drawing the pages of %s: %v: %s", pdf, err, out)
	}
	// pdftoppm pads the numbers (page-01.png) when there are 10 or more.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	num := regexp.MustCompile(`^page-0*(\d+)\.png$`)
	for _, e := range entries {
		if m := num.FindStringSubmatch(e.Name()); m != nil {
			want := "page-" + m[1] + ".png"
			if want != e.Name() {
				if err := os.Rename(filepath.Join(dir, e.Name()), filepath.Join(dir, want)); err != nil {
					return 0, err
				}
			}
		}
	}
	return len(entries), nil
}

// cropPage cuts box (left, top, right, bottom, as fractions) out of
// dir/page-N.png.
func cropPage(dir string, page int, box []float64) (image.Image, error) {
	f, err := os.Open(filepath.Join(dir, fmt.Sprintf("page-%d.png", page)))
	if err != nil {
		return nil, fmt.Errorf("there is no page %d", page)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	at := func(frac float64, lo, size int) int { return lo + int(frac*float64(size)+0.5) }
	r := image.Rect(at(box[0], b.Min.X, b.Dx()), at(box[1], b.Min.Y, b.Dy()), at(box[2], b.Min.X, b.Dx()), at(box[3], b.Min.Y, b.Dy())).Intersect(b)
	if r.Dx() < 4 || r.Dy() < 4 {
		return nil, fmt.Errorf("the box %v is too small", box)
	}
	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok {
		return nil, fmt.Errorf("page %d can't be cropped", page)
	}
	return sub.SubImage(r), nil
}
