# Creating Documents with `docx-js`

The `scripts/` used below come bundled with this CLI. Extract them first, then run the commands from that directory:

```bash
delta docx extract [dir]   # writes scripts/ and its LICENSE into dir (default: the current directory)
pip install defusedxml lxml   # needed by the scripts
```

To create documents, write a script using the `docx` package (`npm install docx` / `require('docx')`).

### Key Guidelines & Gotchas

* **Page Size:** Defaults to A4. For US Letter, explicitly set: `page: { size: { width: 12240, height: 15840 } }` (DXA units; 1440 = 1″).
* **Landscape Orientation:** Pass portrait dimensions alongside `orientation: PageOrientation.LANDSCAPE` (the library handles swapping dimensions internally).
* **Tables:** 
  * Set `columnWidths` on the table and a matching `width` on every cell using `WidthType.DXA` (avoid percentages, as they break in Google Docs).
  * Column widths must sum up to the total table width.
  * Use `ShadingType.CLEAR` for table shading; never use `SOLID` (which renders black).
* **Lists:** Never insert bullet characters (`•`) literally. Configure bullet styling via `numbering` with `LevelFormat.BULLET`.
* **Images:** `ImageRun` requires an explicit file `type` (e.g., `"png"`, `"jpg"`).
* **Page Breaks:** `PageBreak` elements must be nested inside a `Paragraph`.
* **Line Breaks:** Avoid using `\n`. Instead, use separate `Paragraph` elements.
* **Table of Contents:** Headings must use built-in `HeadingLevel.*` types. Custom heading styles require an explicit `outlineLevel` to appear in the TOC.
* **Horizontal Rules:** Avoid using tables as horizontal rules; use paragraph bottom borders instead.
* **Dot-Leaders / Right-Alignment:** Use `PositionalTab` (`alignment: PositionalTabAlignment.RIGHT`, `leader: PositionalTabLeader.DOT`) inside a `TextRun` rather than literal dots or spaces.

---

## Verifying Output

After generating a `.docx` file, render it to review visual formatting:

```bash
python scripts/office/soffice.py --headless --convert-to pdf output.docx
pdftoppm -jpeg -r 100 output.pdf page
ls page-*.jpg   # Review the generated images
```

*(Note: `pdftoppm` automatically zero-pads page numbers based on the total page count, e.g., `page-01.jpg` … `page-12.jpg`).*

---

## Editing Existing Documents

Legacy `.doc` files must be converted before processing:
`python scripts/office/soffice.py --headless --convert-to docx file.doc`

```bash
unzip -q doc.docx -d unpacked/
find unpacked -type l -delete   # Strip symlink entries for security when handling external files
python scripts/merge_runs.py unpacked/   # Coalesce fragmented runs so text strings are searchable
# Edit unpacked/word/document.xml in place — do NOT reformat or pretty-print
(cd unpacked && rm -f ../out.docx && zip -Xr ../out.docx .)
python scripts/office/validate.py out.docx --original doc.docx   # XSD validation; use --auto-repair to fix common issues
```

### Working with Text Runs

Word splits text across multiple `<w:r>` runs (due to revision IDs, spell-check markers, etc.), meaning visible phrases may not exist as contiguous strings in raw XML. `merge_runs.py` resolves this safely without changing content or rendering. It also accepts a `.docx` directly: `python scripts/merge_runs.py doc.docx -o merged.docx`.

---

## Dependencies

* `docx` (npm package)
* `pandoc`
* LibreOffice (`soffice`)
* `pdftoppm` (Poppler utilities)
* Python with `defusedxml` and `lxml` (for the scripts)
