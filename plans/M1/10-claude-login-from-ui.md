# M1.10 — Configuring the Claude login from the UI

This document is the reasoning. The step by step that implements it is
[10-claude-login-from-ui-implementation.md](10-claude-login-from-ui-implementation.md),
and it is the plan to follow: read this one first for why the design is what it
is, then work through that one in order.

## What was asked

> Configurazione claude da interfaccia grafica.
> Deve essere possibile configurare la login di claude da interfaccia grafica.

## Reading of the request

Hexagon runs Claude Code for a living, and the one thing it cannot do today is
say which Claude account it runs as. That has to be arranged on the host, by
hand, before the browser is any use at all.

So the point is not "add a settings field". It is: **whoever can sign in to
Hexagon can put it in a state where sessions have a working Claude Code, without
ever getting a shell on the machine.** Two things follow from taking that
literally.

There are two kinds of credential a person can bring, and they are not the same
object. One is a string you can paste — an API key from the Console, or the
long-lived token `claude setup-token` prints. The other is a subscription, which
is not a string at all: it is an OAuth flow that ends with Claude Code writing a
file. A page that only accepts a pasted string sends the subscriber back to the
host shell, which is the thing the point exists to remove.

And "configure the login" implies seeing it. A page that swallows a credential
and says nothing is a page you cannot debug: the honest surface says what is
stored, what is on disk, and which of the two a new session will actually use.

The user settled the rest when this was written up: **both** ways in; the
browser login writes the file the configuration already names, rather than a new
private copy; the Dockerfile editor uses the same credential; and there is one
Claude login per user, with no per-session choice.

## Current behaviour

Claude Code authentication reaches a container from exactly two places, both
fixed by the process configuration and neither reachable from the browser.

`containerSpec` puts the configured API key in the environment:

```go
if m.cfg.AnthropicAPIKey != "" {
	env = append(env, "ANTHROPIC_API_KEY="+m.cfg.AnthropicAPIKey)
}
```

and bind mounts the configured credentials file over the one place Claude Code
looks for it:

```go
// Read-only: the container gets to use the credentials, not to change them.
if path := m.cfg.ClaudeCredentials; path != "" {
	if _, err := os.Stat(path); err == nil {
		binds = append(binds, path+":"+dockerx.AgentHome+"/.claude/.credentials.json:ro")
	} else {
		m.log.Warn("claude credentials not found, sessions will need their own login",
			"path", path, "err", err)
	}
}
```

That warning is the whole problem in one line. When the file is missing, every
session starts with a Claude Code that asks to be signed in, in a container that
is thrown away, and Hexagon's answer is a log entry the user never sees.

There is a third consumer, and it is the one nobody would guess. `internal/claudex`
runs `claude -p` on the host for the Dockerfile editor and never sets `cmd.Env`,
so it inherits the server process's environment and reads the *server user's*
`~/.claude/.credentials.json`. The Images page spends whoever started the process's
quota. `05-dockerfile-editor.md` says so deliberately; what it did not anticipate
is a product in which the Claude login is a thing the user chooses.

Nothing in `store` holds an Anthropic credential. The nearest machinery is
`provider_accounts` — one sealed secret per user per provider, connected through
`PUT /api/accounts/{provider}` after being verified — and that is the shape to
copy, not the table to reuse: a repository provider is something
`provider.Registry` lists and `provider.Lister` fetches from, and Anthropic is
neither.

## Design

### Two ways in, because there are two kinds of credential

The Accounts page grows a **Claude** card with both:

- a field that takes a pasted credential — an API key, or the token from
  `claude setup-token` — verified before it is stored, sealed in the database;
- a **Log in** button that opens a dialog with a real terminal in it, running
  `claude` inside a container, where the ordinary `/login` flow happens.

Neither replaces the other. The pasted credential is what a Console account and
a headless setup want. The login terminal is the only thing that serves a
subscription, and it is the one that makes the point's promise true.

### Each kind is stored where it belongs, and no conversion happens

| Input | Where it is kept | How it reaches a container |
|---|---|---|
| Pasted key or token | `claude_credentials`, sealed with the server key | `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` in the environment |
| Browser login | The path `claude.credentials` already names, written by Claude Code itself | The bind mount that already exists, unchanged |

The asymmetry is deliberate. A pasted string is an environment credential and
has no file form Hexagon is entitled to invent — `.credentials.json` is a
document Anthropic owns, with an expiry and a refresh token in it, and writing a
hand-built one is how you get a session that fails in a way nobody can read. A
login, conversely, *is* a file, produced by the only program that knows the
format.

