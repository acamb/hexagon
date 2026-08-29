# M1.10 — implementation plan

The reasoning is in [10-claude-login-from-ui.md](10-claude-login-from-ui.md),
and it is settled: read it first, then follow the steps here in order. Where
this file gives an exact name, signature or snippet, use it as written — the
shapes were checked against the code, and the comments record constraints that
are not visible from the call site.

Nothing here is a suggestion to redesign. If a step turns out to be wrong, say
so and stop rather than substituting a different design; the analysis records
what was rejected and why.

Read [../../AGENTS.md](../../AGENTS.md) before the first edit. Two of its rules
are easy to break in this change: comments say **why** and never **what**, and
everything that lands in a file is in English.

## 0 — Two things to check before writing any code

Everything else in this plan is grounded in this repository. These two are not,
and they are cheap to settle.

**The name of the environment variable for a subscription token.** Run
`claude setup-token --help` and `claude --help`. Confirm that `setup-token`
exists and that the token it prints is read from `CLAUDE_CODE_OAUTH_TOKEN`. If
the CLI documents a different name, use that one and change the single constant
in step 2 — nothing else in this plan depends on the spelling.

**That a restarted container re-reads the bind mounted credentials file.**
Create a session, stop it, touch `~/.claude/.credentials.json` with recognisable
content, start it, and `docker exec` a `cat` of
`/home/agent/.claude/.credentials.json`. If the old content comes back, the
claim in the analysis is wrong: no code changes, but the UI sentence in step 8
becomes "sessions created after the login", and say so in an
`## Amendment` section at the end of the analysis, in the style of the two in
[06-additional-providers.md](06-additional-providers.md).

## 1 — Schema and store

`internal/store/migrations/006_claude_credentials.sql`:

```sql
-- The Anthropic credential a user configured from the UI. It is handed to
-- session containers, and to the host's `claude -p`, as an environment
-- variable.
--
-- One row per user, so user_id is the primary key rather than a uuid with a
-- UNIQUE beside it: there is nothing to list and nothing to order, and a second
-- credential for the same person would only raise the question of which one
-- wins.
--
-- This is one half of "the Claude login". The other half is a subscription,
-- which is a file Claude Code writes at claude.credentials on the host in a
-- format Hexagon does not own; see plans/M1/10-claude-login-from-ui.md.
CREATE TABLE claude_credentials (
    user_id    TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    -- kind decides which environment variable carries the secret. The allowed
    -- values are claudex.KindAPIKey and claudex.KindOAuthToken: the meaning of
    -- the kind is which name the CLI reads, so it is that package's word.
    kind       TEXT NOT NULL CHECK (kind IN ('api_key', 'oauth_token')),
    secret_enc BLOB NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
```

New file `internal/store/claude_credentials.go`, in the shape of
`provider_accounts.go`:

```go
// ClaudeCredential is the Anthropic credential a user configured from the UI.
// The secret is sealed with the server key and only internal/auth ever opens
// it.
type ClaudeCredential struct {
	UserID string
	// Kind is claudex.KindAPIKey or claudex.KindOAuthToken. It is kept as a
	// plain string so this package goes on depending on nothing above it.
	Kind      string
	SecretEnc []byte
	CreatedAt time.Time
	UpdatedAt time.Time
}

const claudeCredentialColumns = `user_id, kind, secret_enc, created_at, updated_at`
```

Three methods, all scoped to the user:

- `UpsertClaudeCredential(ctx, *ClaudeCredential) (*ClaudeCredential, error)` —
  `INSERT ... ON CONFLICT(user_id) DO UPDATE SET kind = excluded.kind,
  secret_enc = excluded.secret_enc, updated_at = excluded.updated_at`, then
  re-read through `ClaudeCredential`. **`created_at` is not in the update list**:
  it records when the user first configured one, and replacing a credential is
  not creating one.
- `ClaudeCredential(ctx, userID) (*ClaudeCredential, error)` — `ErrNotFound` on
  `sql.ErrNoRows`.
- `DeleteClaudeCredential(ctx, userID) error` — `ErrNotFound` when
  `RowsAffected() == 0`.

plus `scanClaudeCredential(row scanner)` parsing the two timestamps with
`parseTime`.

