package main

const musicHelp = `delta music: download music for the user

  delta music song "<artist> - <title>"
  delta music album "<artist> - <album>"
  delta music re-login

  The command center locates the best available match and downloads it,
  displaying real-time progress here; the files are then saved to the current
  directory, followed by a summary of saved files. If no exact match is found,
  a broader search is attempted automatically. Downloads typically take a few
  minutes.
  The command center runs one download at a time, so a run may first wait for
  another to finish.

  * Use "song" to download a single track.
  * Use "album" to download an entire album folder, including cover art.
  * To process multiple tracks or albums, run the command separately for each.

  Once downloaded, send files to the user by running "delta push" for each file.
  They will be able to access their music from the Files tab. Ensure authentication
  is set up beforehand using "delta set-auth" (see "delta help").

CHECKING
  Before confirming completion to the user, verify that the downloaded files are
  correct and that albums contain the full tracklist. If a search fails or returns
  incorrect results, retry with a simplified query—such as omitting featured artists,
  remix/edition tags, or "Various Artists" tags.

CONNECTING
  If a download says it couldn't connect, wait a few minutes and try again. Only
  if it still can't connect after that, run "delta music re-login" once, then
  try again: it gives the command center a new account for downloading. Never
  run it routinely; new accounts made too often get the command center blocked.
`
