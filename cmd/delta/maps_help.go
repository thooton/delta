package main

const mapsHelp = `delta maps: find places with Google Maps

  When the user asks for places ("pizza places in Denver", "a pharmacy
  near 85281", "coffee next to Union Station"), search, then get the full
  details of the ones worth recommending. Details include today's and this
  week's hours, so check them before saying a place is open. Times are in
  the place's own time zone, which may not be yours or the user's.

COMMANDS
  search <query> [options]   Find places. Put the location in the query
                             ("pizza in Denver, CO", "pharmacy near 85281")
                             or give it with --near. Without either, Google
                             searches near this machine, not the user.
    --near <place|lat,lng>   Center the search on a place ("Union Station,
                             Denver", "85281", "Tempe, AZ") or coordinates,
                             and show how far each result is from it. A
                             place is looked up first; check the "Near:"
                             line to be sure it found the right one.
    --within <distance>      Only places within this distance of --near:
                             2mi, 1.5km, 500m, 800ft (a bare number is
                             miles). Without it, --near prefers nearby
                             places but may return farther ones.
    --closest                Order by distance from --near instead of by
                             relevance.
    --open-now               Only places open right now.
    --min-rating N           Only places rated N (0 to 5) or higher.
    -n, --limit N            How many results (default 10, at most 20).
    --json                   Google's response as JSON.
  place <id|name>            Everything about one place: address, phone,
                             website, Google Maps and directions links,
                             open-now status with the next open or close
                             time, hours for the next 7 days (holiday
                             changes included) and special hours such as
                             happy hour or drive-through, price range,
                             what it offers (dine-in, takeout, delivery,
                             reservations, vegetarian, outdoor seating,
                             ...), payment, parking, wheelchair access,
                             fuel prices, summaries and up to 5 reviews.
                             Pass an id from search, or a name with its
                             location ("Coors Field, Denver") to look up
                             the best match. --json for raw JSON.
  key set <key|->            Check and save a new Google Maps API key ("-"
                             reads stdin). It needs the Places API (New).

EXAMPLES
  delta maps search "pizza in Denver, CO"
  delta maps search pizza --near 80202 --within 2mi --open-now
  delta maps search "coffee shop" --near "Union Station, Denver" --closest
  delta maps place ChIJ9afkmet5bIcRp8sdvzfnlHM

NOTES
  The API key is kept on the command center (run "delta set-auth" first,
  see "delta help"). Never show it or put it in a file. Each search and
  each place lookup is billed to the user's Google account, so don't run
  searches in loops; look up details only for places you will tell the
  user about.
`