Note the deviation from CLAUDE.md, and do not "fix" it: the `kind` values are
**not** exported as constants from `store`. They belong to `internal/claudex`,
which is the package that knows what a kind is for, and `store` importing it
would put product knowledge in the storage layer. The migration comment above is
the pointer that keeps the three places in lockstep.

## 2 — `internal/claudex`

The package gains the credential, and it is the only place in Hexagon that
knows the variable names Claude Code reads.

```go
// Credential kinds, mirrored by the CHECK constraint in
// 006_claude_credentials.sql. The kind exists to choose a variable name, which
// is why it is defined here rather than beside the table.
const (
	KindAPIKey     = "api_key"
	KindOAuthToken = "oauth_token"
)

// Credential is how the CLI is authenticated. The zero value means "whatever
// the server process already has", which is the host user's own login and the
// behaviour this package had before there was anywhere to configure one.
type Credential struct {
	Kind   string
	Secret string
}

// Env returns the assignments that hand this credential over, and nothing at
// all for the zero value.
func (c Credential) Env() []string {
	if c.Secret == "" {
		return nil
	}
	if c.Kind == KindOAuthToken {
		return []string{"CLAUDE_CODE_OAUTH_TOKEN=" + c.Secret}
	}
	return []string{"ANTHROPIC_API_KEY=" + c.Secret}
}
```

A helper the two runs share:

```go
// environment returns the server's own environment with any Claude Code
// credential in it replaced by cred, or nil to inherit it untouched.
//
// Appending would not do. With the same name present twice it is the C library
// that decides which one the child sees, and a server started with its own
// ANTHROPIC_API_KEY would go on using it about as often as not — a bug that
// only shows up on the machine where the variable happens to be set.
func environment(cred Credential) []string {
	assignments := cred.Env()
	if len(assignments) == 0 {
		return nil
	}
	var out []string
	for _, kv := range os.Environ() {
		switch name, _, _ := strings.Cut(kv, "="); name {
		case "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN":
			continue
		default:
			out = append(out, kv)
		}
	}
	return append(out, assignments...)
}
```

`EditDockerfile` takes the credential as its first argument after the context —
a parameter rather than a `WithCredential` clone, because the credential belongs
to the request while the runner is built once at startup:

```go
func (r *Runner) EditDockerfile(ctx context.Context, cred Credential, dockerfile, instruction string) (DockerfileEdit, error)
```

and sets `cmd.Env = environment(cred)` next to the existing `cmd.Stdin`.

And a new method:

```go
// Check reports whether the CLI can authenticate with cred. It is the cheapest
// call that proves the thing that matters: a credential the API would accept
// but Claude Code does not know how to use is still one that fails in a
// session, an hour later, where nobody can see why.
//
// Every failure is one error. The CLI does not distinguish a refused credential
// from an unreachable API in its exit status, and a distinction invented here
// would send the user off to check the wrong thing.
func (r *Runner) Check(ctx context.Context, cred Credential) error
```

It runs `-p --safe-mode --strict-mcp-config --tools "" --output-format json`
with `Reply with the single word: ok` on stdin, applies `r.model` as
`EditDockerfile` does, and reuses the same three failure modes and `firstLine`.

## 3 — `internal/auth`

Three methods on `*Service`, next to `Connect` / `Credentials`. The cipher still
never leaves the package.

```go
// SetClaudeCredential stores the Anthropic credential this user configured,
// sealed. It has been checked against the CLI by the time it gets here.
func (s *Service) SetClaudeCredential(ctx context.Context, userID string, cred claudex.Credential) error

// ClaudeCredential returns what to run Claude Code as for this user, or the
// zero value when there is none. "None" is not an error: it is the ordinary
// state of a Hexagon configured from a file.
func (s *Service) ClaudeCredential(ctx context.Context, userID string) (claudex.Credential, error)

// ForgetClaudeCredential removes it, or reports store.ErrNotFound.
func (s *Service) ForgetClaudeCredential(ctx context.Context, userID string) error
```

`ClaudeCredential` maps `store.ErrNotFound` to `claudex.Credential{}, nil`, and
any other error through unchanged. It does **not** grow a "does one exist"
variant: the status endpoint reads `kind` and `updated_at` straight from
`s.store.ClaudeCredential`, and never asks for the secret it is not going to
show.

