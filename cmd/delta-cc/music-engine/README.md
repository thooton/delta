`scripts/fetch-music-engine.sh <goos> <goarch>` puts the music engine
here (`engine.tar.gz` and `engine.version`), and
`go build ./cmd/delta-cc` embeds whatever this directory holds. Neither file
is committed. Without them the command center still builds, but answers
music downloads with "not available".
