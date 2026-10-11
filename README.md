# delta

This repository holds the Delta command center and the Delta CLI.

## How it fits together

Delta runs in three places:

1. **Phone (Sonim XP3).** The user carries a Sonim XP3 flip phone running the Delta app (sold separately), which talks to the command center.
2. **Command center (VPS).** A server that runs around the clock. It handles authentication and relays between the phone and everything else, including external services such as Canvas. It updates itself only when an agent tells it to.
3. **CLI (agent).** The tool an agent uses to send things to the command center.

A typical job looks like this. One of the apps on the phone has a bug, so the user opens the Delta app and describes the problem to the agent. The phone apps live in [thooton/sonim](https://github.com/thooton/sonim), and the agent has read and write access to it and to this repository. The agent clones the repository, fixes the bug, and pushes the change. It then uses the CLI to push the rebuilt APK to the command center. The APK shows up in the files list in the Delta app, where the user decides whether to download it, install it, or delete it.

If the problem is in Delta itself, the agent fixes it here instead. Once the fix lands on `master` and GitHub Actions has built it, the agent tells the command center to update to the new binary.

The agent is never told to clone this repository, since that would only clutter its context. Instead, it is told to download the CLI binary and run `delta help`, which explains everything it needs to know to help the user.

## Layout

- `cmd/delta-cc`: the command center, an HTTP server.
- `cmd/delta`: the CLI. Its `delta help` text is written for the agent.
- `.github/workflows/release.yml`: on every push to `master`, builds both programs and publishes them to the [`latest` release](https://github.com/thooton/delta/releases/tag/latest). It does not deploy anything.
- `deploy/delta-cc.service`: the systemd unit for the VPS.

Both programs use only the Go standard library.

## Authentication

The auth token never appears in the code. Whoever runs the command center generates it on the VPS:

```
delta-cc gen-token
```

This prints the token once and stores only its SHA-256 hash, in `data/token.sha256`. Running the command again replaces the token; restart `delta-cc` afterwards so it takes effect. Every request to the command center must include the header `Authorization: Bearer <token>`.

The Delta app stores the token and includes it, along with the command center's URL, in the prompt it sends to the agent. The agent runs `delta set-auth <url> <token>`, which checks the token against the server and saves both values to `~/.config/delta/config.json` for later commands.

## CLI

```
delta set-auth <url> <token>   verify and save credentials
delta push <path> [name]       upload a file, replacing any file with the same name
delta list                     list files
delta delete <name>            delete a file
delta update-cc <binary-url>   update the command center to a new binary
```

Agents download the CLI from `https://github.com/thooton/delta/releases/download/latest/delta-linux-amd64`. Builds for `linux-arm64`, `darwin-amd64`, and `darwin-arm64` are published alongside it.

## Command center API

Every endpoint requires the bearer token.

| Method | Path | Description |
|---|---|---|
| GET | `/auth` | Checks the token. |
| GET | `/files` | Lists files as JSON (`name`, `size`, `modified`), newest first. |
| GET | `/files/{name}` | Downloads a file. |
| PUT | `/files/{name}` | Uploads a file from the raw request body. Returns 201 for a new file and 200 for a replacement. |
| DELETE | `/files/{name}` | Deletes a file. |
| GET | `/version` | Returns the running build's git commit. |
| POST | `/update` | Takes `{"url": "..."}`, installs the binary at that URL, and restarts. |

Running `delta-cc` with no arguments starts the server. By default it listens on `:8080`, stores data in `./data`, and accepts uploads up to 1 GiB; the `-addr`, `-data`, and `-max-upload` flags change these. The server speaks plain HTTP, so run it behind a reverse proxy that handles TLS (Caddy, for example). Otherwise the token travels unencrypted.

## Updating the command center

The command center never updates on its own. To ship a change to it:

1. Push the change to `master`.
2. Wait for the GitHub Actions build to finish and publish the new binaries.
3. Run:

   ```
   delta update-cc https://github.com/thooton/delta/releases/download/latest/delta-cc-linux-amd64
   ```

The command center downloads the binary and runs `<new-binary> test`, a self-test that exercises the real request handler against a temporary data directory (see `cmd/delta-cc/selftest.go`). Only if the test passes does it replace its own executable and restart in place, keeping its arguments and process ID. If the download fails, the file isn't a delta-cc binary, or the test fails, nothing changes and the error is returned to the CLI. The CLI then waits for the restarted server to report the new version.

Anyone holding the token can make the command center run any binary they like, so treat the token like a shell login to the VPS (as the `delta` user).

## Setting up the VPS

This only needs to be done once.

```
sudo useradd --system --home /opt/delta --create-home delta
sudo curl -fLo /opt/delta/delta-cc https://github.com/thooton/delta/releases/download/latest/delta-cc-linux-amd64
sudo chmod 755 /opt/delta/delta-cc
sudo chown -R delta: /opt/delta
sudo -u delta /opt/delta/delta-cc gen-token -data /opt/delta/data
sudo cp deploy/delta-cc.service /etc/systemd/system/
sudo systemctl enable --now delta-cc
```

The `delta` user must own `/opt/delta`, because updates replace `/opt/delta/delta-cc` in place.
