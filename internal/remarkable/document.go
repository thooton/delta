// Package remarkable reads the documents of the user's reMarkable tablet and
// draws them as PDF, looking as they do on the tablet. The command center and
// the transform CLI both use it.
package remarkable

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Rendered is a document drawn as it looks on the tablet.
type Rendered struct {
	Type string // "pdf", "epub" or "notebook"
	// Original is the PDF or EPUB as uploaded; nil for a notebook.
	Original []byte
	// PDF is the document as on the tablet: a PDF's own pages, in the
	// tablet's order, with what was written on them and the pages added
	// between them, or a notebook's pages. When Separate, it is the
	// handwriting alone, on pages of its own labelled with where each is: so
	// it is for an EPUB, which the tablet lays out itself, and for a PDF
	// that couldn't be read (Unreadable says why). Nil when nothing is
	// written on those.
	PDF      []byte
	Separate bool
	Pages    int // in PDF
	Written  int // pages with something written on them
	// Unreadable is why the PDF couldn't be drawn on.
	Unreadable error
	// Problems are pages that couldn't be read, in words.
	Problems []string
}

// ReadZip unpacks a document's files, as the command center sends them.
func ReadZip(data []byte) (map[string][]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		r, err := f.Open()
		if err != nil {
			continue
		}
		b, err := io.ReadAll(r)
		r.Close()
		if err == nil {
			files[f.Name] = b
		}
	}
	return files, nil
}

// Render draws document id from its files, by name as the tablet keeps them
// (<id>.content, <id>.pdf, <id>/<page>.rm, ...).
func Render(id string, files map[string][]byte) *Rendered {
	return render(id, files, 0)
}

// RenderPage draws page n (from 1) of what Render would draw, alone, for
// showing one page at a time. Pages still counts them all; PDF is nil when
// there is no page n.
func RenderPage(id string, files map[string][]byte, n int) *Rendered {
	if n < 1 {
		return &Rendered{}
	}
	return render(id, files, n)
}

// render draws every page when only is 0, otherwise page only alone.
func render(id string, files map[string][]byte, only int) *Rendered {
	var content rmContent
	json.Unmarshal(files[id+".content"], &content)
	out := &Rendered{Type: "notebook"}
	pdfData, isPDF := files[id+".pdf"]
	epubData, isEPUB := files[id+".epub"]
	switch {
	case isPDF:
		out.Type, out.Original = "pdf", pdfData
	case isEPUB:
		out.Type, out.Original = "epub", epubData
	}

	// The tablet's pages, in order, with what is written on each.
	refs := content.pageOrder()
	var src *pdfFile
	var srcPages []pdfPage
	if isPDF {
		var err error
		if src, err = readPDF(pdfData); err != nil {
			src, out.Unreadable = nil, err
		} else {
			srcPages = src.pages()
			if len(refs) == 0 { // never opened on the tablet
				for i := range srcPages {
					refs = append(refs, rmPageRef{redir: i})
				}
			}
		}
	}
	out.Separate = isEPUB || isPDF && src == nil
	scale := pointsPerPixel
	if len(srcPages) > 0 {
		scale = rmScale(srcPages[0].shownSize())
	}
	var pages []outPage
	for i, ref := range refs {
		page := &rmPage{}
		if data, ok := files[id+"/"+ref.id+".rm"]; ok {
			p, err := parseRM(data)
			if err != nil {
				out.Problems = append(out.Problems, fmt.Sprintf("Couldn't read page %d: %v.", i+1, err))
			} else {
				page = p
			}
		}
		images := page.Images[:0]
		for _, im := range page.Images {
			if im.Data = files[id+"/"+ref.id+"/"+im.File]; im.Data != nil {
				images = append(images, im)
			}
		}
		page.Images = images
		hasInk := page.written()
		if hasInk {
			out.Written++
		}
		switch {
		case src != nil && ref.redir >= 0 && ref.redir < len(srcPages):
			sp := &srcPages[ref.redir]
			scale = rmScale(sp.shownSize())
			o := outPage{src: sp}
			if hasInk {
				o.ink = inkOps(page, sp.rmTransform())
			}
			pages = append(pages, o)
		case !out.Separate:
			// A notebook's page, or one added to a PDF on the tablet, as
			// wide as the PDF's pages around it.
			pages = append(pages, notebookPage(page, scale))
		case hasInk:
			// The handwriting alone, labelled with where it is.
			o := notebookPage(page, pointsPerPixel)
			kind := strings.ToUpper(out.Type)
			if ref.redir >= 0 {
				o.label = fmt.Sprintf("Handwriting on page %d of the %s", ref.redir+1, kind)
			} else {
				o.label = fmt.Sprintf("A page added after page %d of the %s", lastRedir(refs[:i])+1, kind)
			}
			pages = append(pages, o)
		}
	}
	if len(pages) == 0 && out.Type == "notebook" {
		pages = append(pages, notebookPage(&rmPage{}, pointsPerPixel)) // a notebook never opened
	}
	out.Pages = len(pages)
	switch {
	case len(pages) == 0:
	case only == 0:
		out.PDF = writePDF(src, pages)
	case only <= len(pages):
		out.PDF = writePDF(src, pages[only-1:only])
	}
	return out
}

// SafeFileName makes a document's name usable as a file name.
func SafeFileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) || r < 32 {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		name = "document"
	}
	return name
}

// rmContent is the part of a document's .content file that orders its
// pages: cPages in firmware 3, pages and redirectionPageMap before.
type rmContent struct {
	Pages    []string `json:"pages"`
	Redirect []int    `json:"redirectionPageMap"`
	CPages   struct {
		Pages []struct {
			ID  string `json:"id"`
			Idx struct {
				Value string `json:"value"`
			} `json:"idx"`
			Redir *struct {
				Value int `json:"value"`
			} `json:"redir"`
			Deleted *struct {
				Value int `json:"value"`
			} `json:"deleted"`
		} `json:"pages"`
	} `json:"cPages"`
}

type rmPageRef struct {
	id    string
	redir int // the page of the PDF or EPUB it is on; -1 for an added page
}

// pageOrder lists a document's pages in order.
func (c *rmContent) pageOrder() []rmPageRef {
	var out []rmPageRef
	if len(c.CPages.Pages) > 0 {
		pages := c.CPages.Pages
		sort.SliceStable(pages, func(i, j int) bool { return pages[i].Idx.Value < pages[j].Idx.Value })
		for _, p := range pages {
			if p.Deleted != nil && p.Deleted.Value != 0 {
				continue
			}
			r := -1
			if p.Redir != nil {
				r = p.Redir.Value
			}
			out = append(out, rmPageRef{p.ID, r})
		}
		return out
	}
	for i, id := range c.Pages {
		r := i
		if i < len(c.Redirect) {
			r = c.Redirect[i]
		}
		out = append(out, rmPageRef{id, r})
	}
	return out
}

func lastRedir(refs []rmPageRef) int {
	for i := len(refs) - 1; i >= 0; i-- {
		if refs[i].redir >= 0 {
			return refs[i].redir
		}
	}
	return -1
}
