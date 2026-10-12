package main

const musicHelp = `delta music: download music for the user

HOW IT WORKS
  delta music searches for the requested song or album and downloads the
  best match to this machine. To get it onto the phone, push it with
  "delta push" afterwards. Run "delta set-auth" first (see "delta help"):
  every run reads the music config from the command center.

  Downloads come from other people's computers, so they can be slow, can
  stall, and are sometimes the wrong file. Check what you got (the file
  names, and the track count of an album) before you tell the user it's
  done. A run prints a line as each item is searched, downloaded or given
  up on, and ends by listing every file it saved.

DOWNLOADING
  song "<artist> - <title>" [options]
                         One song. "Title" alone works too, and so does
                         "artist=A, title=T, album=B, length=215" (length in
                         seconds; the album and length only help choose).
  album "<artist> - <album>" [options]
                         One whole album: the entire folder, cover art and
                         all. "artist=A, title=T" finds the album holding
                         song T. --album-track-count 12 (or 12+, 12-)
                         rejects folders with the wrong number of tracks,
                         which is the best guard against wrong albums.
  artist "<artist>" [options]
                         One copy of every distinct album found for the
                         artist, compilations included. This can be a lot;
                         -n limits how many albums.
  get <input> [options]  Anything else:
                           music://user/path/file.mp3  one file from a search
                           music://user/path/folder/   one album from a search
                           Spotify, YouTube, Bandcamp or MusicBrainz URLs
                             (playlist, album, track, artist, release)
                           a CSV file (columns like Artist, Title, Album)
                           a list file of searches, one per line, with
                             --input-type list
                         Playlists download song by song; -n, --offset and
                         --reverse pick which. --dry-run shows what would be
                         downloaded. Spotify needs --spotify-id and
                         --spotify-secret from the user's own Spotify app.

  Already-downloaded songs and albums are skipped, so re-running after a
  partial failure only fetches what is missing.

CHOOSING BY HAND
  search "<artist> - <title>" [--album] [--limit 10] [--json]
                         Show what's available, best first, without
                         downloading. Each result has a music:// link (a
                         folder link with --album); pass it to "get" to
                         download exactly that one. Use this when the
                         automatic choice was wrong, or to compare versions.

OPTIONS (song, album, artist, get, search)
  -o, --output-dir DIR       Save here instead of the config's output-dir.
  --format mp3,flac          Accept only these formats.
  --pref-format flac         Prefer these formats, but take others.
  --min-bitrate N / --max-bitrate N    (kbps)
  --pref-min-bitrate N / --pref-max-bitrate N
  --length-tol SEC           How far a file's length may be from the
                             expected length (-1: don't check). Raise it
                             when a playlist song is "not found".
  --strict-title / --strict-artist / --strict-album
                             Require the title in the file name, the artist
                             in its path, the album in its folder name.
  --album-track-count N[+|-], --min-album-track-count N,
  --max-album-track-count N  Album folder track counts.
  --strict-album-quality     Every track of an album must meet --format etc.
  --cond "br>=320; format=mp3,ogg"  /  --pref "..."
                             Required / preferred conditions in one string.
  --banned-users a,b         Never download from these users.
  --strict-conditions        Reject files whose bitrate etc. is unknown.
  --album-art largest|most   Fetch extra cover art for albums.
  --name-format FMT          File naming, e.g. "{artist} - {title}". Tags:
                             artist, albumartist, title, album, year, track,
                             disc, filename, remote-foldername; "{a|b}" uses b
                             when a is missing; "( - )" adds text only if
                             tags are present.
  -n, --number N / --offset N / --reverse    Which playlist items.
  --upgrade-to-album         Download each playlist song's whole album.
  --fast-search              Take the first good song match instead of
                             waiting for the search to finish.
  --search-timeout MS        How long a search collects results (5000).
  --max-stale-time MS        Give up on a stalled download after (30000).
  -d, --desperate            Search harder (slower): artist, album and title
                             separately, then filter.
  --remove-ft                Drop "feat. X" from titles before searching.
  --remove-brackets          Drop [bracketed] text from titles.
  --regex "T:\(.*\)"         Remove a pattern from titles (T:), artists
                             (A:) or albums (L:) before searching.
  --artist-maybe-wrong       Also search without the artist name.
  --skip-music-dir DIR       Skip songs already in DIR.
  --no-skip-existing         Download again even if already downloaded.
  --write-playlist           Write an .m3u playlist of the downloads.
  --input-type TYPE          csv|youtube|spotify|bandcamp|string|list|
                             link|musicbrainz (normally detected).
  --spotify-id/--spotify-secret/--spotify-token/--spotify-refresh,
  --youtube-key              Credentials for those sources.
  --verbose                  More detail in the log.

WHEN IT GOES WRONG
  Wrong song: add --strict-title and --strict-artist, or "search" and pick.
  Wrong album: add --album-track-count, or "search --album" and pick.
  Not found: shorten the query (leave out "feat.", remix names, the artist
  if it's "Various Artists"), try --length-tol -1 or --desperate, or relax
  --format/--min-bitrate (the config may require mp3 at 256kbps or more).
  Run "delta music config" to see what the config requires.

THE MUSIC CONFIG
  Defaults for every run (where files go, required and preferred formats,
  file naming) live on the command center, so they are the same for every
  session. Options on the command line override them for one run.
  config                 Show it.
  config set <file|->    Replace it. Same "key = value" syntax as the
                         options above (without the dashes), "#" comments,
                         and "[name]" sections with "profile-cond = album"
                         for album-only settings. Show it first and edit
                         that; only change it when the user asks.
  config reset           Go back to the default.
  It never holds a login: this machine makes up its own music account
  on first use and keeps it in the delta config directory.
`