So the browser login does not capture anything. The container it runs in has the
host's Claude Code directory bind mounted at `$HOME/.claude`, and `claude`
writes `.credentials.json` there itself — into the very file `containerSpec`
already mounts into every session. Nothing is copied, nothing is parsed, and the
host's own `claude` CLI is signed in as a side effect, which is the behaviour
that was asked for.

**Rejected: sealing the credentials file into the database** and materialising
it per session. It buys a uniform storage story and costs a format Hexagon has
to keep up with, a refresh that now has two owners, and a per-session copy that
goes stale silently. The configuration already names a file; the login writes it.

### What a session gets, and the shadowing that comes with it

```
1. the credential this user stored in the UI   -> ANTHROPIC_API_KEY / CLAUDE_CODE_OAUTH_TOKEN
2. cfg.AnthropicAPIKey                         -> ANTHROPIC_API_KEY
3. cfg.ClaudeCredentials                       -> the read-only mount, as today
```

The first two are the same slot, so the stored credential simply outranks the
configured one; the third is a different mechanism and is left exactly as it is.
Nothing that works today stops working: a machine configured entirely from a
file keeps behaving as it did, and the UI is the layer above it.

What the list does not say, and the page must, is that **inside the container an
environment credential wins over the mounted file**. A user who pastes an API
key and then also signs in through the terminal has two logins and is using the
key. That is Claude Code's precedence, not Hexagon's, and it is better named than
worked around: the status endpoint reports which source is *effective*, and the
card says it in a sentence.

### When a change takes effect

Two different answers, for two different mechanisms, and both are properties of
Docker rather than choices:

- **A pasted credential reaches sessions created after it was stored.** It is an
  environment variable, and a container keeps the environment it was created
  with — the same fact that made point 7's token switch a creation-time flag.
- **A browser login reaches an existing session at its next start.** The mount
  is a path, resolved when the container starts, so stopping and starting a
  session picks up the file that is there now. A *running* container keeps the
  file it was given.

Both sentences belong in the UI, next to the control they describe. The
alternative is a user who signs in, sees no change in a running session, and
concludes the feature is broken.

### The login container

Created on demand when the dialog opens, named `hexagon-claude-login-<userID>`,
from a ready image the user picks — every Hexagon image has `claude` and `tmux`
on the PATH by the contract in `deploy/images/base/Dockerfile`, so any of them
will do, and the picker exists because the server cannot know which.

Four details are load bearing.

**`$HOME/.claude` is the host's directory, read-write.** That is the entire
mechanism: `filepath.Dir(cfg.ClaudeCredentials)` bind mounted at
`/home/agent/.claude`, inside a `HOME` that is Hexagon's own scratch directory.
The nested bind is the shape `containerSpec` already uses — the session home,
with the credentials file mounted inside it.

**The container gets no Anthropic environment at all.** A container that already
had `ANTHROPIC_API_KEY` would consider itself authenticated, and the login flow
would be theatre performed on a credential the user is trying to replace.

**It is attached with one exec, not a bootstrap and then a terminal:**

```sh
tmux new-session -A -D -s login -c /home/agent 'claude; exec "${SHELL:-sh}"'
```

`-A` is what makes a browser reload rejoin the login in progress rather than
start a second one on top of it — an OAuth flow that restarts halfway is the
failure this design has to avoid, and the terminal component already reconnects
on its own. The shell fallback is there for the reason recorded in
`03-auto-claude-switch.md`: if the command a tmux session was created with exits,
the session goes with it, and the user is left looking at a socket that closed.

**It is not a managed container.** The label is `hexagon.role=claude-login` and
`hexagon.managed` is absent, so `ListManagedContainers` does not see it and
`Reconcile` does not report it as an orphan at every startup. It is removed when
the dialog closes, and a leftover from a crashed server is removed by name
before a new one is created.

The cost is worth stating rather than hiding: while that dialog is open, a
container has read-write access to the host user's entire Claude Code directory.
That is the same boundary the README already draws — whoever reaches the port
controls the Docker socket — and it is the direct consequence of the decision
that the login writes the host's own file.

**Rejected: running the login on the host under a pty.** It needs a new
dependency for something `dockerx.AttachExec` already does, and it would put an
interactive Claude Code in the server process's own environment, which is
exactly the arrangement this point is dismantling.

### Verifying a pasted credential with the thing that will use it