## 4 — `internal/dockerx`

One constant, beside `LabelManaged` and `LabelSessionID`:

```go
// LabelRole marks a container Hexagon created for something that is not a
// session. ListManagedContainers and the reconciler compare managed containers
// against session rows, and a container with no row would be reported as an
// orphan at every startup.
const LabelRole = "hexagon.role"
```

Nothing else. `CreateContainer` already takes binds, environment and labels;
`AttachExec` already opens a TTY with stdin attached; `RemoveContainer` already
swallows a container that is not there (`containers.go:130-137`), so removing a
leftover by name needs no guard. The *value* of the role stays with the code that
creates the container — `dockerx` has no business knowing Hexagon's roles.

## 5 — `internal/session`

**The credential reaches a container.** `CredentialSource` gains a method, and
`auth.GitCredentialSource` implements it by delegating to its `*Service`:

```go
// ClaudeCredential is what Claude Code inside the container authenticates
// with, or the zero value when the user configured none.
ClaudeCredential(ctx context.Context, userID string) (claudex.Credential, error)
```

`Create` resolves it beside the git credentials and passes it down through
`provision` → `provisionSteps` → `containerSpec`. `Start` does **not** need it:
the credential is part of the environment, and a container keeps the environment
it was created with.

In `containerSpec`, replacing the `AnthropicAPIKey` block:

```go
// A credential configured in the UI outranks the one the process was started
// with: it is the one the user can see, change and be told about.
switch {
case claude.Secret != "":
	env = append(env, claude.Env()...)
case m.cfg.AnthropicAPIKey != "":
	env = append(env, "ANTHROPIC_API_KEY="+m.cfg.AnthropicAPIKey)
}
```

The credentials mount below it is **not touched**.

**The login container.** It lives here rather than in `internal/httpapi` because
this is the package that creates containers, and it is the one that already has
the uid:gid they run as and the path to the credentials file. `Config` gains one
field:

```go
// ClaudeLoginDir holds the throwaway HOME of the container the browser login
// runs in, one directory per user.
ClaudeLoginDir string
```

set in `cmd/hexagon/main.go` to `filepath.Join(cfg.DataDir, "claude-login")`.

Constants beside `TmuxSession`:

```go
// The browser login attaches to its own tmux session, with -A, so a page reload
// rejoins the login in progress instead of starting a second one on top of an
// OAuth flow that is half done.
const ClaudeLoginTmux = "login"

// ClaudeLoginCommand is what that tmux session runs. `claude auth login` goes
// straight to the sign-in flow rather than the full interactive assistant —
// see the amendment in 10-claude-login-from-ui.md. The shell fallback is not
// decoration: if the command a tmux session was created with exits, the session
// goes with it, and the user is left looking at a socket that closed.
const ClaudeLoginCommand = `claude auth login; exec "${SHELL:-sh}"`

const claudeLoginRole = "claude-login"
```

and:

```go
// ErrClaudeLoginUnavailable reports that no credentials path is configured, so
// a browser login would have nowhere to write.
var ErrClaudeLoginUnavailable = errors.New("no claude credentials path is configured")

// StartClaudeLogin prepares the container the browser login runs in and returns
// its id. Any container left over from a previous attempt is removed first: it
// is throwaway, and a stale one would be attached to instead of a fresh one.
func (m *Manager) StartClaudeLogin(ctx context.Context, userID, imageRef string) (string, error)

// StopClaudeLogin removes it. The credentials it wrote are on the host, not in
// it, so there is nothing to keep.
func (m *Manager) StopClaudeLogin(ctx context.Context, userID string) error
```

`StartClaudeLogin`:

1. `ErrClaudeLoginUnavailable` when `m.cfg.ClaudeCredentials == ""`.
2. `name := "hexagon-claude-login-" + userID`, `m.docker.RemoveContainer(ctx, name, true)`.
3. `home := filepath.Join(m.cfg.ClaudeLoginDir, userID)`, `os.MkdirAll(home, 0o700)`,
   then `seedClaudeConfig(home)` — reused as is. Without `$HOME/.claude.json`
   the CLI opens its first-run onboarding, which is not the login the user
   pressed the button for.
