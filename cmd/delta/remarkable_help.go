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
  get <document> [dir]       Save a document into dir (default: here) as
                             "<name>.pdf", looking as it does on the
                             tablet: a PDF's own pages, in the tablet's
                             order, with the user's handwriting,
                             highlights and typed text drawn on them, and
                             any pages they added; a notebook's pages.
                             Pages they scrolled down on come out taller.
                             An EPUB comes as "<name>.epub", plus
                             "<name> (handwriting).pdf": the pages written
                             on, alone, each labelled with its page.
    --original               Save the PDF or EPUB as it was uploaded
                             instead.
  mkdir <folder>             Make a folder, and any missing above it
                             ("School/Math").
  mv <item> <folder>         Move into a folder ("/" is the top level).
  mv <item> <new path>       Rename, or move and rename.
  rm <item>                  Move to the tablet's trash. Only the user can
                             empty it; "mv trash/<item> /" restores.
  transform <document>       Start a transform job: the user wrote
                             instructions on the document ("type this up
                             in MLA format"), and an agent of its own
                             follows them and puts the result on the
                             tablet next to it, and into the files. Only
                             when the user asks for a transform job.
  transforms                 List the transform jobs and how they went.

  Name a document or folder by its path from the top level
  ("School/Math/Worksheet 3"; "trash/..." in the trash) or by its id
  from ls. Two with the same path need the id.

EXAMPLES
  delta remarkable ls
  delta remarkable upload hw3.pdf School/Math --name "Homework 3"
  delta remarkable get "School/Math/Homework 3"
  delta remarkable get "Quick sheets" notes/

NOTES
  Handwriting is drawn as strokes (pens, colors, highlighters): to read
  it, look at the pages; it has no text layer. Typed text is drawn in
  Helvetica, not the tablet's font. Templates (lines, grids) aren't drawn.

  The tablet picks up changes when it next syncs, which is right away if
  it is awake and online; otherwise tell the user to wake it. Things the
  user did on the tablet show up here once it has synced.
`
