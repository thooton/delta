# delta

This repository holds the Delta command center and the Delta CLI.

## How it fits together

Delta runs in three places:

1. **Phone (Sonim XP3).** The user carries a Sonim XP3 flip phone running the Delta app, which lives in [thooton/sonim](https://github.com/thooton/sonim) under `delta/`. It has three tabs: **Chats** lists the user's Claude Code cloud sessions, **Session** is the conversation with one of them, and **Files** shows everything on the command center, where the user can download it, install it (APKs) or delete it.
2. **Command center (VPS).** A server that runs around the clock. It handles authentication and relays between the phone and everything else, including external services such as Canvas. It updates itself only when an agent tells it to.
3. **CLI (agent).** The tool an agent uses to send things to the command center.

A typical job looks like this. One of the apps on the phone has a bug, so the user opens the Delta app and describes the problem to the agent. The phone apps live in [thooton/sonim](https://github.com/thooton/sonim), and the agent has read and write access to it and to this repository. The agent clones the repository, fixes the bug, and pushes the change. It then uses the CLI to push the rebuilt APK to the command center. The APK shows up in the files list in the Delta app, where the user decides whether to download it, install it, or delete it.

If the problem is in Delta itself, the agent fixes it here instead. Once the fix lands on `master` and GitHub Actions has built it, the agent tells the command center to update to the new binary.

The agent is never told to clone this repository, since that would only clutter its context. Instead, the first message of every session started from the app opens with a short `<delta-setup>` block (written by `Setup.wrap` in the app; the phone hides it again). It tells the agent to download the CLI binary, run `delta set-auth` with the command center's URL and token, and read `delta help`, which explains everything it needs to know to help the user. So `delta help` is where anything an agent should know about the phone belongs.

## Layout

- `cmd/delta-cc`: the command center, an HTTP server. `canvas.go` is the Canvas relay, and `music_run.go` runs music downloads.
- `cmd/delta`: the CLI. Its `delta help`, `delta canvas help`, `delta music help` and `delta books help` text is written for the agent. The `canvas*.go` files hold the Canvas commands, `music*.go` the music commands, `books*.go` the book commands, and `markdown.go` converts Markdown to HTML.
- `scripts/install.sh`: the setup script published to the release as `install.sh`.
- `scripts/fetch-music-engine.sh`: puts the music engine into `cmd/delta-cc/music-engine/` for the command center to embed (see [Music](#music)).
- `.github/workflows/release.yml`: on every push to `master`, builds both programs and publishes them to the [`latest` release](https://github.com/thooton/delta/releases/tag/latest). It does not deploy anything.
- `deploy/delta-cc.service`: the systemd unit for the VPS.

Both programs use only the Go standard library. The command center also embeds a prebuilt music engine (see [Music](#music)).

## Authentication

The auth token never appears in the code. Whoever runs the command center generates it on the VPS:

```
delta-cc gen-token
```

This prints the token once and stores only its SHA-256 hash, in `data/token.sha256`. Running the command again replaces the token; restart `delta-cc` afterwards so it takes effect. Every request to the command center must include the header `Authorization: Bearer <token>`.

The Delta app stores the token and includes it, along with the command center's URL, in the prompt it sends to the agent. The agent runs `delta set-auth <url> <token>`, which checks the token against the server and saves both values to `~/.config/delta/config.json` for later commands.

The app gets the URL and token at build time, from the same `~/.config/delta/config.json`. So running `delta set-auth` on the machine that builds the APK is enough (`delta-center.json` beside `sonim/delta/android`, or `-Pdelta.center=<file>`, take precedence). Without one, the user can enter them from the Files tab's menu. The app only connects to an `https://` command center.

Two things follow from the token travelling in the first message. It stays visible in the session on claude.ai/code. And the cloud environment's network must allow the command center's host: the app adds it when it has to create the account's first cloud environment, but an existing environment needs it added by hand.

## CLI

```
delta set-auth <url> <token>   verify and save credentials
delta push <path> [name]       upload a file, replacing any file with the same name
delta pull <name> [dest]       download a file (e.g. a photo sent from the phone)
delta list                     list files
delta delete <name>            delete a file
delta git <args> / delta gh <args>       run git or gh with the user's GitHub token
delta get-personal-access-token          print the user's GitHub token
delta set-personal-access-token <tok|->  check and save a new GitHub token
delta update-cc <binary-url>   update the command center to a new binary
delta canvas <command> ...     use the user's Canvas (see below)
delta music song|album <name>  download a song or an album (see below)
delta music re-login           give the command center a new music account
delta books <command> ...      download ebooks (see below)
```

Agents download the CLI from `https://github.com/thooton/delta/releases/download/latest/delta-linux-amd64`. Builds for `linux-arm64`, `darwin-amd64`, and `darwin-arm64` are published alongside it.

To install it on your own machine, use the setup script published with the release, which picks the right build and puts it on your PATH (`/usr/local/bin` as root, otherwise `~/.local/bin`, or `$DELTA_INSTALL_DIR`):

```
curl -fsSL https://github.com/thooton/delta/releases/download/latest/install.sh | sh
```

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
| GET | `/canvas-status` | Canvas base URL and the cookie service's health. |
| any | `/canvas/{path}` | Relayed to `<canvas-url>/{path}` with the user's Canvas session. |
| POST | `/canvas-upload/{path}` | Canvas file upload. The body is the file, `{path}` is the Canvas preflight endpoint, and the query carries `name`, `content_type` and any other preflight parameters. Responds with the Canvas file object. |
| GET | `/music-config` | The music downloading configuration, as text. `X-Delta-Default: true` while it is the built-in default. |
| PUT | `/music-config` | Replaces it with the request body. Rejects anything the engine can't parse and any login. |
| DELETE | `/music-config` | Restores the default. |
| POST | `/music` | Takes `{"mode": "song"\|"album", "query": "..."}` and runs the download. Streams JSON lines: `{"stdout": ...}` and `{"stderr": ...}` log lines, `{}` keepalives, and finally `{"done": {"error", "job", "files"}}`. |
| GET | `/music-jobs/{id}` | A finished download's files, as a tar. |
| DELETE | `/music-jobs/{id}` | Removes a finished download. |
| POST | `/music-login` | Replaces the music account with a newly made-up one. |
| GET | `/books-login` | The book account, as `{"email", "password"}`. 404 while none is set. |
| PUT | `/books-login` | Replaces it. Needs an email address and a password. |
| DELETE | `/books-login` | Removes it. |
| GET | `/personal-access-token` | The GitHub personal access token, as `{"token"}`. 404 while none is set. |
| PUT | `/personal-access-token` | Replaces it with the raw request body. |

Running `delta-cc` with no arguments starts the server. By default it listens on `:8080`, stores data in `./data`, and accepts uploads up to 1 GiB; the `-addr`, `-data`, and `-max-upload` flags change these. The server speaks plain HTTP, so run it behind a reverse proxy that handles TLS (Caddy, for example). Otherwise the token travels unencrypted.

## Canvas

The agent has full student access to Canvas through `delta canvas`. That covers courses, to-dos, assignments and their attachments, file and text submissions, submission comments, peer reviews, discussions, announcements, modules, pages, files, grades, Classic Quizzes, and the Canvas inbox, plus a raw `delta canvas api` for anything else. The user may not have a laptop, so the agent is their way into Canvas. A typical job: the user photographs handwritten work and sends it to the agent. The agent reads the assignment's requirements, transcribes the photo into the format the assignment accepts, and runs `delta canvas submit`. `delta canvas help` documents every command for the agent.

**How it authenticates.** The command center runs on the same machine as the canvas-cookie service from the old `canvas` project. That service keeps a logged-in Canvas browser session alive and serves its `Cookie` header on `127.0.0.1:4782` to clients holding its API token. The relay in `cmd/delta-cc/canvas.go` does the following:

- Fetches that cookie and caches it for 5 minutes.
- Attaches it to the agent's requests, along with the `X-CSRF-Token` that Canvas requires on writes made with a session.
- When Canvas reports the session as unauthenticated, fetches a fresh cookie and retries once.
- Follows redirects for file downloads, but sends the session only to the Canvas host. Signed storage URLs on other hosts never see it.
- Strips `Set-Cookie` from responses, because the cookie service is the only thing that renews the session.

The agent never sees the cookie, but anyone with the delta token can act as the student in Canvas.

The relay is configured with `-canvas-url` (default `https://canvas.asu.edu`; empty disables it), `-canvas-cookie-service` (default `http://127.0.0.1:4782`) and `-canvas-cookie-token` (default `/etc/canvas-cookie/api-token`). The token file is readable only by root and the `canvas-cookie` group, so the systemd unit gives `delta-cc` `SupplementaryGroups=canvas-cookie`.

**One-time setup on an existing VPS.** `update-cc` keeps the process's groups, so it can't pick up the new group itself. After installing the canvas-cookie service, run:

```
sudo cp deploy/delta-cc.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl restart delta-cc
```

If the Canvas session expires (for example after a password change or an ASU logout), `delta canvas status` reports it as not ready. The cookie service then needs a fresh seed from a browser login; see the canvas-cookie README.

## Music

`delta music` lets the agent download a song (`delta music song "<artist> - <title>"`) or an album (`delta music album "<artist> - <album>"`) from the Soulseek network into its current directory. Besides `delta music re-login` (see Logins), that is the whole interface: no other commands and no options. An earlier version passed on most of the engine's options and modes, plus search, playlists and config editing, and agents got lost in them, so now the command center makes every choice. It searches by name and downloads the best match. If nothing was downloaded it tries once more with a broader search (`--desperate --remove-ft --remove-brackets`), and the CLI ends by listing the files saved. `delta music help` documents it for the agent.

**Where it runs.** The command center does the downloading, because agents usually run in sandboxes that can't reach Soulseek. `POST /music` downloads into a job directory, `data/music-jobs/<id>/`, and streams the engine's (filtered) log back as it goes, ending with the job's id and its files. The CLI prints the log, fetches the files as one tar (`GET /music-jobs/{id}`) into the current directory, and deletes the job. Jobs the CLI never collects are removed after 24 hours. If the CLI disconnects mid-run, the engine is stopped and the job removed.

**The engine.** The work is done by [sockseek](https://github.com/fiso64/sockseek), an unmodified release of which is embedded in `delta-cc`. `scripts/fetch-music-engine.sh <goos> <goarch>` downloads the pinned release (version and SHA-256 checksum are at the top of the script) into `cmd/delta-cc/music-engine/`, and the release workflow runs it before building each command center. A plain `go build` without it still works; `POST /music` then says music isn't available. On first use the command center unpacks the engine into its data directory (`data/music-engine-<hash>/`). sockseek only publishes 32-bit ARM builds for Linux, so the `linux-arm64` command center has no music. The self-test unpacks the engine and runs `--version`, so an update can't install a command center whose engine doesn't run on the VPS. sockseek is AGPL-3.0; its license is unpacked next to it, and its source is at the link above.

The engine is white-labeled. The agent never runs it or sees its name: the command center runs it with its stdin closed (so nothing can wait on a prompt) and passes on its log with its name, its login and its stack traces removed. Nothing the agent sees names Soulseek or hints at file sharing. In the log, remote paths and `slsk://` links are cut down to the file or folder name. Lines other than job status lines that still mention peers, users, shares, uploads or queues are dropped, and error reasons that do become a generic "the download failed".

**Configuration.** The engine's config (required and preferred formats and bitrates, file naming, and profiles such as album-only naming) is stored on the command center as `data/music.conf`, falling back to a built-in default. Each run writes it, with the login, to a private temporary file and points the engine at it. Its `output-dir` is ignored: the engine always saves into the job directory. The CLI doesn't offer a way to change it; use the `/music-config` endpoints.

**Logins.** Soulseek needs an account but registers any unknown username on first login. The command center has one, made up on first use (16-character username, 24-character password) and kept in `data/music-login.json` (mode 0600); the config never holds one. It keeps the same account because making up a new one on every run gets the machine's IP throttled by the Soulseek server, which then never answers logins. `delta music re-login` (`POST /music-login`) replaces it with a new one, for when the account itself stops working; `delta music help` tells the agent to use it only after waiting and retrying fails. A second login with the same account kicks the first off, so downloads take turns: a run that arrives while another is going waits (and says so). If logins keep failing, a run gives up after about a minute instead of retrying forever as the engine would.

## GitHub access

The agent reaches the user's repositories with a GitHub personal access token stored on the command center as `data/personal-access-token`. The agent runs `delta git ...` and `delta gh ...` instead of `git` and `gh`. Each fetches the token and hands it to that one process through its environment: to git as an `http.https://github.com/.extraheader` basic-auth header via `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n` (as actions/checkout does), to gh as `GH_TOKEN`. So no git config, remote URL or clone ever holds it. (An earlier version had the agent install a global git credential helper, which Claude Code's auto-mode classifier blocks as unauthorized persistence.) `delta set-personal-access-token` checks a new token against `api.github.com/user` (refusing one GitHub rejects, saving it with a warning if GitHub can't be reached) before storing it. To set it on the VPS directly, write the token to that file, readable only by the `delta` user.

## Books

`delta books` lets the agent search for ebooks and download them by result number, optionally pushing each straight to the command center (`--push`). `delta books help` documents it for the agent.

**The source.** Books come from Z-Library's `/eapi` JSON API. The client in `cmd/delta/books_source.go` is ported from the standalone zlib CLI, which copies [rookslog/zlibrary-mcp](https://github.com/rookslog/zlibrary-mcp): same domain probing and fallback list, login, search fields, headers, cookies and download flow. Two things differ so the CLI keeps to the standard library: it doesn't offer brotli (`Accept-Encoding: gzip, deflate`), and files get plain names (see below) instead of the reference's `Author_Title_Year_lang_ID`. `DELTA_BOOKS_DOMAIN` pins a domain instead of probing.

It is white-labeled like music: nothing the agent sees names Z-Library or its hosts. Network errors lose their URLs, the anti-bot wall becomes "blocking requests from this machine", and any message from the source is scrubbed of the source's names before it is printed. Files are saved as `Author - Title.ext` (just `Title.ext` without an author), keeping accented letters and dropping only characters that some file systems refuse. A different book that would get the same name gets ` (2)`, ` (3)` and so on, while the same book (same name and size) is kept rather than downloaded twice.

**The account.** The Z-Library email and password are stored on the command center as `data/books-login.json` (mode 0600) and set with `delta books login set <email> -` (password on stdin), which tries the login first and refuses to save one that is rejected. The CLI fetches the account only when it has to log in. Logins are rate-limited (about 10 an hour per IP), so it keeps the session cookies, with the last search and its results, in `~/.config/delta/books-state.json` and reuses them. When a call fails with a stored session, it logs in again once and retries. A session from before an account change keeps working on that machine until it expires.

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

This only needs to be done once. Install the canvas-cookie service first (see its README): the unit file below joins its `canvas-cookie` group, and systemd won't start `delta-cc` if that group doesn't exist.

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
