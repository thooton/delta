package main

import "path/filepath"

// The Google Maps Platform API key (with the Places API (New) enabled) that
// `delta maps` uses. The CLI fetches it on every run and calls Google itself;
// the command center only keeps it. `delta maps key set` replaces it.
//
// Routes (all behind the delta bearer token):
//
//	GET /maps-key   {"key": ...}; 404 while none is set.
//	PUT /maps-key   Replace it with the key as the raw body.
const mapsKeyFile = "maps-key"

func mapsKeyStore(dataDir string) *secretStore {
	return &secretStore{
		path:    filepath.Join(dataDir, mapsKeyFile),
		route:   "/maps-key",
		field:   "key",
		what:    "key",
		missing: "no Google Maps API key is set; ask the user for one and run `delta maps key set -`",
	}
}
