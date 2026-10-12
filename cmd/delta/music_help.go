package main

const musicHelp = `delta music: download music for the user

  delta music song "<artist> - <title>"
  delta music album "<artist> - <album>"

  That is all there is: no other commands and no options. It finds the
  best copy and downloads it into ~/Music on this machine, printing its
  progress and then every file it saved. If nothing is found, it tries once
  more with a broader search. A download can take a few minutes.

  "song" saves one file. "album" saves the whole album folder, cover art
  included. For several songs or albums, run it once for each.

  To get the music to the user, push each saved file with "delta push";
  they get it from the Files tab. Run "delta set-auth" first (see "delta
  help").

CHECKING
  Look at what was saved before telling the user it's done: is it the
  right song or album, and does an album have the right number of tracks?
  If it's wrong, or nothing was found, run it again with the name written
  differently: a shorter title, without "feat. ..." or remix and edition
  names, or without the artist if it is "Various Artists". Something
  already downloaded on this machine is not downloaded again.
`
