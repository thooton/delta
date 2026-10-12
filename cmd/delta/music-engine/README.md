`scripts/fetch-music-engine.sh <goos> <goarch>` puts the engine behind
`delta music` here (`engine.tar.gz` and `engine.version`), and `go build`
embeds whatever this directory holds. Neither file is committed. Without
them the CLI still builds, but `delta music` downloads nothing.
