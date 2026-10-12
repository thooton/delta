package main

const booksHelp = `delta books: find and download ebooks for the user

HOW IT WORKS
  "delta books search" looks a book up and numbers the results; "delta books
  download" saves results by number to this machine. Add --push to send
  each file straight to the command center, where the user gets it from the
  Files tab. Run "delta set-auth" first (see "delta help"): the account
  used to download is kept on the command center.

  The last search and its results are remembered between runs, so search,
  then download by number. A new search replaces the old results.

  The account has a daily download limit. Download only what the user
  asked for, and if downloads start failing with a web page instead of a
  file, tell the user the limit is probably used up for today.

COMMANDS
  search <query> [options]   Search by title, author, ISBN, or any mix.
    -n, --limit N            Results per page (default 10).
    --page N                 Start at page N.
    --ext epub,pdf           Only these formats (also --format). Common:
                             epub, pdf, mobi, azw3, fb2, djvu, txt.
    --lang english,french    Only these languages.
    --from YEAR / --to YEAR  Publication years.
    --exact                  Exact match only.
    --order popular|date_created|date_updated
    --json                   Results as JSON.
  next / prev / page N       Another page of the last search.
  results [--json]           Show the last search's results again (no
                             network).
  download <n>... [options]  Download results of the last search by number
                             ("1 3" or "1,3"). Prints each saved file's
                             path; "get" works too.
    -o, --output-dir DIR     Save here (default: the current directory).
    --push                   Also push each file to the command center.

  Files are named Author_Title_Year_lang_ID.ext, for example
  TolkienJRR_TheHobbit_1937_engli_123456.epub. Downloading the same book
  into the same directory again keeps the existing file.

CHOOSING
  Prefer epub unless the user says otherwise: it reflows on small screens.
  When several results look alike, pick the one in the user's language with
  the most sensible size (a novel is rarely over 10 MB as epub), and check
  the author. If nothing fits, try a shorter query (title only, no
  subtitle) or --ext without the format filter.

THE ACCOUNT
  The download account's email and password live on the command center.
  This machine logs in with them only when it has to and keeps the session
  in the delta config directory. Logins are rate-limited (about 10 an hour),
  so don't remove that file.
  login                      Show which account is set.
  login set <email> -        Replace it; the password is read from stdin
                             (or give it in place of "-"). It is tried
                             first and not saved if it is wrong. Only
                             change it when the user asks.
  login remove               Remove it.
`
