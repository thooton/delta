package main

const remarkableHelp = `delta remarkable: the user's reMarkable tablet

  The user has a reMarkable, an e-ink tablet for reading and handwriting.
  Through this you can put PDFs and EPUBs on it, organize it, and get
  back what they wrote: their notebooks, and the PDFs they wrote on (a
  worksheet they filled in, say). A typical job: download an assignment
  from Canvas, upload it here, the user does it by hand on the tablet,
  then you get it back and read or submit their work.

COMMANDS
  ls [folder]                List everything, as a tree: name, type, page
                             count, last change and id. With a folder,
                             just what is in it.
    --trash                  List the trash instead.
  upload <file> [folder]     Put a PDF or EPUB on the tablet, in a folder
                             (default: the top level). It is named after
                             the file, without its extension.
    --name NAME              Name it something else.
  get <document> [dir]       Save a document into dir (default: here):
                             - a notebook as "<name>.pdf", one page per
                               page;
                             - a PDF or EPUB as "<name>.pdf" or ".epub",
                               as it was uploaded, plus, if the user wrote
                               on it, "<name> (handwriting).pdf": only the
                               pages written on, each labelled with the
                               page it is on. Read both side by side.
  mkdir <folder>             Make a folder, and any missing above it
                             ("School/Math").
  mv <item> <folder>         Move into a folder ("/" is the top level).
  mv <item> <new path>       Rename, or move and rename.
  rm <item>                  Move to the tablet's trash. Only the user can
                             empty it; "mv trash/<item> /" restores.

  Name a document or folder by its path from the top level
  ("School/Math/Worksheet 3"; "trash/..." in the trash) or by its id
  from ls. Two with the same path need the id.

EXAMPLES
  delta remarkable ls
  delta remarkable upload hw3.pdf School/Math --name "Homework 3"
  delta remarkable get "School/Math/Homework 3"
  delta remarkable get "Quick sheets" notes/

NOTES
  The handwriting PDFs draw the strokes themselves (pens, colors,
  highlighters) and any typed text, at the page's real size. To read
  handwriting, look at the pages; don't expect a text layer. Templates
  (lines, grids) aren't drawn.

  The tablet picks up changes when it next syncs, which is right away if
  it is awake and online; otherwise tell the user to wake it. Things the
  user did on the tablet show up here once it has synced.
`
