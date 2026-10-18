The release workflow builds the transform CLI (`cmd/transform`) for linux-amd64
into this directory as `transform-linux-amd64`, and the command center embeds
it, to serve to transform jobs' cloud sessions at `GET /transform/cli`. A build
without it still works; that route then says the CLI isn't there.
