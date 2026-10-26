# delta

This repository holds the Delta command center and the Delta CLI.

## How it fits together

Delta runs in three places:

1. **Website (the user's computer).** The command center serves Delta's website (see [Website](#website)), laid out like the Codex app. It is where agent sessions are started: its sidebar lists the user's chats, **Files** shows everything on the command center, where the user can download, upload or delete it, **Tablet** shows the reMarkable's documents and their transform jobs, and **Accounts** shows each Claude account with how much of its 5-hour and weekly limits is used. The user can attach files to a message: the website puts them on the command center and ends the message with a `<delta-files>` block naming them, which the agent fetches with `delta pull`.
   Chats are of two kinds. Regular ones, marked ●, are Claude Code cloud sessions, run on the user's Claude accounts, whose logins the command center keeps (see [Cloud accounts](#cloud-accounts)). Backup ones, marked ▲, are run by the command center itself for when cloud sessions aren't available (see [Backup sessions](#backup-sessions)). The website is white-labelled: it never names the agent or the model behind either kind.
2. **Phone (Sonim XP3).** The user carries a Sonim XP3 flip phone whose apps live in [thooton/sonim](https://github.com/thooton/sonim). Several of them talk to the command center with the delta token themselves: Canvas (the [Canvas relay](#canvas)), Source ([music](#music) and [books](#books)), Places ([maps](#maps)), Mail ([Gmail](#gmail)), Authenticator ([two-factor accounts](#two-factor-accounts)) and Tablet ([reMarkable](#remarkable) and [transform jobs](#transform-jobs)). (There used to be a Delta app on the phone that started sessions; it has been retired, and sessions are started from the website alone.)
3. **Command center (VPS).** A server that runs around the clock. It handles authentication and relays between the website, the phone's apps and everything else, including external services such as Canvas. It also keeps the Claude accounts' logins and runs the backup sessions. It updates itself only when an agent tells it to.
4. **CLI (agent).** The tool an agent uses to talk to the command center.

A typical job looks like this. One of the apps on the phone has a bug, so the user starts a chat on the website and describes the problem to the agent. The phone apps live in [thooton/sonim](https://github.com/thooton/sonim), and the agent has read and write access to it and to this repository. The agent clones the repository, fixes the bug, raises the app's version and pushes the change to `master`. sonim's GitHub Actions build every app and publish them to its `latest` release, and the Updater app on the phone offers the new version from there: the agent uploads nothing. Apps for the user's reMarkable tablet work the same way in [thooton/tablet](https://github.com/thooton/tablet): the agent pushes, its GitHub Actions publish each app to its `latest` release, and the Updater app on the tablet installs it.

If the problem is in Delta itself, the agent fixes it here instead. Once the fix lands on `master` and GitHub Actions has built it, the agent tells the command center to update to the new binary.

The agent is never told to clone this repository, since that would only clutter its context. Instead, the first message of every session opens with a short `<delta-setup>` block (`SETUP_BLOCK` in `web/app.js`; the website hides it again). It tells the agent who it is and to read `delta help`, which explains everything it needs to know to help the user. The CLI is already there by then: the session's setup script installs it (the cloud environment's `init_script`, `SETUP_SCRIPT` in `web/app.js`; `-agent-setup` for backup sessions), and `DELTA_SERVER` and `DELTA_TOKEN` in the session's environment sign it in. The website and backup sessions send the block word for word the same (`SETUP_BLOCK` and `agentSetup`); keep them in step. So `delta help` is where anything an agent should know belongs.

## Layout

- `cmd/delta-cc`: the command center, an HTTP server. `agents.go` runs the backup sessions, with `agents_loop.go` (the agent loop and the saved conversation), `agents_model.go` (the model, and how its requests pass for the OpenCode CLI's), `agents_tools.go` (the agent's tools, which run in the container) and `agents_runtime.go` (the containers). `scope.go` decides what each kind of token may do. `cloud.go` keeps the Claude accounts and renews their logins, `conversations.go` stores conversations carried between sessions, `canvas.go` is the Canvas relay, `music_run.go` runs music downloads, `secret.go` stores single secrets (the GitHub token in `github.go`, which also has the GitHub relay and the Google Maps key in `maps.go`), and `books.go` and `gmail.go` store the book and Gmail logins, `otp.go` keeps the two-factor accounts, `flashcards.go` keeps the flashcards and pairs the tablet's Flashcards, and `remarkable.go` reads and writes the reMarkable's files in rmfakecloud's storage. `smtp.go` is the SMTP relay, which sends mail through the user's Gmail for rmfakecloud. `transform.go` runs transform jobs, with `transform_files.go` (the embedded transform CLI, in `transform-cli/`, and the fonts transform sessions install) and `shorthand.go` (the relay to the Gregg shorthand reader on the user's PC). `web.go` serves the website, whose files are in `web/` (embedded).
- `cmd/delta`: the CLI. Its `delta help` and `delta accounts help` text is written for the agent. `gmail.go` and `gmail_login.go` hold `delta gmail login` and `logout`, which save the Gmail login (see [Gmail](#gmail)), `accounts.go` the cloud account commands, `resume.go` `delta resume`, `relay.go` the GitHub relay's local end for backup sessions, and `util.go` has the argument parsing and formatting the commands share. (The CLI used to have `canvas`, `music`, `books`, `maps`, `remarkable`, `docx` and Gmail commands too. They were removed once the phone's apps took them over; the command center's routes for them stay, and the transform CLI keeps its `get` and `docx`.)
- `cmd/transform`: the transform CLI, which a transform job's cloud session uses instead of `delta` (see [Transform jobs](#transform-jobs)). Its `transform help` is written for that agent.
- `internal/remarkable`: reads the tablet's documents and draws them as one PDF, for the command center's page and PDF routes and `transform get` (`lines.go` reads the page files, `render.go` draws them, `pdf.go` reads and writes PDF, `document.go` puts a document together).
- `internal/docx`: the guide to making Word documents (`help.md`) and the scripts it uses (`docx/`), for `transform docx`; `extract` writes out the scripts.
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

The website asks for the token at sign-in and gives it to each session in its cloud environment's variables, as `DELTA_TOKEN` beside `DELTA_SERVER`. On a machine of your own, `delta set-auth <url> <token>` checks the token against the server and saves both values to `~/.config/delta/config.json` for later commands.

The phone's apps get the URL and token at build time, from the same `~/.config/delta/config.json` (see `gradle/bundled.gradle` in thooton/sonim), or have them entered by hand. They only connect to an `https://` command center.

Two things follow from the token travelling in the environment. It is visible to anyone who can read the environment on claude.ai/code. And the cloud environment's network must allow the command center's host: the website adds it when it has to create the account's first cloud environment, but an existing environment needs it added by hand.

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
delta gmail login|logout       save or remove the Gmail login, for the user (see below)
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
| GET | `/otp` | The two-factor accounts, oldest first, as `[{"id", "issuer", "account", "secret", "algorithm", "digits", "period", "added"}]`. For Authenticator on the phone. |
| POST | `/otp` | Adds one account (`{"issuer", "account", "secret", "algorithm"?, "digits"?, "period"?}`) or a list of them. One whose secret is already there is skipped. 201 with every account. |
| PUT | `/otp/{id}` | Changes one (the same fields as POST; an empty `secret` keeps the one it has, and one another account has is refused). Answers with every account. |
| DELETE | `/otp/{id}` | Removes one; answers with the accounts left. |
| GET | `/flashcards` | The flashcards collection, `{"usn", "decks", "cards"}`. For the delta token and paired devices. |
| POST | `/flashcards/sync` | Takes `{"usn", "zone"?, "decks", "cards"}`, the device's changes since it last saw `usn`, and answers the same shape: every record changed here past that `usn`, and any sent one that lost to a later change. See [Flashcards](#flashcards). |
| POST | `/flashcards/pair` | No token. Takes `{"name"?}`, what the device calls itself, and starts a pairing: `{"code", "secret", "expires"}`. |
| GET | `/flashcards/pair/{secret}` | No token. 202 while the code waits, then `{"token"}`, once. |
| POST | `/flashcards/pair/approve` | Takes `{"code", "name"?}` and gives the device showing it a flashcards token. Delta token only. |
| GET | `/flashcards/devices` | The paired devices, `[{"id", "name", "added", "seen"}]`. |
| DELETE | `/flashcards/devices/{id}` | Unpairs one; its token stops working. |
| GET | `/remarkable` | The reMarkable's documents and folders, as `[{"id", "name", "type", "parent", "pages", "size", "modified"}]`. `type` is `folder`, `pdf`, `epub` or `notebook`; `parent` is `""` at the top level and `"trash"` in the trash. |
| GET | `/remarkable/{id}` | A document's files, as a zip (the tablet's `.rmdoc`). |
| GET | `/remarkable/{id}/pages/{n}` | Page `n` (from 1) of a document as the tablet shows it, drawn by `internal/remarkable`, as a one-page PDF. `X-Pages` says how many pages there are (on a 404 past the end too), and `X-Handwriting-Only: true` that they are the handwriting alone (an EPUB, or a PDF that couldn't be read). 404 with `X-Pages: 0` when there is nothing to show. For Tablet on the phone. |
| GET | `/remarkable/{id}/pdf` | The whole document as the tablet shows it, as one PDF, drawn by `internal/remarkable`; `X-Handwriting-Only` as for a page. 404 when there is nothing to draw (an EPUB with nothing written on it). For Tablet on the phone, which saves it to the SD card. |
| POST | `/remarkable/documents` | Puts the body, a PDF or EPUB, on the tablet. Query: `name`, `parent` (a folder's id, or empty). Returns 201 and `{"id"}`. |
| POST | `/remarkable/folders` | Takes `{"name", "parent"}` and makes a folder. Returns 201 and `{"id"}`. |
| POST | `/remarkable/sync` | Tells the tablet to sync now (every change already does). 503 with the reason when it can't be told. |
| PATCH | `/remarkable/{id}` | Takes `{"name"?, "parent"?}`: renames or moves, and `"parent": "trash"` trashes. |
| POST | `/remarkable/{id}` | The same as `PATCH`, for clients that can't send it (Java's `HttpURLConnection`, so Tablet on the phone). |
| GET | `/transforms` | The transform jobs, newest first, as `[{"id", "document", "name", "status", "created", "updated", "account", "session", "title", "progress", "percent", "error", "result", "tablet", "files"}]`. `status` is `running`, `done` or `failed`; `title`, `progress` (a status line) and `percent` are what the agent last reported. |
| POST | `/transforms` | Takes `{"document", "formats"?, "server"?}` and starts a transform job on that tablet document. `formats` are what it makes, any of `pdf`, `docx`, `md`, `txt` and `jpg`; `pdf` is always added (default: a PDF only). `server` is the command center's `https://` address as the job's session reaches it (by default `https://` and the request's host). Returns 201 and the job. |
| DELETE | `/transforms/{id}` | Forgets a job; a running one's key stops working. |
| GET | `/transform/job` | For a job's key: `{"name", "type", "pages"}` of its document, and the job's `formats`. |
| GET | `/transform/document` | For a job's key: the document's files, as a zip. |
| POST | `/transform/status` | For a job's key: takes `{"title"?, "status"?, "percent"?}`, what the job is (at most 6 words, no em dashes or arrows), what the agent is doing, and how far along it is (0 to 100), for the user. |
| POST | `/transform/submit` | For a job's key: takes the result (query: `name`), a PDF as the body, or `multipart/form-data` with a part for each of the job's formats (`pdf`, `docx`, `md`, `txt`, and `jpg` once per page, in order), puts the PDF on the tablet and everything in Files (page images as `<name> 1.jpg`, `<name> 2.jpg`…), and ends the job. A format missing or not asked for is refused, as is a Word document without its PDF. |
| POST | `/transform/fail` | For a job's key: takes `{"reason"}` and ends the job. |
| POST | `/transform/shorthand` | For a job's key: submits Gregg shorthand to read, the body a PNG, JPEG, WebP or BMP image (query: `mode`, `page` (default) or `line`; `region`, `x,y,w,h` in pixels). Queued for the shorthand reader's worker; answers at once, 202 `{"id", "status": "pending"}`, or 503 `shorthand service offline` when no worker is connected (or the queue is full). See [Shorthand reader](#shorthand-reader). |
| GET | `/transform/shorthand/{id}` | For the job's key that submitted the reading (404 for any other, or one forgotten): answers at once with `{"status": "pending", "state" (queued or reading), "seconds"}`, `{"status": "done", "result": {"lines": [{"grascii", "english", "draft", "postedited", "postedit", "outlines": [{"outline", "words", "exact"}], "box"}], "image", "seconds", "timing", "models", "postedit"}}` or `{"status": "error", "code", "error"}` (code 503 offline, 504 over 10 minutes, 502 failed or lost, 400 an image it can't read). Results are kept 30 minutes. |
| POST | `/shorthand`, GET `/shorthand/{id}` | The same, for the delta token (whose readings a job's key can't fetch, nor the other way round). |
| GET | `/shorthand` | `{"online", "last_seen", "queued", "reading", "worker"}`: whether the shorthand reader's worker is connected, and the models it has loaded. |
| POST | `/shorthand/reload` | Takes `{"g2e"?}` and has the worker reload its Grascii-to-English model, switched to that checkpoint (a subfolder of its model repository, like `state/best`). Returns the worker's models. |
| POST | `/shorthand/worker/next` | For the shorthand worker's secret only (not behind the delta token): takes `{"worker", "info"}` and waits up to 25 s for a request, `{"id", "kind" (read or reload), "image" (base64), "mode", "region", "g2e"}`; 204 when none came. |
| POST | `/shorthand/worker/result/{id}` | For the shorthand worker's secret only: the request's result, or `{"error", "status"?}`. |
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

**Rotation (on the website).** The website lists the accounts' sessions together in its sidebar. It starts each new session on the first account, in the listed order, that it doesn't know to be out: before using one it checks its usage report, and one whose 5-hour or weekly window is at 100% is skipped until that window resets. When a session's account reaches its limit mid-conversation, the website says until when. The website can then move the conversation to a new session on another account (as the retired phone app could): it reads the old session's full event history from the cloud, writes all of it out as text (tool calls and results included) and stores it on the command center under a random id (`data/conversations/<id>.txt`, see `conversations.go`). The new session's first message opens with a `<delta-continued>` block naming the id, and the agent saves the conversation to a file with `delta resume <id> <file>` and reads it (a file, because an agent's shell cuts long output short). `delta accounts` shows each account's usage.

## Website

`GET /` serves Delta for a computer's browser: plain HTML, CSS and JavaScript in `cmd/delta-cc/web/` (no build step, no libraries), embedded in `delta-cc` so it updates with `update-cc`. It is the only place sessions are started from. It is laid out like the Codex app: a sidebar with New chat, Files, Tablet (the reMarkable's documents and their transform jobs), Accounts and every chat (● and ▲ alike, newest first), and the conversation with a composer under it. The composer's picker chooses the kind of a new chat, ● or ▲.

**Signing in.** The page asks for the delta token, checks it with `GET /auth` (it refuses an `agent` token), and keeps it in the browser's `localStorage`. It sends it with every request to the API on the same origin, so there is no CORS to set up. Every request is relative to the page's own address, so the command center can sit in a subdirectory behind a proxy (`https://host/cc/`); the page adds a missing trailing slash, and gives new environments that address, subdirectory included, as `DELTA_SERVER`. A 401 from the command center signs the browser out.

**Regular sessions (●).** The provider's session routes refuse a browser's CORS preflight, so the page can't call them itself. It goes through the cloud relay instead (`/cloud-accounts/{id}/api/...`, in `cloud.go`), which adds the account's access token (renewing the login as the token route does), the `anthropic-version` and client headers, and passes on only `Accept`, `Content-Type`, `anthropic-beta`, `x-organization-uuid` and `Last-Event-ID`. The browser never holds a cloud login. The page keeps the session list per account, rotation (the first account whose usage report has room in both windows, remembered in `localStorage` while one is out), the Delta environment found by its name (a hash of the setup script, the page's origin as `DELTA_SERVER`, and the token, as the retired phone app named it, so environments it made are still found), the first message opening with the `<delta-setup>` block, `set_permission_mode: auto` before every message, interrupt, permission answers and archive. Keep `SETUP_BLOCK` and `SETUP_SCRIPT` in `web/app.js` in step with `agentSetup` and `defaultAgentSetup`. When a chat's account reaches its limit, the banner over the composer says until when and, if another account isn't known to be out, offers **Move to another account** (`moveChat`): it pages through the session's events (`history`), writes them out (`carryText`, as the phone's `Carry.build` did), stores that with `POST /conversations` and starts a session titled the same on another account, whose first message is the setup block, the `<delta-continued>` block (`carryBlock`) and whatever was in the composer, with the pictures the user had sent attached. The old chat stays in the list. Keep `carryBlock` in step with `delta help`.

**Backup sessions (▲).** Through `/agent-sessions`.

**Messages.** Pictures (pasted, dropped or attached, at most four) are shrunk to 1568 pixels and sent as JPEGs in the message. Other files go up to `/files` first (under a numbered name if taken) and are named in a closing `<delta-files>` block. The transcript shows what was typed, the agent's replies as Markdown (`web/md.js`), thinking, and each tool call as one line that opens to its input and result (a diff for edits, the plan for to-do updates), as Codex does. Enter sends, Shift+Enter starts a new line, Escape stops a turn.

## Flashcards

Flashcards on the phone (`flashcards/` in thooton/sonim) and on the tablet (`flashcards/` in thooton/tablet) study the same decks, kept on the command center in `data/flashcards.json` as AnkiWeb keeps Anki's (`flashcards.go`). Each device keeps a full copy, studies offline, and syncs when it opens, after answers and when it closes.

**The collection.** Decks (`id`, `name`, `new_per_day`: -1 for AUTO, else a daily limit) and cards (`id`, `deck`, `ord`, `front`, `back`, `aux`, and the FSRS state as java-fsrs keeps it: `state`, `step`, `stability`, `difficulty`, `due`, `last`, plus `intro`, when the card was first answered, which a daily limit counts). Every record has `mod`, the device's clock in milliseconds when it last changed, and `usn`, the collection's sequence number when it changed here. A sync sends what changed on the device since the `usn` it last saw and gets back everything past it; where both sides changed a record, the later `mod` wins, and a sent record that lost comes back so the device takes what is kept. A record the command center hasn't seen is added, which is how the phone's first sync brings its Spanish deck. The phone also sends its time zone (`zone`), which is kept and handed to the tablet: the tablet keeps UTC, and the Anki day must roll over at 4am where the user is on both.

**Pairing.** The tablet has no way to type the delta token, and the phone only a T9 keypad, so they pair: Flashcards asks `POST /flashcards/pair` for a code, shows it, and polls with the secret; the user types the code on the website's Tablet page, whose `POST /flashcards/pair/approve` makes a token for that device, named as it named itself ("reMarkable", "Phone") (`fc_` and 48 hex digits; only its SHA-256 is kept, in `data/flashcards-devices.json`). That token reaches `GET /flashcards`, `POST /flashcards/sync` and `GET /auth` and nothing else (`scope.go`). Codes last 15 minutes, at most 20 wait at once, and the Tablet page lists paired devices with Unpair. Backup sessions may read and sync the collection but not pair.

## Canvas

The Canvas app on the phone (`canvas/` in thooton/sonim) has full student access to Canvas through the command center's relay (`/canvas/...`, `/canvas-upload/...`, `/canvas-status`). (The CLI's `delta canvas` used the same relay; it has been removed.)

**How it authenticates.** The command center runs on the same machine as the canvas-cookie service from the old `canvas` project. That service keeps a logged-in Canvas browser session alive and serves its `Cookie` header on `127.0.0.1:4782` to clients holding its API token. The relay in `cmd/delta-cc/canvas.go` does the following:

- Fetches that cookie and caches it for 5 minutes.
- Attaches it to the agent's requests, along with the `X-CSRF-Token` that Canvas requires on writes made with a session.
- When Canvas reports the session as unauthenticated, fetches a fresh cookie and retries once.
- Follows redirects for file downloads, but sends the session only to the Canvas host. Signed storage URLs on other hosts never see it.
- Strips `Set-Cookie` from responses, because the cookie service is the only thing that renews the session.

No client ever sees the cookie, but anyone with the delta token can act as the student in Canvas.

The relay is configured with `-canvas-url` (default `https://canvas.asu.edu`; empty disables it), `-canvas-cookie-service` (default `http://127.0.0.1:4782`) and `-canvas-cookie-token` (default `/etc/canvas-cookie/api-token`). The token file is readable only by root and the `canvas-cookie` group, so the systemd unit gives `delta-cc` `SupplementaryGroups=canvas-cookie`.

**One-time setup on an existing VPS.** `update-cc` keeps the process's groups, so it can't pick up the new group itself. After installing the canvas-cookie service, run:

```
sudo cp deploy/delta-cc.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl restart delta-cc
```

If the Canvas session expires (for example after a password change or an ASU logout), `GET /canvas-status` reports it as not ready. The cookie service then needs a fresh seed from a browser login; see the canvas-cookie README.

## Music

The command center downloads songs and albums from the Soulseek network, for Source on the phone (see Choosing, below). `POST /music` with a mode and a name also downloads by name, white-labeled, with the command center making every choice: it searches by name and downloads the best match, and if nothing was downloaded it tries once more with a broader search (`--desperate --remove-ft --remove-brackets`). (That was `delta music`, which has been removed from the CLI.)

**Where it runs.** The command center does the downloading, because neither the phone nor an agent's sandbox can reach Soulseek. `POST /music` downloads into a job directory, `data/music-jobs/<id>/`, and streams the engine's (filtered) log back as it goes, ending with the job's id and its files. The client fetches the files as one tar (`GET /music-jobs/{id}`) and deletes the job. Jobs never collected are removed after 24 hours. If the client disconnects mid-run, the engine is stopped and the job removed.

**The engine.** The work is done by [sockseek](https://github.com/fiso64/sockseek), an unmodified release of which is embedded in `delta-cc`. `scripts/fetch-music-engine.sh <goos> <goarch>` downloads the pinned release (version and SHA-256 checksum are at the top of the script) into `cmd/delta-cc/music-engine/`, and the release workflow runs it before building each command center. A plain `go build` without it still works; `POST /music` then says music isn't available. On first use the command center unpacks the engine into its data directory (`data/music-engine-<hash>/`). sockseek only publishes 32-bit ARM builds for Linux, so the `linux-arm64` command center has no music. The self-test unpacks the engine and runs `--version`, so an update can't install a command center whose engine doesn't run on the VPS. sockseek is AGPL-3.0; its license is unpacked next to it, and its source is at the link above.

The engine is white-labeled in the by-name mode. The client never runs it or sees its name: the command center runs it with its stdin closed (so nothing can wait on a prompt) and passes on its log with its name, its login and its stack traces removed. Nothing the client sees names Soulseek or hints at file sharing. In the log, remote paths and `slsk://` links are cut down to the file or folder name. Lines other than job status lines that still mention peers, users, shares, uploads or queues are dropped, and error reasons that do become a generic "the download failed".

**Choosing, on the phone.** The phone's Source app (`source/` in thooton/sonim) lets the user pick what to download instead. `POST /music-search` runs the engine with `--print json-all`, which lists every result that meets the config's required conditions, ranked by its preferred ones, the order a download by name would choose in (and searches again more broadly when there are none, as a download does). The phone shows them, and `POST /music` with the picked result's `slsk://` link downloads exactly that file or folder into a job as usual. Neither is white-labeled: the user sees the engine's own log, the network and who shares each result. Backup sessions can't use either.

**Configuration.** The engine's config (required and preferred formats and bitrates, file naming, and profiles such as album-only naming) is stored on the command center as `data/music.conf`, falling back to a built-in default. Each run writes it, with the login, to a private temporary file and points the engine at it. Its `output-dir` is ignored: the engine always saves into the job directory. Change it with the `/music-config` endpoints.

**Logins.** Soulseek needs an account but registers any unknown username on first login. The command center has one, made up on first use (16-character username, 24-character password) and kept in `data/music-login.json` (mode 0600); the config never holds one. It keeps the same account because making up a new one on every run gets the machine's IP throttled by the Soulseek server, which then never answers logins. `POST /music-login` (Source's "Reconnect music account") replaces it with a new one, for when the account itself stops working; use it only after waiting and retrying fails. A second login with the same account kicks the first off, so downloads take turns: a run that arrives while another is going waits (and says so). If logins keep failing, a run gives up after about a minute instead of retrying forever as the engine would.

## GitHub access

The agent reaches the user's repositories with a GitHub personal access token stored on the command center as `data/personal-access-token`, which no session ever sees: there is no route that returns it. The agent runs `delta git ...` and `delta gh ...` instead of `git` and `gh`, and the CLI runs a small proxy for the length of the command:

- git reaches it through a `url.http://127.0.0.1:<port>/github.com/.insteadOf=https://github.com/` setting passed in its environment (`GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n`), which rewrites every GitHub URL it fetches from or pushes to (`https://`, `git@github.com:` and `ssh://git@github.com/` forms alike). So no git config, remote URL or clone ever holds a proxy address or a token. git applies only one rewrite per URL, and on a tie the first defined, and Claude Code cloud sessions pass their own `git@github.com:` → `https://github.com/` rewrite the same way. So the CLI drops the environment's rewrites of GitHub prefixes and keeps everything else it sets; otherwise SSH-style remotes would go straight to GitHub.
- gh reaches it through `http_unix_socket` in a temporary `GH_CONFIG_DIR`, which makes gh send every request as plain HTTP over a unix socket, with a placeholder `GH_TOKEN` so it doesn't ask to log in.

The proxy sends GitHub requests (to `github.com`, `api.github.com`, `uploads.github.com`, `codeload.github.com`) to the command center's `/github/{host}/...` relay with the session's delta token, and the relay adds the GitHub token on the way out and strips cookies and credentials on the way back. Anything off GitHub (signed download links for release assets and artifacts) the proxy fetches directly, with no credentials. Redirects are passed back to git or gh, not followed, so the token is never sent to a host a redirect names.

So someone who copies the delta token out of a transcript can use GitHub only through the command center, for as long as that token is valid: rotating it (`delta-cc gen-token`) cuts them off, with no need to revoke the GitHub token. (Earlier versions handed the token to git and gh through their environment, and before that had the agent install a global git credential helper, which Claude Code's auto-mode classifier blocks as unauthorized persistence.) `delta set-personal-access-token` checks a new token against `api.github.com/user` (refusing one GitHub rejects, saving it with a warning if GitHub can't be reached) before storing it. To set it on the VPS directly, write the token to that file, readable only by the `delta` user.

## Backup sessions

Backup sessions (▲ on the website) are agent sessions the command center runs itself, for when Claude Code cloud sessions aren't available. The website lists them beside the cloud sessions and talks to them the same way: `POST /agent-sessions` starts one with its first message, and its event stream uses the cloud's frame format (`event: client_event`, `{"sequence_num", "source", "payload"}` with `user`, `assistant`, `result` and `env_manager_log` payloads), so the website's transcript code reads both. It offers **Delete** in place of Archive for a backup session.

**How one runs.** The command center runs the agent itself, in Go (`agents_loop.go`), using GLM 5.3 Flash through OpenCode Zen's free tier (see [The model](#the-model-free)). A turn asks the model for a reply, runs the tool calls in it, and asks again with their results, until a reply calls no tools. The agent has four tools, read, write, edit and bash (`agents_tools.go`), and every one runs in the session's container through `<runtime> exec`, in `/root`, with a fixed environment, so everything the model does happens there. read shows text files up to 2000 lines or 50 KB at a time and attaches pictures (scaled down to fit 1568 pixels); bash shows the last 2000 lines or 50 KB of a command's output and saves the whole of it in the container's `/tmp`; edit replaces exact text, and when the text doesn't quite match it tries again ignoring trailing spaces and typographic quotes, dashes and spaces. Each session gets its own container, made from `-agent-image` (default `docker.io/library/buildpack-deps:trixie-scm`: Debian with curl and git, and nothing else). Nothing is baked into the image: like a cloud environment, a new container first runs the setup script as root (`bash -s`, up to 15 minutes), which is the file `-agent-setup` names, or by default `defaultAgentSetup`, which installs the delta CLI. A script that fails is shown on the website as a status line and the session carries on. The container is made with `DELTA_SERVER` (the agent socket) and `DELTA_TOKEN` (see below) set, so the CLI is signed in without `delta set-auth`. Then the agent reads a `<delta-setup>` block the command center puts ahead of the first message, telling it who it is and to read `delta help`. The website never sees that block, because the stream records only what was typed. A message sent mid-turn waits in the session's inbox and joins the conversation before the next request to the model, so it reaches the agent between its steps. Interrupting cancels the request under way, or kills the running command's process group in the container.

**Surviving updates.** The conversation is kept in `transcript.jsonl` as the model's input items, appended at every step, and messages not yet seen by the model in `inbox.json`. When the command center starts (after an `update-cc`, say), every session whose turn was under way carries on from its last step. A tool call that was running when the old process stopped gets a result saying its output was lost and that the agent should check whether it finished. Sessions made by older command centers, which ran the agent with pi, have their pi stopped, and their conversation rebuilt from the session's stream (what was typed and replied, without tool calls).

**Idle sessions.** After `-agent-idle` (default 15 minutes) with nothing happening, a session's container is stopped (not removed). The next message starts it again, with its files as they were.

**What a backup session can reach.** Containers are made with no `--privileged`, no host network and no runtime socket, and with `-agent-memory` (4g), `-agent-cpus` (2) and `-agent-pids` (4096) limits. They reach the command center only through the agent socket, whose directory is mounted at `/run/delta`. The CLI signs in there with the container's token, made with the container (a container made again gets a new one, replacing the old) and passed as `DELTA_TOKEN`. The command center accepts that token only on the socket, never on the public listener, and accepts the delta token only on the public listener. On the socket the token may use the files, music, the reMarkable, `GET` of the book login and the maps key, `/auth`, `/version`, the GitHub relay, and `/update`. Canvas, Gmail, changing the GitHub token, the settings, transform jobs and the backup sessions API all answer 403. The book login and the maps key stay readable, as they were for the `delta books` and `delta maps` commands, which have since been removed.

`delta git` and `delta gh` work as in any session, through the relay (see [GitHub access](#github-access)).

<a id="the-model-free"></a>**The model (free).** Backup sessions use `glm-5.3-flash-free`, with reasoning effort high, from OpenCode Zen's free tier, with no API key. Paid models aren't offered: there is no flag to pick another model and no key to pay with. Zen gives its free models only to requests that look like the OpenCode CLI's. As of OpenCode 1.19.3 it checks four things (found by capturing the CLI's own requests and taking them apart): a `User-Agent` containing `opencode/<version>`, 1.18.0 or newer (older versions get 426); `x-opencode-session` holding an OpenCode session id (`ses_` and a descending ULID); `stream: true`; and a tool named `read` (its parameters don't matter). Anything else gets 403 `FreeTierError`. `Authorization`, `x-opencode-session-id` and the other `x-opencode` headers, `stream_options`, the path and the system prompt don't matter, but `agents_model.go` sends what the CLI sends anyway (`Bearer public`, on `https://opencode.ai/inference/openai/v1`). GLM's own server also wants every tool to have a description (400 otherwise). Each session keeps its own session id as `zen_session` in `session.json`. GLM speaks only OpenAI's Chat Completions API (the Responses API gets 400 `ModelProtocolUnsupported`), but the transcript is still kept as Responses input items, as it was for the Muse Spark model sessions used before: `chatMessages` turns it into chat messages for each request, and `readModelStream` turns the streamed reply back into items. Each request carries the whole conversation. The model's reasoning (`reasoning_content`) is kept as a reasoning item and goes back as its assistant message's `reasoning_content`, as OpenCode does; pictures a tool returns follow the tool's result as a user message. Requests that fail with a 429, a 5xx or a network error are tried up to four times; Zen also answers 429 when the model's upstream is busy ("Endpoint is unavailable") or when the free tier's rate limit (`FreeUsageLimitError`) is hit. If Zen tightens the check, replies fail with a 401, 403 or 426. The website then says the command center needs an update, and the fix goes in `agents_model.go`: capture what the current CLI sends (point `provider.opencode.options.baseURL` in an `opencode.json` at a logging proxy and run `opencode run -m opencode/glm-5.3-flash-free ...`) and compare. Model errors reach the website as generic messages ("Too many requests", ...), because it never names the provider. The provider's own error text goes to the command center's log.

**Storage.** Each session lives in `data/agents/<id>/`: `session.json` (title, times, the token's hash, the OpenCode session id, whether a turn is under way), `events.jsonl` (the website's stream), `transcript.jsonl` (the model's conversation) and `inbox.json` (messages waiting for it). Containers are named `delta-<id>` (the session id without its `session_delta_`) and labelled `delta-session=<id>`.

**Failing safely.** If the agent socket can't be opened, the command center logs why and runs without backup sessions rather than not starting, so an update can't lock the user out. The self-test runs a backup session from start to delete with stand-ins for the model and the container runtime (`selftest_agents.go`): the real loop and tools, a tool call's result reaching the model with the reasoning before it, and a turn carrying on after a restart. It also checks what a session's token can and can't reach.

## Books

Source on the phone searches for ebooks and downloads them itself (`source/` in thooton/sonim). The command center only keeps the book source's account, as `data/books-login.json` (mode 0600), behind `GET`/`PUT`/`DELETE /books-login`. A `PUT` stores what it is given without trying it, so check a new login by searching from Source. (The CLI's `delta books`, whose client Source's was ported from, has been removed.)

## Maps

Places on the phone finds places with the [Places API (New)](https://developers.google.com/maps/documentation/places/web-service/op-overview) itself (`places/` in thooton/sonim). The command center only keeps the Google Maps Platform API key, as `data/maps-key` (mode 0600), behind `GET`/`PUT /maps-key`; the app fetches it on every run and calls Google itself, so the command center never talks to Google. A `PUT` stores the key without trying it. To set it on the VPS directly, write the key to that file, readable only by the `delta` user. (The CLI's `delta maps`, which Places follows, has been removed.)

## Gmail

The phone's Mail app (sonim `mail/`) reads and sends the user's Gmail, and the [SMTP relay](#smtp-relay) sends through it; both use the login below. Agents have no Gmail commands: the CLI's `delta gmail` mail commands were removed once Mail took them over, and only `login` and `logout` are left, for the user.

**The login.** Gmail has no API keys, only OAuth, so the command center stores `data/gmail-login.json` (mode 0600): the user's own OAuth client (ID and secret), the refresh token they granted it, and the account's address. Mail fetches it, trades the refresh token for an hour-long access token at `oauth2.googleapis.com/token` and calls Gmail itself. The command center talks to Google only for the [SMTP relay](#smtp-relay). The scope is `gmail.modify`: read, send, drafts, labels and trash, but not permanent deletion. The login also asks for `directory.readonly` and `contacts.other.readonly`, which the Mail app on the phone (sonim `mail/`) uses to suggest people from the user's organization and from Gmail's saved "other contacts" as an address is typed; they are optional, and a login without them (or a project without the People API) still sends and reads mail.

**Setting it up (once, on a computer with a browser).**

1. In the [Google Cloud console](https://console.cloud.google.com/), create a project (or pick one) and enable the **Gmail API** and the **People API** (APIs & Services → Library).
2. Under **Google Auth Platform** (APIs & Services → OAuth consent screen), configure it as **External**, with the user's address as the support and developer contact. Under **Data Access**, add the scopes `https://www.googleapis.com/auth/gmail.modify`, `https://www.googleapis.com/auth/directory.readonly` and `https://www.googleapis.com/auth/contacts.other.readonly`.
3. Under **Audience**, click **Publish app** so it is "In production". While an app is in "Testing", Google expires its Gmail refresh tokens after 7 days. A personal app doesn't need verification. Google just shows a "Google hasn't verified this app" screen at login, which **Advanced → Go to ... (unsafe)** passes.
4. Under **Clients**, create an OAuth client of type **Desktop app** and download its JSON.
5. Run `delta gmail login <that file>` (after `delta set-auth`). It prints an address and opens it in the browser. Sign in, allow access, and the browser returns to a listener on `127.0.0.1`. If the browser is on another device, its last page won't load; paste that page's address into the terminal instead. The login checks that Gmail answers before saving anything on the command center.

`delta gmail logout` revokes the login and removes it. The refresh token stops working if the user revokes Delta at [myaccount.google.com/permissions](https://myaccount.google.com/permissions), changes their Google password, or leaves it unused for 6 months; the user then runs `delta gmail login` again.

## Two-factor accounts

Authenticator on the phone (`authenticator/` in thooton/sonim) shows the codes for the user's two-factor accounts (TOTP, RFC 6238). The secrets live on the command center, in `data/otp.json` (mode 0600), behind `/otp` (`otp.go`), so a lost or reset phone loses none of them. The app keeps a copy so its codes show offline, works the codes out itself, and syncs the list each time it starts; the command center never computes a code. Secrets are stored as Base32, upper case with no spaces or padding; `algorithm` is `SHA1` (the default), `SHA256` or `SHA512`, `digits` 6 to 8 (default 6) and `period` 1 to 300 seconds (default 30). Adding an account whose secret is already there changes nothing, so adding the same QR code twice is harmless. Backup sessions can't reach the accounts.

## reMarkable

The command center's `/remarkable` routes give the user's reMarkable tablet to Tablet on the phone (`tablet/` in thooton/sonim) and the website's Tablet page: list, upload (a PDF or EPUB), get (as files, a page or a PDF), make folders, rename, move, trash, and sync. (The CLI's `delta remarkable` used them too; it has been removed.)

**Where the files are.** The tablet syncs with [rmfakecloud](https://github.com/ddvk/rmfakecloud) on the VPS, installed in `/opt/delta/rmfakecloud/` and running as the `delta` user, with its data in `/opt/delta/rmfakecloud/data/`. The command center reads and writes that storage directly for one user (`-remarkable-data`, `-remarkable-user`, by default `sam13703`), as one more sync client. rmfakecloud keeps each user's files in `data/users/<user>/sync/` the way the reMarkable cloud's sync 1.5 does. Every file is a blob named by the SHA-256 of its contents. A document is an index blob listing its files (`<id>.metadata`, `<id>.content`, `<id>.pdf`, `<id>/<page>.rm`, ...), and the root index lists the documents. `root` holds the root index's hash, and an index's hash is the SHA-256 of its entries' hashes, not of its text. A change writes new blobs, then a new root index, and points `root` at it under rmfakecloud's own lock (`flock` on `.root.history`), appending a line to `.root.history`. The length of that file is the generation a syncing tablet must match, so a change that races the tablet makes it sync again rather than losing either. Old blobs are left behind, as rmfakecloud leaves them.

**Telling the tablet.** After a change, the command center asks rmfakecloud to tell the tablet to sync (`GET /ui/api/sync`, with a token signed as rmfakecloud's login signs them: HS256, keyed with PBKDF2 of its `JWT_SECRET_KEY`). It reads `JWT_SECRET_KEY` and `PORT` from rmfakecloud's environment file, `-remarkable-env` (default `/opt/delta/rmfakecloud/rmfakecloud.conf`, the `KEY=VALUE` format systemd's `EnvironmentFile=` reads), on every change. Without it, changes still land, but the tablet sees them only when it next syncs by itself (in practice, when its wifi reconnects), so the command center logs why it couldn't tell the tablet, and `POST /remarkable/sync` asks again and answers with the reason.

**Getting pages back.** The handwriting is in the page files, the `.rm` format of firmware 3 (version 6), which rmfakecloud's own exporter can't read. `internal/remarkable` reads them (`lines.go`, after [rmscene](https://github.com/ricklupton/rmscene)): strokes, layers, highlights snapped to a PDF's text, typed text with its paragraph styles, strokes anchored to typed text, placed as rmc places them, and images pasted onto a page (a scene item of their own, block 0x0F, which rmscene doesn't know yet: a mesh of triangles whose corners carry page positions and the image's u and v, naming its file through the page's image file table, block 0x0E; the file, a PNG or JPEG, is `<id>/<page>/<file>`). It then makes one PDF that looks as the document does on the tablet (`render.go` draws, `pdf.go` reads and writes PDF). The pages come in the tablet's order (`cPages` in the `.content` file). A page of the original PDF is copied as it is, with the strokes drawn over it: the tablet's coordinates put the page, as shown after `/Rotate`, at its real size at the screen's 226 dpi (not fitted to the 1404×1872 screen, as rmapi assumed for older firmware), top at y = 0 and middle at x = 0. A page added on the tablet, and every page of a notebook, is drawn on a page of its own, as tall as it was scrolled, and as wide as the PDF's pages around it (a notebook's at the tablet's 226 dpi). The PDF reader handles cross-reference tables and streams, object streams and broken files (by scanning for objects), and copies content, fonts and images without decoding them. Pasted images are added as image XObjects under the page's strokes, placed by the transform their mesh gives them and clipped to it; a JPEG goes in as it is, anything else losslessly, with a soft mask for its transparency. It refuses encrypted PDFs, and then gives the original plus the handwriting on pages of its own, as it always does for EPUBs, which the tablet lays out itself. Typed text is drawn in Helvetica, so strokes anchored to it can sit a line off, and templates aren't drawn.

## SMTP relay

The reMarkable can email a document from the tablet, which rmfakecloud does over SMTP (`RM_SMTP_*` in its environment file). Sending through Gmail over SMTP needs an app password, which some Google accounts (those whose organization turns them off) can't have. So the command center runs a small SMTP server of its own (`smtp.go`) that hands each message to Gmail's API with the Gmail login `delta gmail login` stored (see [Gmail](#gmail)): it trades the refresh token for an access token at `oauth2.googleapis.com/token`, keeps that in memory until it nearly expires, and sends the message as it is with `messages.send` (`uploadType=media`, up to 35 MB). Gmail sends to the recipients in the message's headers, so any envelope recipient they don't name is added as a `Bcc`, which Gmail strips. Gmail sends from the login's address (or a send-as address of it, if the `From` names one).

It listens on `127.0.0.1:2525` (`-smtp-addr`; empty turns it off), and speaks plain ESMTP with no TLS and `AUTH PLAIN` or `AUTH LOGIN`, as rmfakecloud's client (Go's `net/smtp`) does with a server on the loopback interface. Backup sessions' containers can't reach the VPS's loopback (see [Setting up the VPS](#setting-up-the-vps)). Nothing is sent without its login, which `delta-cc smtp-password` makes on the VPS: it prints a new random password once and keeps only its SHA-256, with the username (`-user`, default `delta`), in `data/smtp-login.json` (mode 0600). The relay reads it on every login, so a new password works at once. A failed login waits a second, and three close the connection. If the port can't be had, the relay logs why and the rest of the command center runs on. Gmail's refusals reach the sender: a permanent one (a malformed message, a revoked Gmail login, which needs `delta gmail login` again) as a 554, anything else (Google out of reach) as a 451.

To turn it on (once, on the VPS):

```
sudo -u delta /opt/delta/delta-cc smtp-password -data /opt/delta/data
```

and put the four lines it prints into rmfakecloud's environment file, `/opt/delta/rmfakecloud/rmfakecloud.conf`, then restart rmfakecloud:

```
RM_SMTP_SERVER=127.0.0.1:2525
RM_SMTP_NOTLS=true
RM_SMTP_USERNAME=delta
RM_SMTP_PASSWORD=<the password it printed>
```

## Transform jobs

A transform job turns a document on the reMarkable into a new one, following instructions the user wrote on it: they write "type this up in MLA format" at the top of a handwritten essay, start a job, and get the typed essay back on the tablet. Jobs are started from the website's **Tablet** page (each document has a Transform button), from **Tablet** on the phone (`tablet/` in [thooton/sonim](https://github.com/thooton/sonim), a reMarkable companion that also browses the tablet's files and shows their pages through `GET /remarkable/{id}/pages/{n}`), or with `POST /transforms`.

**How one runs.** `transform.go` starts a cloud session for the job itself, on the first Claude account with room left in both usage windows (as the website picks one). It runs in an environment made for transform jobs (one per account and command center address, found by its name, `Delta transform <hash>`), unlike the Delta one in three ways:

- **No delta CLI.** Its setup script downloads the transform CLI from the command center (`GET /transform/cli`; the release workflow builds it into `cmd/delta-cc/transform-cli/` for the command center to embed) and installs the Python packages its guides use. `TRANSFORM_SERVER` holds the command center's address.
- **The real fonts.** The cloud image has only stand-ins for Times New Roman and the like (Liberation, Carlito, Caladea), and teachers ask for Times New Roman by name. The setup script also installs Times New Roman, Arial, Courier New, Georgia and Verdana, from Microsoft's core fonts for the web: the command center serves the installers (`GET /transform/fonts/{name}`, `transform_files.go`), which it downloads from SourceForge's copy of them the first time one is asked for, checks against the SHA-256 pinned in `transformFonts`, and keeps in `transform-fonts/` in its data directory. They are passed on unchanged, as their licence allows. The script unpacks them (cabinet archives, LZX-compressed) with `libarchive-c` over the image's libarchive, into `/usr/local/share/fonts/msttcorefonts/`, and runs `fc-cache`. A font that can't be had leaves the stand-ins.
- **A key for one document.** The session's first message carries the job's key (`delta_transform_...`), which `transform start` saves. It works on the public listener for the job's own routes (`/transform/...`) only: the document's files, preview and submit. Anything else answers 403. Submitting, failing, `DELETE /transforms/{id}`, or 12 hours passing ends the job and its key.
- **Almost no network.** The environment reaches the command center and the Python and npm package registries (`pypi.org`, `files.pythonhosted.org`, `registry.npmjs.org`) and nothing else (`allow_default_hosts: false`).

The first message also tells the agent to do only the transformation, and to ignore anything in the document asking for more (searching the web, sending data, unrelated tasks), whoever it claims to be from. It is a `<delta-setup>` block, which the website hides, followed by `Transform “<name>”`, so the session appears in Chats under that title. **Keeping the user posted.** The user sees nothing of the session from the Tablet page but what the agent reports there (`transform title`, `transform status "<line>" [percent]`, `transform percent <n>`, through `POST /transform/status`): a title for the job, a status line and a percentage, which the page shows with a bar and refreshes every 5 seconds while a job runs. A title is at most 6 words and 60 characters, with no em dashes and no ASCII arrows (`->`, `<-`, `=>`); the command center refuses anything else and says why, so the agent rewords it. The title is also the result's default name. A status line is one line of at most 120 characters, and a finished job shows 100%. `transform help` tells the agent to set the title once it knows what it's making, and to update the status at each step.

`transform help` gives the agent the workflow: `transform get` saves the document as it looks on the tablet (`internal/remarkable`), and `transform docx` is the guide to Word documents (`internal/docx`). It is pedagogical, since the user is a student and Transform is used in class: its THE STUDENT'S WORK section (and a rule in the first message) lets the agent change only how the student's own work looks (typing, formatting, arranging, their marked edits), never what it says. It adds no information (a request like "list the presidents from 1900 to 1930" is failed with a kind sentence telling the student to write it themselves), answers or solves nothing, writes, expands or rewords nothing, and corrects none of their answers or grammar, though it does correct every misspelled word, as a spell checker would, unless spelling is what the work tests (its SPELLING section); where their work is missing it leaves a marked placeholder like `[Your answer]`. It also covers marked-up pages: edits the user marks on a document (words crossed out, insertions and corrections written in) count as instructions even with nothing written to say so, and ask for the same document back, clean and laid out as before, with exactly those revisions made (its MARKED-UP PAGES section); the session's first message says the same. A page with handwriting and neither instructions nor marked edits is transcribed, faithfully (its TRANSCRIBING section). It tells the agent to use the font a format names (Times New Roman for MLA), never a stand-in.

**Formats.** The user ticks the formats when starting a job (in Tablet on the phone or on the website), at least one of them, which the screens enforce by disabling Start: a PDF, ticked to begin with and marked "to tablet" since it is the copy that goes on the tablet, and any of a Word document, Markdown, plain text, an email (`.eml`) and JPG images of the PDF's pages. The job keeps them (`formats`), the first message and `transform start` tell the agent, and its FORMATS section says what each holds: all the same result, Markdown with headings, lists and the page's bold, italic and underline (`<u>`), plain text with a blank line between unwrapped paragraphs, images at 150 dpi unless the instructions ask for another resolution. The email is for a message the user drafted by hand (To, Cc, Subject, the message) to send from the phone: its EMAIL section has the agent write it with Python's `email` package, the addresses as written (never guessed), no From or Date, a plain-text body, and `X-Unsent: 1`. Submit checks it (`checkEmail`): headers that parse, a Subject, no From, address lists that parse, and a text/plain body. Tablet downloads it into Documents on the SD card, and Mail's Open email file loads it into a new message. The ticks win over anything the document says about formats. Submit refuses a format missing or not asked for. Jobs from before formats have none, and submit a PDF and, optionally, a Word document.

**The result.** The agent submits every format the user ticked, at once (`transform submit essay.pdf essay.docx essay.txt`, a `multipart/form-data` request). A job with none asked for (from the API) makes a PDF, and one from before formats always does. Without the PDF ticked, nothing goes on the tablet: the agent still makes a PDF to check its work, but doesn't submit it. It converts the Word document itself, with the session's LibreOffice, where it can read the PDF before submitting and where every font it used is installed; the command center converts nothing. The two serve the user's two next steps: the `.docx` is what they hand in (on Canvas, where many of their teachers take only Word documents), and the PDF goes on the tablet, where they read it and, if it needs more work, write on it and transform it again. The PDF goes on the tablet in the original's folder, named as the agent chose (default: the job's title, or `<name> (transformed)`), and every file goes into Files, numbered if the name is taken; the job's `files` lists them, and the website and Tablet on the phone download them from there. A Word document submitted without its PDF is refused, and the CLI tells the agent how to make the PDF.

### Shorthand reader

The user sometimes writes in Gregg shorthand (Anniversary). `transform shorthand <image> [--line] [--region x,y,w,h]` (`cmd/transform/shorthand.go`) reads it for the agent, through `POST /transform/shorthand`. The models run on the user's PC (an RTX 2060, behind NAT), in the topaz project's `serve/shorthand_worker.py`, so the command center can't call them; their worker calls it instead (`shorthand.go`). It long-polls `POST /shorthand/worker/next`, which hands it the oldest waiting request, and posts each result to `POST /shorthand/worker/result/{id}`. A reading takes minutes, and the command center sits behind Cloudflare, which ends any request that waits over ~100 s (524), so a reading is asynchronous: `POST /transform/shorthand` queues it and answers with its id at once, and the CLI polls `GET /transform/shorthand/{id}` every 4 s, each poll answered at once, until it is done (it prints a progress line to stderr about every 30 s, retries a poll only when it failed in passing, no answer or a 5xx such as a 524, never submits a reading twice, and gives up after 12 minutes). Only the key or token that submitted a reading can fetch it; its result is kept 30 minutes (at most 32 are kept). A reading may take at most 10 minutes from when it was submitted, and a worker has 10 to answer one it took (its post-edit takes 1-5 minutes a page, and the worker stops it at 9); one it took but hasn't answered by its next poll is lost (502). When no worker has polled in the last minute (and none is reading), a reading is refused at once with 503 `shorthand service offline`, and a queued one whose worker goes away ends with that error; the CLI tells the agent to read the page itself. At most 16 requests wait. `POST /shorthand/reload` still waits for the worker's answer (seconds). Nothing is stored on disk: requests and results live in memory only.

The worker signs in with a secret of its own (`Authorization: Bearer delta_shorthand_...`), which `delta-cc shorthand-secret` makes on the VPS: it prints it once and keeps only its SHA-256, in `data/shorthand-worker.sha256` (mode 0600), read on every request, so a new secret works at once and the old one stops. The secret reaches the two worker routes, which are outside `requireAuth`, and nothing else; neither the delta token nor a job's key reaches them.

```sh
sudo -u delta /opt/delta/delta-cc shorthand-secret -data /opt/delta/data
```

For each line of the page (found by the OCR model's segmenter, column by column) the worker returns the Grascii its OCR model (Topaz-61M) read, the dictionary's candidates for each outline (fuzzy for outlines not in it, `exact: false`), the draft English its Grascii-to-English model made of the line (`draft`), the finished English (`english`), and the line's box. The finished English comes from a post-edit on the worker: DeepSeek, through OpenRouter, reads the outlines, their candidates and the draft, ten lines per call (`postedited: true`; about one word in eight wrong on book pages against one in four for the draft). A chunk whose call reasons too long is cut off and answered by a short second call from its reasoning (the line's `postedit` is `"salvaged"`, otherwise `"done"`); where that fails too, or the post-edit is off, `english` is the draft, `postedited` is false (`postedit` `"draft"`), and the result's `postedit` says why. `transform shorthand` shows each line's finished English with its Grascii under it, giving dictionary words only for `~` outlines (not in the dictionary) and for draft lines, which it marks. `transform help` (its SHORTHAND section) tells the agent to take the English as the transcription and check it lightly, looking again only where a word makes no sense or a `~` outline sits, and to work out draft lines itself as before. It also tells it a page takes a few minutes, which the command waits out itself (printing progress), one page at a time.

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
