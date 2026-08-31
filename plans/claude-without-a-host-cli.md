# Claude Code on a server that has none

*Written after the first packaged install, from two things the user hit in the
same sitting.*

> se non ho fatto prima una build non posso configurare claude, usa una build di
> default. Altra cosa: dopo che ho configurato claude non vedo "ask" claude per
> dockerfile e docker compose.

Two reports, one cause underneath: **Hexagon needed a Claude Code that was not
there, and said nothing about it.**

## What was actually wrong

The browser login runs `claude auth login` in a throwaway container, so it needs
an image with the CLI inside. The dialog offered a menu of the user's own ready
images — and on a fresh install that menu is empty, with no way forward and no
explanation. Nothing was broken; there was simply nothing to pick.

"Ask Claude" is a different mechanism with the same shape of failure.
`internal/claudex` runs `claude -p` on the **host**, and the Images page hides
the box when there is no binary (`canAsk`). A packaged install never has one: the
`hexagon` service user has no npm, no login shell and no home to install into.
So the box is absent, permanently, and the page cannot explain the absence of a
control it does not draw. Configuring a credential on the Accounts page does not
help and cannot: a credential authenticates a CLI, it does not provide one.

That last point is the trap. From the outside, "I configured Claude" and "Claude
features work" look like the same thing.

## The decision

Both features need the same thing — *a container with Claude Code in it* — and
Hexagon already knows how to make one: it is the reference image the Images page
offers as a starting point, and it is embedded in the binary. So:

**`claudex.DefaultImage`** builds that Dockerfile for Hexagon's own use, under a
tag carrying a hash of the file, so editing the reference produces a new image
rather than a stale one under the same name. It is built at most once per
process, lazily — a server whose user only ever pulls registry images should not
spend a build on this — and:

- **asking for it is what starts the build, and the ask never waits.** A build
  is minutes; a request that blocked on one would be a browser holding a socket
  open for as long as an image takes, behind whatever proxy is in front of the
  server. A caller that finds it missing is told so, and asks again.
- **a failed build is reported with the daemon's own words and left alone for a
  minute.** The Accounts page polls; without that interval a daemon out of disk
  would be asked to retry several times a minute forever.

**The login** then uses that image when the request names none, which is the
default in the dialog until the user has images of their own.

**The editor** gains a second implementation, `claudex.Container`, chosen at
startup when there is no binary. The user asked for exactly this shape:

> container come fallback, ma in questo caso avvisa l'utente che claude non e'
> installato sul server e viene eseguito in un container (piu' lento)

So the binary wins wherever there is one — it answers in seconds, where the
container path pays for a container on every call and an image build on the
first — and the API says which of the two the server got (`askInContainer`,
`inContainer`), so the box under each editor can say it plainly instead of just
feeling slow.

## What the container path had to get right

- **Nothing variable is interpolated into the shell command.** The prompt is
  whatever a user typed and the schema is a JSON document; a command built from
  either is a quoting bug waiting for the input that triggers it. Both travel in
  the container's environment, and the script names them — `claude -p
  "$HEXAGON_PROMPT" … --json-schema "$HEXAGON_SCHEMA"`. The script text itself is
  fixed, chosen in Go, with the optional flags decided there rather than by shell
  expansion.
- **The CLI's diagnostics are kept out of its answer.** `RunExec` gets one
  stream: the daemon folds an exec's stdout and stderr together, and a warning
  printed beside a good reply would make it unparseable. So stderr goes to a file
  in the container and is read back only when the call failed.
- **A failure prefers the answer over the exit status.** Claude Code reports a
  refused credential in both, and only one of them says *which* thing went wrong.
  A run that exits non-zero but produced the JSON envelope is handed on to be
  read, so the user gets "Not logged in · Please run /login" instead of "exit
  status 1". The host runner had the same weakness and got the same fix.
- **`$HOME` is seeded.** Without `$HOME/.claude.json` the CLI finds a machine it
  has never run on and starts its first-run onboarding, which for a
  non-interactive call is an answer that never arrives. The session containers
  already do this, for the same reason.
- **The container runs as the host user**, like everything else Hexagon starts.
  It has no ports and no name, its only mount is the read-only one described at
  the end of this document, and it is removed as soon as the one exec returns —
  including when that exec failed.

## Verified

`internal/claudex` over a fake daemon: the image is built once and in the
background, its tag follows the Dockerfile, a failed build reports why; the
prompt reaches the container's environment and never the command line, the
container runs as the host user and is removed after a failure as well as a
success, a non-zero exit reads the diagnostics file, and an answer that reports
an error wins over the exit status. `internal/httpapi` over the real router: a
login that names no image runs in the default one, one that arrives while it is
still building gets 409 and after a failed build 503, and the template reports
which way "Ask Claude" runs.

By hand, against a real daemon and the real reference image: the exact script the
code generates, with a prompt full of quotes, dollars and backticks, reaching
Claude Code intact and coming back as the parseable envelope. The one thing not
exercised that way is a successful authenticated call, which needs somebody's
credential.

## The half that was still missing

*Added the same day, from the next thing the user hit.*

> ora pero' configuro claude su accounts, ma se faccio "ask claude" mi dice
> "claude code reported an error: Not logged in · Please run /login". se avvio
> una sessione invece claude e' loggato correttamente

The container ran, and had nothing to run *as*. The editor was handed the
credential this user had pasted and nothing else, while a session container has
three ways to be authenticated and takes the first that is there: the pasted
credential, the key the server was configured with, and — the one that matters
here — the host's own credentials file, bind mounted read-only, which is exactly
what the browser login on the Accounts page writes.

So a user who signed in through the browser had a working session and an editor
that said "Not logged in", for the same account, on the same server, minutes
apart. The host runner hid this on a developer machine: it inherits the server
user's environment and home, so it finds that same file by accident.

The fix is to stop improvising in two places. `httpapi.editorCredential` resolves
the same order of preference `session.containerSpec` does, and the container
runner mounts the credentials file when it is handed no credential at all — read
only, and copied into the container's throwaway home before use, because the CLI
rewrites that file to refresh an expiring token and what it writes belongs to a
container that is about to be removed rather than to the host's own login.

Verified by hand as well as in tests: with a deliberately invalid credentials
file mounted, the CLI in the container answers "Failed to authenticate: OAuth
session expired" instead of "Not logged in · Please run /login". The message
changing is the proof that the file is being found and read; a valid one is the
same path with a different answer.

## What was deliberately left alone

The image is Hexagon's own and not a row in the user's Images list. A row would
be theirs to rename, edit or delete, and both features would then break in a way
that looks like a bug in them rather than a consequence of that. It is the same
kind of thing as the code-server release the server downloads for itself: part of
the machinery, not part of the user's data.