4. `credentialsDir := filepath.Dir(m.cfg.ClaudeCredentials)`,
   `os.MkdirAll(credentialsDir, 0o700)` — Docker would otherwise create it
   itself, owned by root, on a machine where nobody has run `claude` yet.
5. `CreateContainer` and `StartContainer` with:

```go
dockerx.ContainerSpec{
	Name:  name,
	Image: imageRef,
	Cmd:   []string{"sleep", "infinity"},
	// No Anthropic variable of any kind. A container that already had one
	// would consider itself authenticated, and the login the user came here
	// for would be theatre performed on the credential they are replacing.
	Env:        []string{"HOME=" + dockerx.AgentHome, "TERM=xterm-256color"},
	WorkingDir: dockerx.AgentHome,
	User:       m.cfg.ContainerUser,
	// Not hexagon.managed: the reconciler matches managed containers against
	// session rows, and this one has none.
	Labels: map[string]string{dockerx.LabelRole: claudeLoginRole},
	Binds: []string{
		home + ":" + dockerx.AgentHome,
		// Read-write, and the host's own directory: this is the whole
		// mechanism. Claude Code writes .credentials.json here itself, into
		// the file every session already mounts.
		credentialsDir + ":" + dockerx.AgentHome + "/.claude",
	},
}
```

The nested bind is the shape `containerSpec` already uses — the session home,
with the credentials file mounted inside it.

## 6 — `internal/httpapi`

Routes, added to the `protected` map in `router.go`:

```go
"GET /api/claude":                  s.handleClaudeStatus,
"PUT /api/claude/credential":       s.handleSetClaudeCredential,
"DELETE /api/claude/credential":    s.handleForgetClaudeCredential,
"GET /api/claude/login/terminal":   s.handleClaudeLoginTerminal,
"DELETE /api/claude/login":         s.handleStopClaudeLogin,
```

`DockerfileEditor` in `router.go` gains the credential and the check:

```go
type DockerfileEditor interface {
	EditDockerfile(ctx context.Context, cred claudex.Credential, dockerfile, instruction string) (claudex.DockerfileEdit, error)
	Check(ctx context.Context, cred claudex.Credential) error
}
```

`handleEditDockerfile` resolves `s.auth.ClaudeCredential(r.Context(), s.user(r).ID)`
and passes it. Everything else about that handler is unchanged.

New file `internal/httpapi/handlers_claude.go`, with the wire types:

