package main

const gmailHelp = `delta gmail: the user's Gmail

  Read, search, send, reply to, forward, archive, label and trash the
  user's email. Run "delta set-auth" first (see "delta help").

IDS
  Every id is a message id (like 19a2b3c4d5e6f708). search lists
  conversations, each by its latest message's id. read shows the whole
  conversation of any message in it. trash, archive, mark and label act on
  the whole conversation, as in the Gmail app; reply, forward and
  attachment use that one message (read shows each message's id).

READING
  search [query] [options]  List conversations, newest first. The query
                            uses Gmail's search syntax; without one, the
                            inbox. Shows each one's latest sender, date,
                            subject, a snippet, labels and UNREAD.
    -n, --limit N           How many (default 10, at most 50).
    --page TOKEN            The next page (search prints the command).
  read <id> [--full]        The whole conversation: each message's
                            sender, recipients, date, attachments and
                            text (HTML mail is shown as Markdown, links
                            kept). Quoted copies of earlier messages are
                            hidden and very long messages cut; --full
                            shows everything. Reading doesn't mark
                            anything read.
  attachment <id> [n...] [-o dir] [--push]
                            Save a message's attachments (all, or the
                            numbers read lists) into the current
                            directory or dir. --push also uploads each to
                            the command center, for the user's Files.
  labels                    List the labels.
  account                   The account, its unread count and the
                            addresses it can send from.

  Search syntax: from:amy to:me subject:invoice "exact words"
  is:unread is:starred has:attachment filename:pdf label:work
  in:inbox in:sent in:spam in:trash in:anywhere category:primary
  newer_than:7d older_than:1y after:2026/10/01 before:2026/10/14
  larger:5M  -excluded  (a OR b)
  e.g. delta gmail search is:unread -category:promotions newer_than:3d

WRITING
  send --to <addresses> --subject <subject> <text> [options]
  reply <id> <text> [--all] [options]
                            Reply to the sender (or Reply-To), in the
                            same conversation, quoting the message. --all
                            copies everyone else on it. Replying to one of
                            the user's own messages goes to its recipients.
  forward <id> --to <addresses> [<text>] [options]
                            Forward the message with its attachments and
                            formatting, with an optional note on top.
  send-draft <id>           Send a draft (one saved with --draft, or any
                            in the user's drafts).

  <text> is --body "..." or --body-file FILE, where FILE "-" reads stdin
  (best for anything long):

    delta gmail reply 19a2b3c4d5e6f708 --body-file - <<'EOF'
    Hi Amy,

    Thursday works. See you then.
    EOF

  Options:
    --to, --cc, --bcc <addresses>   Comma-separated, each "a@b.com" or
                                    "Name <a@b.com>"; repeatable. For a
                                    reply, they add to who it goes to.
    --attach <file>                 Attach a file; repeatable (about 25 MB
                                    in all).
    --draft                         Save to the user's Gmail drafts
                                    instead of sending.

  Mail is sent from the user's default address (or, for replies and
  forwards, the one the message was sent to), as plain text: write it
  the way the user would, without Markdown. Gmail doesn't add their
  signature.

  Sending can't be undone. Unless the user told you exactly what to send
  and to whom, show them the recipients and the text first and wait for
  their OK. Ask before trashing or reporting spam unless they asked for it.

ORGANIZING
  archive <id>...           Take conversations out of the inbox.
  unarchive <id>...         Put them back in the inbox.
  trash <id>...             Move them to the trash (Gmail empties it after
                            30 days). untrash <id>... brings them back.
  mark <what> <id>...       what is read, unread, starred, unstarred,
                            important, unimportant, spam or not-spam.
  label <label> <id>...     Add a label (by name, any case). --create
                            makes it if it doesn't exist.
  unlabel <label> <id>...   Remove a label.

SETUP (the user does this, on a computer)
  The Gmail login is kept on the command center; you never see it. If a
  command says Gmail isn't set up, or that the login expired or was
  revoked, ask the user to run this on a computer with a browser:

    delta gmail login <client-secret.json>

  where the file is the "Desktop app" OAuth client they made in Google
  Cloud for Delta (the thooton/delta AGENTS.md explains how).
  "delta gmail logout" removes and revokes the login.

  Times are in the user's time zone when known (DELTA_TZ, or their Canvas
  profile's), else this machine's; each shows its zone.
`
