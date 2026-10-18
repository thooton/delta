# delta

This repository holds the Delta command center and the Delta CLI.

## How it fits together

Delta runs in three places:

1. **Phone (Sonim XP3).** The user carries a Sonim XP3 flip phone running the Delta app, which lives in [thooton/sonim](https://github.com/thooton/sonim) under `delta/`. It has four tabs: **Chats** lists the user's sessions, **Session** is the conversation with one of them, **Files** shows everything on the command center, where the user can download it, install it (APKs), move it into a folder on the phone such as Music, or delete it, and **Accounts** shows each Claude account with how much of its 5-hour and weekly limits is used. The user can also upload a file from the phone and send it to the agent: the app puts it on the command center and ends the message with a `<delta-files>` block naming it, which the agent fetches with `delta pull`.
   Chats holds two kinds of session. Regular ones, marked ●, are Claude Code cloud sessions, run on the user's Claude accounts, whose logins the command center keeps (see [Cloud accounts](#cloud-accounts)). Backup ones, marked ▲, are run by the command center itself for when cloud sessions aren't available (see [Backup sessions](#backup-sessions)). The app is white-labelled: it never names the agent or the model behind either kind.
2. **Command center (VPS).** A server that runs around the clock. It handles authentication and relays between the phone and everything else, including external services such as Canvas. It also keeps the Claude accounts' logins and runs the backup sessions. It updates itself only when an agent tells it to.
3. **CLI (agent).** The tool an agent uses to send things to the command center.

The command center also serves a **website** (see [Website](#website)): the phone app's Chats, Session, Files and Accounts for a computer's browser, laid out like the Codex app.

A typical job looks like this. One of the apps on the phone has a bug, so the user opens the Delta app and describes the problem to the agent. The phone apps live in [thooton/sonim](https://github.com/thooton/sonim), and the agent has read and write access to it and to this repository. The agent clones the repository, fixes the bug, and pushes the change. It then uses the CLI to push the rebuilt APK to the command center. The APK shows up in the files list in the Delta app, where the user decides whether to download it, install it, or delete it.

If the problem is in Delta itself, the agent fixes it here instead. Once the fix lands on `master` and GitHub Actions has built it, the agent tells the command center to update to the new binary.

The agent is never told to clone this repository, since that would only clutter its context. Instead, the first message of every session started from the app opens with a short `<delta-setup>` block (written by `Setup.wrap` in the app; the phone hides it again). It tells the agent who it is and to read `delta help`, which explains everything it needs to know to help the user. The CLI is already there by then: the session's setup script installs it (the cloud environment's `init_script`, `Setup.SCRIPT` in the app; `-agent-setup` for backup sessions), and `DELTA_SERVER` and `DELTA_TOKEN` in the session's environment sign it in. The app and the command center send the block word for word the same (`Setup.wrap` and `agentSetup`); keep them in step. So `delta help` is where anything an agent should know about the phone belongs.

## Layout

- `cmd/delta-cc`: the command center, an HTTP server. `agents.go` runs the backup sessions, with `agents_loop.go` (the agent loop and the saved conversation), `agents_model.go` (the model, and how its requests pass for the OpenCode CLI's), `agents_tools.go` (the agent's tools, which run in the container) and `agents_runtime.go` (the containers). `scope.go` decides what each kind of token may do. `cloud.go` keeps the Claude accounts and renews their logins, `conversations.go` stores conversations carried between sessions, `canvas.go` is the Canvas relay, `music_run.go` runs music downloads, `secret.go` stores single secrets (the GitHub token in `github.go`, which also has the GitHub relay and the Google Maps key in `maps.go`), and `books.go` and `gmail.go` store the book and Gmail logins, and `remarkable.go` reads and writes the reMarkable's files in rmfakecloud's storage. `transform.go` runs transform jobs, with `transform_files.go` (the embedded transform CLI, in `transform-cli/`, and the fonts transform sessions install). `web.go` serves the website, whose files are in `web/` (embedded).
- `cmd/delta`: the CLI. Its `delta help`, `delta accounts help`, `delta canvas help`, `delta music help`, `delta books help`, `delta maps help`, `delta gmail help`, `delta remarkable help` and `delta docx` text is written for the agent. The `canvas*.go` files hold the Canvas commands, `music*.go` the music commands, `books*.go` the book commands, `maps*.go` the place search commands, `gmail*.go` the Gmail commands, `remarkable*.go` the reMarkable commands, `accounts.go` the cloud account commands, `resume.go` `delta resume`, `relay.go` the GitHub relay's local end for backup sessions, and `markdown.go` converts Markdown to HTML.
- `cmd/transform`: the transform CLI, which a transform job's cloud session uses instead of `delta` (see [Transform jobs](#transform-jobs)). Its `transform help` is written for that agent.
- `internal/remarkable`: reads the tablet's documents and draws them as one PDF, for `delta remarkable get` and `transform get` (`lines.go` reads the page files, `render.go` draws them, `pdf.go` reads and writes PDF, `document.go` puts a document together).
- `internal/docx`: the guide to making Word documents (`help.md`) and the scripts it uses (`docx/`), for `delta docx` and `transform docx`; `extract` writes out the scripts.
- `scripts/install.sh`: the setup script published to the release as `install.sh`.
- `scripts/fetch-music-engine.sh`: puts the music engine into `cmd/delta-cc/music-engine/` for the command center to embed (see [Music](#music)).
- `.github/workflows/release.yml`: on every push to `master`, builds both programs (and the transform CLI, which the command center embeds) and publishes them to the [`latest` release](https://github.com/thooton/delta/releases/tag/latest). It does not deploy anything.
- `deploy/delta-cc.service`: the systemd unit for the VPS.

All three programs use only the Go standard library. The command center also embeds a prebuilt music engine (see [Music](#music)).

## Authentication

The auth token never appears in the code. Whoever runs the command center generates it on the VPS:

```
delta-cc gen-token
```

This prints the token once and stores only its SHA-256 hash, in `data/token.sha256`. Running the command again replaces the token; restart `delta-cc` afterwards so it takes effect. Every request to the command center must include the header `Authorization: Bearer <token>`.

Transform jobs get a key of their own, which reaches only that job's routes (see [Transform jobs](#transform-jobs)). Backup sessions get a token of their own instead, made with the session's container and revoked when it is deleted. It works only on the agent socket inside its container and can do much less (see [Backup sessions](#backup-sessions)). `GET /auth` reports which kind a token is, as `"scope": "full"` or `"agent"`.

The Delta app stores the token and includes it, along with the command center's URL, in the prompt it sends to the agent. The agent runs `delta set-auth <url> <token>`, which checks the token against the server and saves both values to `~/.config/delta/config.json` for later commands.

The app gets the URL and token at build time, from the same `~/.config/delta/config.json`. So running `delta set-auth` on the machine that builds the APK is enough (`delta-center.json` beside `sonim/delta/android`, or `-Pdelta.center=<file>`, take precedence). Without one, the user can enter them from the Files tab's menu. The app only connects to an `https://` command center.

Two things follow from the token travelling in the first message. It stays visible in the session on claude.ai/code. And the cloud environment's network must allow the command center's host: the app adds it when it has to create the account's first cloud environment, but an existing environment needs it added by hand.

## CLI

```
delta set-auth <url> <token>   verify and save credentials
delta push <path> [name]       upload a file (refused if the name is taken)
delta pull <name> [dest]       download a file (e.g. a photo sent from the phone)
delta list                     list files
delta delete <name>            delete a file
delta git <args> / delta gh <args>       run git or gh through the GitHub relay
delta set-personal-access-token <tok|->  check and save a new GitHub token (write-only)
delta update-cc                update the command center to its latest official build
delta accounts [...]           list, add and remove the Claude accounts (see below)
delta resume [<id> <file>]     save a conversation carried over from another session to a file
delta canvas <command> ...     use the user's Canvas (see below)
delta music song|album <name>  download a song or an album (see below)
delta music re-login           give the command center a new music account
delta books <command> ...      download ebooks (see below)
delta maps <command> ...       find places and their hours (see below)
delta gmail <command> ...      read and send the user's Gmail (see below)
delta remarkable <command> ... the user's reMarkable tablet (see below)
delta docx                     print a guide to making Word documents
delta docx extract [dir]       write out the scripts that guide uses
```

Agents download the CLI from `https://github.com/thooton/delta/releases/download/latest/delta-linux-amd64`. Builds for `linux-arm64`, `darwin-amd64`, and `darwin-arm64` are published alongside it.

To install it on your own machine, use the setup script published with the release, which picks the right build and puts it on your PATH (`/usr/local/bin` as root, otherwise `~/.local/bin`, or `$DELTA_INSTALL_DIR`):

```
curl -fsSL https://github.com/thooton/delta/releases/download/latest/install.sh | sh
```

## Command center API

Every endpoint requires the bearer token, except the website's `GET /` and `GET /web/...`, `GET /transform/cli` and `GET /transform/fonts/{name}`.

| Method | Path | Description |
|---|---|---|
| GET | `/` | The website. No token needed: it asks for one. |
| GET | `/web/{file}` | The website's scripts and styles. |
| GET | `/auth` | Checks the token, and says its scope (`full` or `agent`). |
| GET | `/files` | Lists files as JSON (`name`, `size`, `modified`), newest first. |
| GET | `/files/{name}` | Downloads a file. |
| PUT | `/files/{name}` | Uploads a file from the raw request body. Returns 201, or 409 if the name is taken: files are never overwritten. |
| DELETE | `/files/{name}` | Deletes a file. |
| GET | `/version` | Returns the running build's git commit. |
| POST | `/update` | Installs the latest official build (`https://github.com/thooton/delta/releases/download/latest/delta-cc-<os>-<arch>`) and restarts. It takes no URL; a body is ignored. |
| GET | `/canvas-status` | Canvas base URL and the cookie service's health. |
| any | `/canvas/{path}` | Relayed to `<canvas-url>/{path}` with the user's Canvas session. |
| POST | `/canvas-upload/{path}` | Canvas file upload. The body is the file, `{path}` is the Canvas preflight endpoint, and the query carries `name`, `content_type` and any other preflight parameters. Responds with the Canvas file object. |
| GET | `/music-config` | The music downloading configuration, as text. `X-Delta-Default: true` while it is the built-in default. |
| PUT | `/music-config` | Replaces it with the request body. Rejects anything the engine can't parse and any login. |
| DELETE | `/music-config` | Restores the default. |
| POST | `/music` | Takes `{"mode": "song"\|"album", "query": "..."}` and runs the download. Streams JSON lines: `{"stdout": ...}` and `{"stderr": ...}` log lines, `{}` keepalives, and finally `{"done": {"error", "job", "files"}}`. Or takes `{"link": "slsk://..."}`, a result from `/music-search`, and downloads that one, with the log not white-labeled (refused to backup sessions). |
| POST | `/music-search` | Takes `{"mode": "song"\|"album", "query": "..."}` and streams the search like `/music`, ending with `{"done": {"error", "results"}}`: every result that meets the config's conditions, best first by its preferences, each `{"link", "user", "speed", "free", "folder"?, "files": [{"path", "size", "length", "bitrate", "samplerate", "bitdepth"}]}`. Not white-labeled, and not open to backup sessions. For Source on the phone. |
| GET | `/music-jobs/{id}` | A finished download's files, as a tar. |
| DELETE | `/music-jobs/{id}` | Removes a finished download. |
| POST | `/music-login` | Replaces the music account with a newly made-up one. |
| GET | `/cloud-accounts` | The Claude accounts, as `[{"id", "email", "plan", "added"}]`, in the order the phone tries them. |
| POST | `/cloud-accounts` | Adds one from `{"refresh_token", "access_token"?, "expires_at"?}` after checking it with the provider. 201, or 200 when the same account was already there (its login is replaced in place). 403 if the provider rejects the login. |
| DELETE | `/cloud-accounts/{id}` | Removes one. |
| POST | `/cloud-accounts/{id}/token` | An access token for the account, as `{"access_token", "expires_at"}` (milliseconds), renewing the login first if need be. A body of `{"refused": "<token>"}` says the provider turned that token down. 403 when the login no longer works. |
| GET | `/cloud-accounts/{id}/usage` | The provider's usage report for the account (`five_hour` and `seven_day` utilization and reset times), as it sends it. |
| any | `/cloud-accounts/{id}/api/{path}` | Relayed to `https://api.anthropic.com/{path}` with the account's access token, for the website. Only the session routes (`v1/sessions`, `v1/code/sessions`, `v1/environment_providers`, `api/oauth/profile`). A 401 renews the login once and retries; event streams are passed through as they arrive. |
| POST | `/conversations` | Stores a carried-over conversation (the raw body, as text) under a new random id. Returns `{"id"}` with 201. |
| GET | `/conversations` | The stored conversations, as `[{"id", "size", "modified", "title"}]`, newest first. |
| GET | `/conversations/{id}` | One conversation, as text. |
| GET | `/books-login` | The book account, as `{"email", "password"}`. 404 while none is set. |
| PUT | `/books-login` | Replaces it. Needs an email address and a password. |
| DELETE | `/books-login` | Removes it. |
| PUT | `/personal-access-token` | Replaces the GitHub personal access token with the raw request body. There is no `GET`: it can't be read back. |
| GET | `/maps-key` | The Google Maps API key, as `{"key"}`. 404 while none is set. |
| PUT | `/maps-key` | Replaces it with the raw request body. |
| GET | `/gmail-login` | The Gmail login, as `{"client_id", "client_secret", "refresh_token", "email"}`. 404 while none is set. |
| PUT | `/gmail-login` | Replaces it. Needs all four fields. |
| DELETE | `/gmail-login` | Removes it. |
| GET | `/remarkable` | The reMarkable's documents and folders, as `[{"id", "name", "type", "parent", "pages", "size", "modified"}]`. `type` is `folder`, `pdf`, `epub` or `notebook`; `parent` is `""` at the top level and `"trash"` in the trash. |
| GET | `/remarkable/{id}` | A document's files, as a zip (the tablet's `.rmdoc`). |
| GET | `/remarkable/{id}/pages/{n}` | Page `n` (from 1) of a document as the tablet shows it, drawn as `delta remarkable get` draws it, as a one-page PDF. `X-Pages` says how many pages there are (on a 404 past the end too), and `X-Handwriting-Only: true` that they are the handwriting alone (an EPUB, or a PDF that couldn't be read). 404 with `X-Pages: 0` when there is nothing to show. For Tablet on the phone. |
| POST | `/remarkable/documents` | Puts the body, a PDF or EPUB, on the tablet. Query: `name`, `parent` (a folder's id, or empty). Returns 201 and `{"id"}`. |
| POST | `/remarkable/folders` | Takes `{"name", "parent"}` and makes a folder. Returns 201 and `{"id"}`. |
| POST | `/remarkable/sync` | Tells the tablet to sync now (every change already does). 503 with the reason when it can't be told. |
| PATCH | `/remarkable/{id}` | Takes `{"name"?, "parent"?}`: renames or moves, and `"parent": "trash"` trashes. |
| POST | `/remarkable/{id}` | The same as `PATCH`, for clients that can't send it (Java's `HttpURLConnection`, so Tablet on the phone). |
| GET | `/transforms` | The transform jobs, newest first, as `[{"id", "document", "name", "status", "created", "updated", "account", "session", "title", "progress", "percent", "error", "result", "tablet", "files"}]`. `status` is `running`, `done` or `failed`; `title`, `progress` (a status line) and `percent` are what the agent last reported. |
| POST | `/transforms` | Takes `{"document", "server"?}` and starts a transform job on that tablet document. `server` is the command center's `https://` address as the job's session reaches it (by default `https://` and the request's host). Returns 201 and the job. |
| DELETE | `/transforms/{id}` | Forgets a job; a running one's key stops working. |
| GET | `/transform/job` | For a job's key: `{"name", "type", "pages"}` of its document. |
| GET | `/transform/document` | For a job's key: the document's files, as a zip. |
| POST | `/transform/status` | For a job's key: takes `{"title"?, "status"?, "percent"?}`, what the job is (at most 6 words, no em dashes or arrows), what the agent is doing, and how far along it is (0 to 100), for the user. |
| POST | `/transform/submit` | For a job's key: takes the result (query: `name`), a PDF as the body, or `multipart/form-data` with the parts `pdf` and `docx` (the Word document it was made from, optional), puts the PDF on the tablet and both in Files, and ends the job. A Word document without its PDF is refused. |
| POST | `/transform/fail` | For a job's key: takes `{"reason"}` and ends the job. |
| GET | `/transform/cli` | The transform CLI (linux-amd64). No token needed. |
| GET | `/transform/fonts/{name}` | One of Microsoft's core font installers (`times32.exe`, `arial32.exe`, `courie32.exe`, `georgi32.exe`, `verdan32.exe`), as Microsoft published them, for transform sessions to install. No token needed. |
| any | `/github/{host}/{path}` | Relayed to `https://{host}/{path}` with the GitHub token, for `github.com`, `api.github.com`, `uploads.github.com` and `codeload.github.com`. Redirects are passed back, not followed. |
| GET | `/agent-sessions` | The backup sessions, as `{"data": [{"id", "title", "status", "created", "updated"}]}`, newest first. `status` is `running` or `idle`. |
| POST | `/agent-sessions` | Takes `{"title", "uuid", "text", "images"}` (images as base64 JPEGs) and starts a backup session with it. Returns 201 and `{"id"}`. |
| POST | `/agent-sessions/{id}/messages` | Takes `{"uuid", "text", "images"}`. A message sent mid-turn reaches the agent between its steps. |
| POST | `/agent-sessions/{id}/interrupt` | Stops the turn under way. |
| DELETE | `/agent-sessions/{id}` | Deletes the session: its agent, its container, its token and its files. |
| GET | `/agent-sessions/{id}/events` | The session's stream as server-sent events, replayed after `?from_sequence_num=` or `Last-Event-ID`. |

The `/agent-sessions` routes are on the public listener only. A backup session's token can't use them.

Running `delta-cc` with no arguments starts the server. By default it listens on `:8080`, stores data in `./data`, and accepts uploads up to 1 GiB; the `-addr`, `-data`, and `-max-upload` flags change these. It also listens on the agent socket, `data/agent-socket/delta.sock` (see [Backup sessions](#backup-sessions)). The server speaks plain HTTP, so run it behind a reverse proxy that handles TLS (Caddy, for example). Otherwise the token travels unencrypted.

## Cloud accounts

The phone's sessions run on the user's Claude subscriptions (Pro, say), and there can be several. Their logins live only on the command center, in `data/cloud-accounts.json` (mode 0600). The phone holds none: it asks for an access token whenever it needs one.

**Why the command center renews them.** A subscription login is an OAuth pair whose refresh token is replaced every time it is used; the old one stops working. If two parties renewed the same login, one of them would be left with a dead refresh token. So only `cloud.go` renews. `POST /cloud-accounts/{id}/token` hands out the current access token while it has more than 15 minutes left and renews it otherwise. A caller whose token the provider turned down early sends it back as `refused`, and the login is renewed only if that token is still the current one. Renewals of one account are taken one at a time, so callers arriving together share one renewal. Renewal uses the provider's token endpoint with the same client ID and scopes as Claude Code.

**Adding one.** `delta accounts login` runs Claude Code's OAuth flow with the manual redirect: it prints an address, the user signs in with the account to add, and the page shows a code that the user pastes back (or that is passed to `delta accounts finish <code>` when there is no terminal, as when an agent runs it for the user). The CLI trades the code for a login and posts it to the command center, which looks the account up (`/api/oauth/profile`: its UUID, which becomes its id, email and plan) before saving it. `delta accounts add <file>` takes an existing login instead (Claude Code's `.credentials.json`, a Pi `auth.json`, or `{"refresh_token"}`), but the copy it came from stops working once the command center renews it. Adding an account that is already there replaces its login in place.

**Rotation (on the phone).** The phone lists the accounts and their sessions together in Chats. It starts each new session on the first account, in the listed order, that it doesn't know to be out: before using one it checks its usage report, and one whose 5-hour or weekly window is at 100% is skipped until that window resets. When a session's account reaches its limit mid-conversation (the stream's `rate_limit_event` says `rejected`), the foot of the transcript says so and offers a button to move the conversation to a new session on another account. A message the user sends instead waits on the phone and goes out when the limit lifts. Moving reads the old session's full event history from the cloud, writes all of it out as text (tool calls and results included) and stores it on the command center under a random id (`data/conversations/<id>.txt`, see `conversations.go`). The new session's first message opens with a `<delta-continued>` block naming the id, and the agent saves the conversation to a file with `delta resume <id> <file>` and reads it (`delta help` explains this; a file, because an agent's shell cuts long output short). `delta accounts` shows each account's usage.

## Website

`GET /` serves Delta for a computer's browser: plain HTML, CSS and JavaScript in `cmd/delta-cc/web/` (no build step, no libraries), embedded in `delta-cc` so it updates with `update-cc`. It is the phone app's tabs laid out like the Codex app: a sidebar with New chat, Files, Tablet (the reMarkable's documents and their transform jobs), Accounts and every chat (● and ▲ alike, newest first), and the conversation with a composer under it. The composer's picker chooses the kind of a new chat, as the phone's "New session ●" and "New session ▲" do.

**Signing in.** The page asks for the delta token, checks it with `GET /auth` (it refuses an `agent` token), and keeps it in the browser's `localStorage`. It sends it with every request to the API on the same origin, so there is no CORS to set up. Every request is relative to the page's own address, so the command center can sit in a subdirectory behind a proxy (`https://host/cc/`); the page adds a missing trailing slash, and gives new environments that address, subdirectory included, as `DELTA_SERVER`. A 401 from the command center signs the browser out.

**Regular sessions (●).** The provider's session routes refuse a browser's CORS preflight, so the page can't call them itself. It goes through the cloud relay instead (`/cloud-accounts/{id}/api/...`, in `cloud.go`), which adds the account's access token (renewing the login as the token route does), the `anthropic-version` and client headers, and passes on only `Accept`, `Content-Type`, `anthropic-beta`, `x-organization-uuid` and `Last-Event-ID`. The browser never holds a cloud login. Everything else follows the app (`Cloud.java` in thooton/sonim): the session list per account, rotation (the first account whose usage report has room in both windows, remembered in `localStorage` while one is out), the Delta environment found by the same name (a hash of the setup script, the page's origin as `DELTA_SERVER`, and the token), the first message opening with the same `<delta-setup>` block, `set_permission_mode: auto` before every message, interrupt, permission answers and archive. Keep `SETUP_BLOCK` and `SETUP_SCRIPT` in `web/app.js` in step with `Setup.java` and `agentSetup`. Moving a conversation to another account when its limit is reached isn't offered yet: the page says until when the account is out.

**Backup sessions (▲).** Through `/agent-sessions`, as the phone does.

**Messages.** Pictures (pasted, dropped or attached, at most four) are shrunk to 1568 pixels and sent as JPEGs in the message. Other files go up to `/files` first (under a numbered name if taken) and are named in a closing `<delta-files>` block, word for word the app's `Attached.wrap`. The transcript shows what was typed, the agent's replies as Markdown (`web/md.js`), thinking, and each tool call as one line that opens to its input and result (a diff for edits, the plan for to-do updates), as Codex does. Enter sends, Shift+Enter starts a new line, Escape stops a turn.

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

**Choosing, on the phone.** The phone's Source app (`source/` in thooton/sonim) lets the user pick what to download instead. `POST /music-search` runs the engine with `--print json-all`, which lists every result that meets the config's required conditions, ranked by its preferred ones, the order a download by name would choose in (and searches again more broadly when there are none, as a download does). The phone shows them, and `POST /music` with the picked result's `slsk://` link downloads exactly that file or folder into a job as usual. Neither is white-labeled: the user sees the engine's own log, the network and who shares each result. Backup sessions can't use either.

**Configuration.** The engine's config (required and preferred formats and bitrates, file naming, and profiles such as album-only naming) is stored on the command center as `data/music.conf`, falling back to a built-in default. Each run writes it, with the login, to a private temporary file and points the engine at it. Its `output-dir` is ignored: the engine always saves into the job directory. The CLI doesn't offer a way to change it; use the `/music-config` endpoints.

**Logins.** Soulseek needs an account but registers any unknown username on first login. The command center has one, made up on first use (16-character username, 24-character password) and kept in `data/music-login.json` (mode 0600); the config never holds one. It keeps the same account because making up a new one on every run gets the machine's IP throttled by the Soulseek server, which then never answers logins. `delta music re-login` (`POST /music-login`) replaces it with a new one, for when the account itself stops working; `delta music help` tells the agent to use it only after waiting and retrying fails. A second login with the same account kicks the first off, so downloads take turns: a run that arrives while another is going waits (and says so). If logins keep failing, a run gives up after about a minute instead of retrying forever as the engine would.

## GitHub access

The agent reaches the user's repositories with a GitHub personal access token stored on the command center as `data/personal-access-token`, which no session ever sees: there is no route that returns it. The agent runs `delta git ...` and `delta gh ...` instead of `git` and `gh`, and the CLI runs a small proxy for the length of the command:

- git reaches it through a `url.http://127.0.0.1:<port>/github.com/.insteadOf=https://github.com/` setting passed in its environment (`GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n`), which rewrites every GitHub URL it fetches from or pushes to (`https://`, `git@github.com:` and `ssh://git@github.com/` forms alike). So no git config, remote URL or clone ever holds a proxy address or a token. git applies only one rewrite per URL, and on a tie the first defined, and Claude Code cloud sessions pass their own `git@github.com:` → `https://github.com/` rewrite the same way. So the CLI drops the environment's rewrites of GitHub prefixes and keeps everything else it sets; otherwise SSH-style remotes would go straight to GitHub.
- gh reaches it through `http_unix_socket` in a temporary `GH_CONFIG_DIR`, which makes gh send every request as plain HTTP over a unix socket, with a placeholder `GH_TOKEN` so it doesn't ask to log in.

The proxy sends GitHub requests (to `github.com`, `api.github.com`, `uploads.github.com`, `codeload.github.com`) to the command center's `/github/{host}/...` relay with the session's delta token, and the relay adds the GitHub token on the way out and strips cookies and credentials on the way back. Anything off GitHub (signed download links for release assets and artifacts) the proxy fetches directly, with no credentials. Redirects are passed back to git or gh, not followed, so the token is never sent to a host a redirect names.

So someone who copies the delta token out of a transcript can use GitHub only through the command center, for as long as that token is valid: rotating it (`delta-cc gen-token`) cuts them off, with no need to revoke the GitHub token. (Earlier versions handed the token to git and gh through their environment, and before that had the agent install a global git credential helper, which Claude Code's auto-mode classifier blocks as unauthorized persistence.) `delta set-personal-access-token` checks a new token against `api.github.com/user` (refusing one GitHub rejects, saving it with a warning if GitHub can't be reached) before storing it. To set it on the VPS directly, write the token to that file, readable only by the `delta` user.

## Backup sessions

Backup sessions (▲ in the app) are agent sessions the command center runs itself, for when Claude Code cloud sessions aren't available. The phone lists them in Chats beside the cloud sessions and talks to them the same way: `POST /agent-sessions` starts one with its first message, and its event stream uses the cloud's frame format (`event: client_event`, `{"sequence_num", "source", "payload"}` with `user`, `assistant`, `result` and `env_manager_log` payloads), so the app's transcript code reads both. The app offers "New session ●" and "New session ▲", and **Delete** in place of Archive for a backup session.

**How one runs.** The command center runs the agent itself, in Go (`agents_loop.go`), using GLM 5.3 Flash through OpenCode Zen's free tier (see [The model](#the-model-free)). A turn asks the model for a reply, runs the tool calls in it, and asks again with their results, until a reply calls no tools. The agent has four tools, read, write, edit and bash (`agents_tools.go`), and every one runs in the session's container through `<runtime> exec`, in `/root`, with a fixed environment, so everything the model does happens there. read shows text files up to 2000 lines or 50 KB at a time and attaches pictures (scaled down to fit 1568 pixels); bash shows the last 2000 lines or 50 KB of a command's output and saves the whole of it in the container's `/tmp`; edit replaces exact text, and when the text doesn't quite match it tries again ignoring trailing spaces and typographic quotes, dashes and spaces. Each session gets its own container, made from `-agent-image` (default `docker.io/library/buildpack-deps:trixie-scm`: Debian with curl and git, and nothing else). Nothing is baked into the image: like a cloud environment, a new container first runs the setup script as root (`bash -s`, up to 15 minutes), which is the file `-agent-setup` names, or by default `defaultAgentSetup`, which installs the delta CLI. A script that fails is shown on the phone as a status line and the session carries on. The container is made with `DELTA_SERVER` (the agent socket) and `DELTA_TOKEN` (see below) set, so the CLI is signed in without `delta set-auth`. Then the agent reads a `<delta-setup>` block the command center puts ahead of the first message, telling it who it is and to read `delta help`. The phone never sees that block, because the stream records only what was typed. A message sent mid-turn waits in the session's inbox and joins the conversation before the next request to the model, so it reaches the agent between its steps. Interrupting cancels the request under way, or kills the running command's process group in the container.

**Surviving updates.** The conversation is kept in `transcript.jsonl` as the model's input items, appended at every step, and messages not yet seen by the model in `inbox.json`. When the command center starts (after an `update-cc`, say), every session whose turn was under way carries on from its last step. A tool call that was running when the old process stopped gets a result saying its output was lost and that the agent should check whether it finished. Sessions made by older command centers, which ran the agent with pi, have their pi stopped, and their conversation rebuilt from the phone's stream (what was typed and replied, without tool calls).

**Idle sessions.** After `-agent-idle` (default 15 minutes) with nothing happening, a session's container is stopped (not removed). The next message starts it again, with its files as they were.

**What a backup session can reach.** Containers are made with no `--privileged`, no host network and no runtime socket, and with `-agent-memory` (4g), `-agent-cpus` (2) and `-agent-pids` (4096) limits. They reach the command center only through the agent socket, whose directory is mounted at `/run/delta`. The CLI signs in there with the container's token, made with the container (a container made again gets a new one, replacing the old) and passed as `DELTA_TOKEN`. The command center accepts that token only on the socket, never on the public listener, and accepts the delta token only on the public listener. On the socket the token may use the files, music, the reMarkable, `GET` of the book login and the maps key, `/auth`, `/version`, the GitHub relay, and `/update`. Canvas, Gmail, changing the GitHub token, the settings, transform jobs and the backup sessions API all answer 403. The book login and the maps key stay readable, because `delta books` and `delta maps` need them.

`delta git` and `delta gh` work as in any session, through the relay (see [GitHub access](#github-access)).

<a id="the-model-free"></a>**The model (free).** Backup sessions use `glm-5.3-flash-free`, with reasoning effort high, from OpenCode Zen's free tier, with no API key. Paid models aren't offered: there is no flag to pick another model and no key to pay with. Zen gives its free models only to requests that look like the OpenCode CLI's. As of OpenCode 1.19.3 it checks four things (found by capturing the CLI's own requests and taking them apart): a `User-Agent` containing `opencode/<version>`, 1.18.0 or newer (older versions get 426); `x-opencode-session` holding an OpenCode session id (`ses_` and a descending ULID); `stream: true`; and a tool named `read` (its parameters don't matter). Anything else gets 403 `FreeTierError`. `Authorization`, `x-opencode-session-id` and the other `x-opencode` headers, `stream_options`, the path and the system prompt don't matter, but `agents_model.go` sends what the CLI sends anyway (`Bearer public`, on `https://opencode.ai/inference/openai/v1`). GLM's own server also wants every tool to have a description (400 otherwise). Each session keeps its own session id as `zen_session` in `session.json`. GLM speaks only OpenAI's Chat Completions API (the Responses API gets 400 `ModelProtocolUnsupported`), but the transcript is still kept as Responses input items, as it was for the Muse Spark model sessions used before: `chatMessages` turns it into chat messages for each request, and `readModelStream` turns the streamed reply back into items. Each request carries the whole conversation. The model's reasoning (`reasoning_content`) is kept as a reasoning item and goes back as its assistant message's `reasoning_content`, as OpenCode does; pictures a tool returns follow the tool's result as a user message. Requests that fail with a 429, a 5xx or a network error are tried up to four times; Zen also answers 429 when the model's upstream is busy ("Endpoint is unavailable") or when the free tier's rate limit (`FreeUsageLimitError`) is hit. If Zen tightens the check, replies fail with a 401, 403 or 426. The phone then says the command center needs an update, and the fix goes in `agents_model.go`: capture what the current CLI sends (point `provider.opencode.options.baseURL` in an `opencode.json` at a logging proxy and run `opencode run -m opencode/glm-5.3-flash-free ...`) and compare. Model errors reach the phone as generic messages ("Too many requests", ...), because the app never names the provider. The provider's own error text goes to the command center's log.

**Storage.** Each session lives in `data/agents/<id>/`: `session.json` (title, times, the token's hash, the OpenCode session id, whether a turn is under way), `events.jsonl` (the phone's stream), `transcript.jsonl` (the model's conversation) and `inbox.json` (messages waiting for it). Containers are named `delta-<id>` (the session id without its `session_delta_`) and labelled `delta-session=<id>`.

**Failing safely.** If the agent socket can't be opened, the command center logs why and runs without backup sessions rather than not starting, so an update can't lock the phone out. The self-test runs a backup session from start to delete with stand-ins for the model and the container runtime (`selftest_agents.go`): the real loop and tools, a tool call's result reaching the model with the reasoning before it, and a turn carrying on after a restart. It also checks what a session's token can and can't reach.

## Books

`delta books` lets the agent search for ebooks and download them by result number, optionally pushing each straight to the command center (`--push`). `delta books help` documents it for the agent.

**The source.** Books come from Z-Library's `/eapi` JSON API. The client in `cmd/delta/books_source.go` is ported from the standalone zlib CLI, which copies [rookslog/zlibrary-mcp](https://github.com/rookslog/zlibrary-mcp): same domain probing and fallback list, login, search fields, headers, cookies and download flow. Two things differ so the CLI keeps to the standard library: it doesn't offer brotli (`Accept-Encoding: gzip, deflate`), and files get plain names (see below) instead of the reference's `Author_Title_Year_lang_ID`. `DELTA_BOOKS_DOMAIN` pins a domain instead of probing.

It is white-labeled like music: nothing the agent sees names Z-Library or its hosts. Network errors lose their URLs, the anti-bot wall becomes "blocking requests from this machine", and any message from the source is scrubbed of the source's names before it is printed. Files are saved as `Author - Title.ext` (just `Title.ext` without an author), keeping accented letters and dropping only characters that some file systems refuse. A different book that would get the same name gets ` (2)`, ` (3)` and so on, while the same book (same name and size) is kept rather than downloaded twice.

**The account.** The Z-Library email and password are stored on the command center as `data/books-login.json` (mode 0600) and set with `delta books login set <email> -` (password on stdin), which tries the login first and refuses to save one that is rejected. The CLI fetches the account only when it has to log in. Logins are rate-limited (about 10 an hour per IP), so it keeps the session cookies, with the last search and its results, in `~/.config/delta/books-state.json` and reuses them. When a call fails with a stored session, it logs in again once and retries. A session from before an account change keeps working on that machine until it expires.

## Maps

`delta maps` lets the agent find places with the [Places API (New)](https://developers.google.com/maps/documentation/places/web-service/op-overview), for requests like "pizza places in Denver", "a pharmacy near 85281" or "coffee next to Union Station". `delta maps help` documents it for the agent.

**The key.** The Google Maps Platform API key is stored on the command center as `data/maps-key` (mode 0600) and set with `delta maps key set -` (key on stdin), which first runs an IDs-only search (a request Google doesn't bill) and refuses a key Google rejects. Like the GitHub token, the CLI fetches it on every run and keeps no copy; it calls Google itself, so the command center never talks to Google. To set it on the VPS directly, write the key to that file, readable only by the `delta` user.

**Commands.** `delta maps search <query>` runs a Text Search and prints each place's type, rating, price level, address, whether it is open now and until when, and its id. Options: `--near <place|lat,lng>` (a place is looked up with one Text Search first, then results are biased toward it and show their distance), `--within <distance>` (a hard limit: a `locationRestriction` box around `--near`, then anything outside the circle is dropped), `--closest` (rank by distance), `--open-now`, `--min-rating`, `--limit` (at most 20) and `--json`. `delta maps place <id|name>` prints everything about one place: contact details and links, open-now status, hours for the next 7 days and special hours (happy hour, drive-through, ...), price range, amenities, payment, parking, accessibility, fuel prices, Google's summaries and up to 5 reviews. Times are shown in the place's own time zone (from `utcOffsetMinutes`), with a "Local time there" line, because the agent's sandbox runs in UTC. No separate geocoder is needed: Text Search resolves cities, zip codes, addresses and businesses alike.

**Billing.** Field masks decide which SKU Google bills. Search asks for nothing past the Enterprise tier (ratings and opening hours need it); only `place` asks for reviews and other Atmosphere fields. Resolving `--near` asks only for Pro fields.

## Gmail

`delta gmail` gives the agent the user's Gmail: `search` (Gmail's search syntax, conversations newest first), `read` (a whole conversation, HTML shown as Markdown, quoted text hidden), `attachment` (save, optionally `--push` to the Files tab), `send`, `reply` (`--all`), `forward` (with the original's attachments and HTML), `--draft` on those three and `send-draft`, `archive`/`unarchive`, `trash`/`untrash`, `mark` (read, unread, starred, important, spam and their opposites), `label`/`unlabel`, `labels` and `account`. `delta gmail help` documents it for the agent. Every id the agent sees is a message id: search shows each conversation by its latest message, and the organizing commands act on the message's whole conversation, as the Gmail app does.

**The login.** Gmail has no API keys, only OAuth, so the command center stores `data/gmail-login.json` (mode 0600): the user's own OAuth client (ID and secret), the refresh token they granted it, and the account's address. Like the maps key, the CLI fetches it on every run, trades the refresh token for an hour-long access token at `oauth2.googleapis.com/token`, calls `gmail.googleapis.com` itself and keeps nothing. The command center never talks to Google. The scope is `gmail.modify`: read, send, drafts, labels and trash, but not permanent deletion. Mail goes up through the multipart upload endpoints, which take messages up to 35 MB.

**Setting it up (once, on a computer with a browser).**

1. In the [Google Cloud console](https://console.cloud.google.com/), create a project (or pick one) and enable the **Gmail API** (APIs & Services → Library).
2. Under **Google Auth Platform** (APIs & Services → OAuth consent screen), configure it as **External**, with the user's address as the support and developer contact. Under **Data Access**, add the scope `https://www.googleapis.com/auth/gmail.modify`.
3. Under **Audience**, click **Publish app** so it is "In production". While an app is in "Testing", Google expires its Gmail refresh tokens after 7 days. A personal app doesn't need verification. Google just shows a "Google hasn't verified this app" screen at login, which **Advanced → Go to ... (unsafe)** passes.
4. Under **Clients**, create an OAuth client of type **Desktop app** and download its JSON.
5. Run `delta gmail login <that file>` (after `delta set-auth`). It prints an address and opens it in the browser. Sign in, allow access, and the browser returns to a listener on `127.0.0.1`. If the browser is on another device, its last page won't load; paste that page's address into the terminal instead. The login checks that Gmail answers before saving anything on the command center.

`delta gmail logout` revokes the login and removes it. The refresh token stops working if the user revokes Delta at [myaccount.google.com/permissions](https://myaccount.google.com/permissions), changes their Google password, or leaves it unused for 6 months; the CLI then tells the agent to ask for a new `delta gmail login`.

## reMarkable

`delta remarkable` gives the agent the user's reMarkable tablet: `ls` (the whole tree, `--trash` for the trash), `upload` (a PDF or EPUB, into a folder), `get`, `mkdir` (with any missing parents), `mv` (into a folder, or to a new path) `rm` (to the tablet's trash, which only the user empties), `transform` and `transforms` (start and list [transform jobs](#transform-jobs)), and `sync` (tell the tablet to sync now). Documents are named by path (`School/Math/Homework 3`) or id. `delta remarkable help` documents it for the agent.

**Where the files are.** The tablet syncs with [rmfakecloud](https://github.com/ddvk/rmfakecloud) on the VPS, installed in `/opt/delta/rmfakecloud/` and running as the `delta` user, with its data in `/opt/delta/rmfakecloud/data/`. The command center reads and writes that storage directly for one user (`-remarkable-data`, `-remarkable-user`, by default `sam13703`), as one more sync client. rmfakecloud keeps each user's files in `data/users/<user>/sync/` the way the reMarkable cloud's sync 1.5 does. Every file is a blob named by the SHA-256 of its contents. A document is an index blob listing its files (`<id>.metadata`, `<id>.content`, `<id>.pdf`, `<id>/<page>.rm`, ...), and the root index lists the documents. `root` holds the root index's hash, and an index's hash is the SHA-256 of its entries' hashes, not of its text. A change writes new blobs, then a new root index, and points `root` at it under rmfakecloud's own lock (`flock` on `.root.history`), appending a line to `.root.history`. The length of that file is the generation a syncing tablet must match, so a change that races the tablet makes it sync again rather than losing either. Old blobs are left behind, as rmfakecloud leaves them.

**Telling the tablet.** After a change, the command center asks rmfakecloud to tell the tablet to sync (`GET /ui/api/sync`, with a token signed as rmfakecloud's login signs them: HS256, keyed with PBKDF2 of its `JWT_SECRET_KEY`). It reads `JWT_SECRET_KEY` and `PORT` from rmfakecloud's environment file, `-remarkable-env` (default `/opt/delta/rmfakecloud/rmfakecloud.conf`, the `KEY=VALUE` format systemd's `EnvironmentFile=` reads), on every change. Without it, changes still land, but the tablet sees them only when it next syncs by itself (in practice, when its wifi reconnects), so the command center logs why it couldn't tell the tablet, and `delta remarkable sync` (`POST /remarkable/sync`) asks again and prints the reason.

**Getting pages back.** The handwriting is in the page files, the `.rm` format of firmware 3 (version 6), which rmfakecloud's own exporter can't read. The CLI reads them (`internal/remarkable/lines.go`, after [rmscene](https://github.com/ricklupton/rmscene)): strokes, layers, highlights snapped to a PDF's text, typed text with its paragraph styles, and strokes anchored to typed text, placed as rmc places them. `get` then makes one PDF that looks as the document does on the tablet (`render.go` draws, `pdf.go` reads and writes PDF). The pages come in the tablet's order (`cPages` in the `.content` file). A page of the original PDF is copied as it is, with the strokes drawn over it: the tablet's coordinates put the page, as shown after `/Rotate`, at its real size at the screen's 226 dpi (not fitted to the 1404×1872 screen, as rmapi assumed for older firmware), top at y = 0 and middle at x = 0. A page added on the tablet, and every page of a notebook, is drawn on a page of its own, as tall as it was scrolled, and as wide as the PDF's pages around it (a notebook's at the tablet's 226 dpi). The PDF reader handles cross-reference tables and streams, object streams and broken files (by scanning for objects), and copies content, fonts and images without decoding them. It refuses encrypted PDFs, and then `get` saves the original plus the handwriting on pages of its own, as it always does for EPUBs, which the tablet lays out itself. Typed text is drawn in Helvetica, so strokes anchored to it can sit a line off, and templates aren't drawn.

## Transform jobs

A transform job turns a document on the reMarkable into a new one, following instructions the user wrote on it: they write "type this up in MLA format" at the top of a handwritten essay, start a job, and get the typed essay back on the tablet. Jobs are started from the website's **Tablet** page (each document has a Transform button), from **Tablet** on the phone (`tablet/` in [thooton/sonim](https://github.com/thooton/sonim), a reMarkable companion that also browses the tablet's files and shows their pages through `GET /remarkable/{id}/pages/{n}`), with `delta remarkable transform <document>`, or with `POST /transforms`.

**How one runs.** `transform.go` starts a cloud session for the job itself, on the first Claude account with room left in both usage windows (as the phone picks one). It runs in an environment made for transform jobs (one per account and command center address, found by its name, `Delta transform <hash>`), unlike the Delta one in three ways:

- **No delta CLI.** Its setup script downloads the transform CLI from the command center (`GET /transform/cli`; the release workflow builds it into `cmd/delta-cc/transform-cli/` for the command center to embed) and installs the Python packages its guides use. `TRANSFORM_SERVER` holds the command center's address.
- **The real fonts.** The cloud image has only stand-ins for Times New Roman and the like (Liberation, Carlito, Caladea), and teachers ask for Times New Roman by name. The setup script also installs Times New Roman, Arial, Courier New, Georgia and Verdana, from Microsoft's core fonts for the web: the command center serves the installers (`GET /transform/fonts/{name}`, `transform_files.go`), which it downloads from SourceForge's copy of them the first time one is asked for, checks against the SHA-256 pinned in `transformFonts`, and keeps in `transform-fonts/` in its data directory. They are passed on unchanged, as their licence allows. The script unpacks them (cabinet archives, LZX-compressed) with `libarchive-c` over the image's libarchive, into `/usr/local/share/fonts/msttcorefonts/`, and runs `fc-cache`. A font that can't be had leaves the stand-ins.
- **A key for one document.** The session's first message carries the job's key (`delta_transform_...`), which `transform start` saves. It works on the public listener for the job's own routes (`/transform/...`) only: the document's files, preview and submit. Anything else answers 403. Submitting, failing, `DELETE /transforms/{id}`, or 12 hours passing ends the job and its key.
- **Almost no network.** The environment reaches the command center and the Python and npm package registries (`pypi.org`, `files.pythonhosted.org`, `registry.npmjs.org`) and nothing else (`allow_default_hosts: false`).

The first message also tells the agent to do only the transformation, and to ignore anything in the document asking for more (searching the web, sending data, unrelated tasks), whoever it claims to be from. It is a `<delta-setup>` block, which the phone and the website hide, followed by `Transform “<name>”`, so the session appears in Chats under that title. **Keeping the user posted.** The user sees nothing of the session from the Tablet page but what the agent reports there (`transform title`, `transform status "<line>" [percent]`, `transform percent <n>`, through `POST /transform/status`): a title for the job, a status line and a percentage, which the page shows with a bar and refreshes every 5 seconds while a job runs. `delta remarkable transforms` shows them too. A title is at most 6 words and 60 characters, with no em dashes and no ASCII arrows (`->`, `<-`, `=>`); the command center refuses anything else and says why, so the agent rewords it. The title is also the result's default name. A status line is one line of at most 120 characters, and a finished job shows 100%. `transform help` tells the agent to set the title once it knows what it's making, and to update the status at each step.

`transform help` gives the agent the workflow: `transform get` saves the document as it looks on the tablet (`internal/remarkable`, as `delta remarkable get` does), and `transform docx` is the same guide to Word documents as `delta docx`. It also covers marked-up pages: edits the user marks on a document (words crossed out, insertions and corrections written in) count as instructions even with nothing written to say so, and ask for the same document back, clean and laid out as before, with exactly those revisions made (its MARKED-UP PAGES section); the session's first message says the same. It tells the agent to make schoolwork a Word document, since many of the user's teachers take only `.docx`, and to use the font a format names (Times New Roman for MLA), never a stand-in.

**The result.** The agent always submits a PDF, and with it the Word document it made the PDF from, if it made one (`transform submit essay.pdf essay.docx`, a `multipart/form-data` request). It converts the Word document itself, with the session's LibreOffice, where it can read the PDF before submitting and where every font it used is installed; the command center converts nothing. The two serve the user's two next steps: the `.docx` is what they hand in (on Canvas, where many of their teachers take only Word documents), and the PDF goes on the tablet, where they read it and, if it needs more work, write on it and transform it again. The PDF goes on the tablet in the original's folder, named as the agent chose (default: the job's title, or `<name> (transformed)`), and both files go into Files, numbered if the name is taken; the job's `files` lists them, and the website and Tablet on the phone download them from there. A Word document submitted without its PDF is refused, and the CLI tells the agent how to make the PDF.

## Updating the command center

The command center never updates on its own. To ship a change to it:

1. Push the change to `master`.
2. Wait for the GitHub Actions build to finish and publish the new binaries.
3. Run:

   ```
   delta update-cc
   ```

It takes no link. The command center always installs the latest official build of itself, from `https://github.com/thooton/delta/releases/download/latest/delta-cc-<os>-<arch>`, and nothing else. It downloads the binary and runs `<new-binary> test`, a self-test that exercises the real request handler against a temporary data directory (see `cmd/delta-cc/selftest.go`). Only if the test passes does it replace its own executable and restart in place, keeping its arguments and process ID. If the download fails, the file isn't a delta-cc binary, or the test fails, nothing changes and the error is returned to the CLI. The CLI then waits for the restarted server to report the new version.

So the only code the command center ever runs is what was built from `master` by GitHub Actions. Someone holding the delta token can't make it run a binary of their own, and getting the GitHub token out would take a commit to `master`, which shows in its history. To run a build that isn't released, install it on the VPS by hand.

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

**For backup sessions** (also once, and on an existing VPS too), install rootless Podman for the `delta` user:

```
sudo apt-get install -y podman uidmap passt
# Rootless containers need subordinate ids, and a user session that outlives logins.
sudo usermod --add-subuids 200000-265535 --add-subgids 200000-265535 delta
sudo loginctl enable-linger delta
sudo -u delta XDG_RUNTIME_DIR=/run/user/$(id -u delta) podman pull docker.io/library/buildpack-deps:trixie-scm
```

Backup sessions need no API key. The command center finds `/run/user/<uid>` itself. Never add `delta` to the `docker` group or give containers the runtime's socket: either makes the delta token root on the VPS. To check that containers can't reach services bound to the VPS's loopback (such as the canvas-cookie service on `127.0.0.1:4782`), run `sudo -u delta XDG_RUNTIME_DIR=/run/user/$(id -u delta) podman run --rm docker.io/library/buildpack-deps:trixie-scm curl -m 5 http://host.containers.internal:4782/`; it should fail to connect. A restart, whether `update-cc` or `systemctl restart delta-cc`, picks up turns that were under way where they left off.