`handleConnectAccount` verifies a provider token before storing it, because a
mistyped token that was stored anyway shows up later as an account that lists
nothing with no clue why. The same argument applies here, and Hexagon already
has the right instrument: `internal/claudex` runs `claude -p` with every tool
removed. A one-line prompt with the candidate credential in the environment
answers the only question worth asking — *can Claude Code authenticate with
this* — and it does so without a hand-written Anthropic HTTP client, whose
notion of a valid credential would be its own rather than the CLI's.

When the server has no `claude` binary the credential is stored unverified, and
the status says so. That is the `canAsk` rule from the Images page: leave the
claim out rather than make one that is not backed by anything.

### The Dockerfile editor stops borrowing the host's login

`claudex.Runner` gains a `Credential` and passes it in the subprocess
environment. The type lives in `claudex` because that package is the one that
owns *how the Claude Code CLI is spoken to*; the names `ANTHROPIC_API_KEY` and
`CLAUDE_CODE_OAUTH_TOKEN` then appear in exactly one place, and `internal/auth`,
`internal/session` and `internal/httpapi` all read them from there.

With no credential stored the runner behaves exactly as it does today —
inheriting the server's environment and the server user's login — so this is a
strict addition.

### The page, not a new page

The card goes on `AccountsView.vue`. It is already the place where credentials
are configured, it already has the shape this needs (a row per thing, an inline
form that opens, a hint explaining precisely which token to create), and a
second settings page whose only content is one card would be a worse answer to
"where do I configure things".

The login terminal is a dialog over that page rather than a route of its own: it
is a modal task with an end, and the user is coming back to the card to see that
it worked.

`TerminalPane.vue` builds its own URL from a `sessionId` prop. It takes a `url`
instead, so the login dialog can point it somewhere else. That is a smaller
change than a second copy of the xterm wiring, and the component was always
about a socket rather than about sessions.

## Impact

| Area | Change |
|---|---|
| Schema | `006_claude_credentials.sql`: one row per user, `kind` checked against `('api_key', 'oauth_token')`, `secret_enc` sealed |
| Store | `ClaudeCredential` model and the `ClaudeKind*` constants; `UpsertClaudeCredential`, `ClaudeCredential`, `DeleteClaudeCredential` |
| claudex | `Credential` with `Env()`; `EditDockerfile` takes one; new `Check` |
| auth | `SetClaudeCredential`, `ClaudeCredential`, `ForgetClaudeCredential` — the cipher still never leaves the package |
| dockerx | `LabelRole`. `AttachExec`, `CreateContainer` and `RemoveContainer` already do the rest |
| Session | `CredentialSource` gains `ClaudeCredential`; `containerSpec` prefers it over `cfg.AnthropicAPIKey`; the mount is untouched |
| API | `GET /api/claude`, `PUT`/`DELETE /api/claude/credential`, `GET /api/claude/login/terminal`, `DELETE /api/claude/login`, all protected |
| Config | none. The README row for `claude.credentials` gains a sentence: it is also what the browser login writes |
| UI | a Claude card in `AccountsView.vue`, a login dialog around `TerminalPane`, `api.claude` |

## Verification

In `internal/httpapi`, against the `testEnv` that already inspects the specs the
fake Docker was handed:

- a session created while a credential is stored gets its environment variable,
  and one created with none falls back to `cfg.AnthropicAPIKey` — the two halves
  of the resolution order, asserted together because either alone would pass
  with the rule half written;
- the credentials mount is present in both, unchanged: the test that proves the
  new path did not disturb the old one;
- `PUT /api/claude/credential` with an empty secret is a 400, one the fake
  editor rejects is a 400 carrying the CLI's own message, and a stored one comes
  back from `GET /api/claude` as a kind and a timestamp and **never as a
  secret** — that last assertion is the point of the endpoint's existence;
- `GET /api/claude/login/terminal` creates a container whose binds include the
  host credentials directory at `/home/agent/.claude` and whose environment
  contains no Anthropic variable, and `DELETE /api/claude/login` removes it;
- without a cookie every one of them is a 401.

In `internal/claudex`, extending the tests that already point the binary at a
script in `t.TempDir()` recording its argv: the credential reaches the child's
environment under the right name for its kind, and `Check` turns a non-zero exit
into an error that names what went wrong.

In `internal/store`, a migration test row: an existing user comes out with no
Claude credential, and the `CHECK` refuses a third kind.

By hand, which is the only thing that proves the terminal really carries a
login: with no credentials file on the host, open the card, press **Log in**,
complete `/login` in the browser, and watch `~/.claude/.credentials.json`
appear. Then start a session and confirm `claude` comes up signed in. Then stop
and start a session that was already running from before the login, and confirm
it picks the file up — the claim about when a login takes effect is the one most
likely to be wrong.
