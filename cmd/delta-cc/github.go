package main

import "path/filepath"

// The GitHub personal access token the agent uses for git and gh on the
// user's repositories. `delta get-personal-access-token` fetches it and
// `delta set-personal-access-token` replaces it.
//
// Routes (all behind the delta bearer token):
//
//	GET /personal-access-token   {"token": ...}; 404 while none is set.
//	PUT /personal-access-token   Replace it with the token as the raw body.
const accessTokenFile = "personal-access-token"

func accessTokenStore(dataDir string) *secretStore {
	return &secretStore{
		path:    filepath.Join(dataDir, accessTokenFile),
		route:   "/personal-access-token",
		field:   "token",
		what:    "token",
		missing: "no personal access token is set; ask the user for one and run `delta set-personal-access-token -`",
	}
}