```go
// maxClaudeRequestBody bounds a credential: a kind and a token.
const maxClaudeRequestBody = 8 << 10

type claudeStatusResponse struct {
	// Credential is the credential this user pasted, without its secret.
	Credential *claudeCredentialResponse `json:"credential"`
	// File describes the credentials file the session mount points at.
	File claudeFileResponse `json:"file"`
	// Effective names what a new session will actually authenticate with:
	// "credential", "apiKey", "file" or "none". Inside the container an
	// environment variable wins over the mounted file, so a pasted credential
	// shadows a browser login, and the page has to be able to say so.
	Effective string `json:"effective"`
	// CanLogin is false when no credentials path is configured: there would be
	// nowhere for a browser login to write.
	CanLogin bool `json:"canLogin"`
	// CanVerify is false without a host claude binary, in which case a
	// credential is stored unchecked — the canAsk rule from the Images page.
	CanVerify bool `json:"canVerify"`
}

type claudeCredentialResponse struct {
	Kind      string    `json:"kind"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type claudeFileResponse struct {
	Path      string    `json:"path"`
	Present   bool      `json:"present"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

type setClaudeCredentialRequest struct {
	Kind   string `json:"kind"`
	Secret string `json:"secret"`
}
```

`handleClaudeStatus` builds it from `s.store.ClaudeCredential` (`ErrNotFound`
leaves `Credential` nil), an `os.Stat` of `s.cfg.ClaudeCredentials`, and
`s.editor != nil`. `Effective` follows the order in the analysis: the stored
credential, then `s.cfg.AnthropicAPIKey`, then the file, then `"none"`.

`handleSetClaudeCredential` follows `handleConnectAccount` step for step:
decode through `http.MaxBytesReader`; trim; a kind that is neither
`claudex.KindAPIKey` nor `claudex.KindOAuthToken` is a 400; an empty secret is a
400; when `s.editor != nil`, `Check` — and a failure is a 400 that carries the
CLI's own words, not ours, because what is wrong with a credential is something
only it knows; then `s.auth.SetClaudeCredential`; then the same body
`handleClaudeStatus` returns, so the page has one shape to render.

`handleForgetClaudeCredential` is `204`, with `store.ErrNotFound` as a 404.

`handleClaudeLoginTerminal` is `handleTerminal` with a different container.
Copy that handler and change four things:

- instead of a session, read `r.URL.Query().Get("image")`, load it with
  `s.store.ImageByID` scoped to `s.user(r).ID`, 404 if absent, and 409 unless
  its status is `store.ImageStatusReady` — a login needs an image with `claude`
  in it, and only a ready one has one;
- `session.ErrClaudeLoginUnavailable` from `StartClaudeLogin` is a 409;
- the same `Origin` rule, verbatim, including refusing a missing one: this
  endpoint hands out a shell;
- the exec:

```go
exec, err := s.docker.AttachExec(ctx, dockerx.ExecRequest{
	ContainerID: containerID,
	Cmd: []string{"tmux", "new-session", "-A", "-D", "-s", session.ClaudeLoginTmux,
		"-c", dockerx.AgentHome, session.ClaudeLoginCommand},
	Env:  []string{"TERM=xterm-256color"},
	Size: sizeFromQuery(r.URL.Query()),
})
```

then `s.pumpTerminal(ctx, conn, exec)` exactly as `handleTerminal` does. Do not
name a local variable `session` in this file: `handleTerminal` does, and here it
would shadow the package.

`handleStopClaudeLogin` calls `m.StopClaudeLogin` and answers `204`. It is what
the dialog calls when it closes; the socket closing does not remove anything,
because a page reload is a closed socket too.

`guardStateChanges` needs no exemption. The WebSocket is a `GET`, and the `PUT`
and `DELETE` come from `api.ts`, which sets the JSON content type on everything
that is not a `GET`.

## 7 — Wiring in `cmd/hexagon/main.go`

`session.Config` gains `ClaudeLoginDir: filepath.Join(cfg.DataDir, "claude-login")`.
The `claudex` runner is already assigned to `deps.Editor` and now satisfies the
wider interface; nothing else changes.

## 8 — Frontend

**`web/src/api.ts`** — the types mirror the Go structs field for field:

```ts
export type ClaudeCredentialKind = 'api_key' | 'oauth_token'
export type ClaudeSource = 'credential' | 'apiKey' | 'file' | 'none'

export interface ClaudeCredential { kind: ClaudeCredentialKind; updatedAt: string }
export interface ClaudeFile { path: string; present: boolean; updatedAt?: string }
export interface ClaudeStatus {
  credential: ClaudeCredential | null
  file: ClaudeFile
  effective: ClaudeSource
  canLogin: boolean
  canVerify: boolean
}
```

and a namespace beside `accounts`:

```ts
claude: {
  status: () => request<ClaudeStatus>('/claude'),
  setCredential: (kind: ClaudeCredentialKind, secret: string) =>
    request<ClaudeStatus>('/claude/credential', { method: 'PUT', body: JSON.stringify({ kind, secret }) }),
  forgetCredential: () => request<null>('/claude/credential', { method: 'DELETE' }),
  stopLogin: () => request<null>('/claude/login', { method: 'DELETE' }),
  // The terminal is a WebSocket, so it is a path for TerminalPane rather than
  // a fetch.
  loginTerminal: (imageId: string) => `/api/claude/login/terminal?image=${encodeURIComponent(imageId)}`,
},
```

**`web/src/components/TerminalPane.vue`** — the prop becomes
`defineProps<{ url: string }>()`, and `socketURL` appends the geometry to
whatever it was given:

```ts
function socketURL(): string {
  const scheme = window.location.protocol === 'https:' ? 'wss' : 'ws'
  const size = fit.value?.proposeDimensions()
  // The login terminal already carries a query, the session one does not.
  const sep = props.url.includes('?') ? '&' : '?'
  const query = size ? `${sep}cols=${size.cols}&rows=${size.rows}` : ''
  return `${scheme}://${window.location.host}${props.url}${query}`
}
```

`SessionView.vue` passes `:url="`/api/sessions/${session.id}/terminal`"`.

**`web/src/views/AccountsView.vue`** — a Claude section under the accounts list,
with its own `ref`s and its own `refresh`. What it shows:

- the effective source in one sentence, from `status.effective`: a stored
  credential, the server's configured key, the credentials file, or nothing at
  all;
- when a credential is stored, its kind and when — **never the secret** — with
  a Forget button;
- when `effective === 'credential'` and `file.present`, the shadowing sentence:
  the session will use the stored credential, because Claude Code prefers it to
  the file;
- a form with a kind selector (*API key* / *Long-lived token*) and a secret
  field, whose hint says where each comes from: the Console for a key,
  `claude setup-token` for a token. When `canVerify` is false, add that the
  server cannot check it before storing it;
- a **Log in** button when `canLogin`, opening the dialog, and a note that the
  browser login signs in the machine itself, so a session picks it up the next
  time it starts;
- when `canLogin` is false, the reason instead of the button: no credentials
  path is configured.

The dialog is a `<dialog>`-style overlay on this page, holding an image
`<select>` of ready images (`api.images.list()`, filtered on
`status === 'ready'`) and a `TerminalPane` pointed at
`api.claude.loginTerminal(imageId)`. Closing it calls `api.claude.stopLogin()`
and then `refresh()`, so the card shows the file that was just written.

Copy is sentence case and says what is happening, like `status.ts`.

## 9 — Documentation

- `README.md`: the `claude.credentials` row in the configuration table gains
  that it is also where the browser login writes, and the feature list gains a
  line about configuring the Claude login from the Accounts page.
- `AGENTS.md`, the HTTP API section: it says the only WebSockets are the
  terminal and the VS Code proxy. There is now a third, the Claude login
  terminal. Add it to that sentence.
- `plans/M1/README.md`: row 10 becomes a link to the analysis, state `done`.

## 10 — Tests, and running them

`make fmt`, `make vet`, `make test`, and `make build` — the last one is what
type checks the frontend (`vue-tsc -b`), and steps 6 and 8 change a prop.

Fakes to update before anything compiles: `fakeEditor` in
`internal/httpapi/images_test.go:443` gains the credential parameter and a
`Check`, and the fake credential source in `testEnv` gains `ClaudeCredential`.

Then the tests the analysis asks for:

- `internal/store/claude_credentials_test.go`: upsert, read back, replace
  (`created_at` survives, `updated_at` moves), delete, `ErrNotFound` on both
  reads of an absent row, and a row with a third kind refused by the `CHECK`.
- `internal/claudex`: extend the existing script-in-`t.TempDir()` tests so the
  script records its environment — an `api_key` credential arrives as
  `ANTHROPIC_API_KEY` and an `oauth_token` one as `CLAUDE_CODE_OAUTH_TOKEN`, a
  zero credential leaves the environment alone, and a server variable is
  *replaced* rather than duplicated. Plus `Check` on a script that exits
  non-zero producing an error that names it.
- `internal/httpapi/claude_test.go`: the status shape with and without a stored
  credential, and the assertion that no response body ever contains the secret;
  an empty secret and an unknown kind as 400s; a `Check` failure as a 400
  carrying the fake's message; `DELETE` twice, 204 then 404; a login terminal
  whose container binds the credentials directory at `/home/agent/.claude` and
  whose environment holds no Anthropic variable; a login against an image that
  is not ready as a 409; and 401 for every one of them without a cookie.
- `internal/httpapi/sessions_test.go`: a session created with a credential
  stored gets the variable, one created without falls back to the configured
  key, and **both keep the credentials mount** — the test that proves the new
  path did not disturb the old one.

By hand, last, because it is the only thing that proves the feature: with no
credentials file on the host, open the Accounts page, press **Log in**, complete
`/login` in the terminal, watch `~/.claude/.credentials.json` appear, and start
a session in which `claude` comes up already signed in.
