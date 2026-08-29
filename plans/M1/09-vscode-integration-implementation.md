# M1.9 — implementation plan

The reasoning is in [09-vscode-integration.md](09-vscode-integration.md), and it
is settled: read it first, then follow the steps here in order. Where this file
gives an exact name, signature or snippet, use it as written — the shapes were
checked against the code and against code-server's own behaviour, and the
comments explain constraints that are not visible from the call site.

Nothing here is a suggestion to redesign. If a step turns out to be wrong, say
so and stop rather than substituting a different design; the analysis records
what was rejected and why.

## 1 — Schema and store

`internal/store/migrations/005_session_vscode.sql`, in the shape of `004`:

```sql
-- Whether this session was created with the VS Code integration: its container
-- publishes code-server and has the release bind mounted. Existing rows are 0
-- because their containers have neither, and a container keeps the mounts and
-- the port bindings it was created with.
ALTER TABLE sessions ADD COLUMN vscode INTEGER NOT NULL DEFAULT 0 CHECK (vscode IN (0, 1));
```

In `internal/store/sessions.go`, four places that must stay in lockstep:
`Session.VSCode bool`; `vscode` added to `sessionColumns` after
`propagate_token` and before `created_at`; `&s.VSCode` in `scanSession` in that
same position; one more `?` and `session.VSCode` in `CreateSession`. No setter —
the flag is not editable after creation.

## 2 — Ports in `internal/dockerx`

Constants next to `WorkspaceMount` and `AgentHome`:

```go
// VSCodeMount is where the host's code-server release appears inside a session
// container, and VSCodePort the port it listens on there. 8443 rather than
// code-server's own 8080, which is the port a project under /workspace is
// likeliest to want for itself.
const VSCodeMount = "/opt/code-server"
const VSCodePort = 8443
```

`ContainerSpec` gains `Ports []int`, and `CreateContainer` turns it into a
binding. This needs `github.com/docker/go-connections/nat`, which is already in
the module graph as an indirect dependency of the Docker client, so `go mod
tidy` only promotes it to a direct requirement:

```go
if len(spec.Ports) > 0 {
	config.ExposedPorts = nat.PortSet{}
	hostConfig.PortBindings = nat.PortMap{}
	for _, p := range spec.Ports {
		port := nat.Port(strconv.Itoa(p) + "/tcp")
		config.ExposedPorts[port] = struct{}{}
		// Loopback, and a port Docker picks: whoever reaches it gets an editor
		// inside the session without being asked for anything.
		hostConfig.PortBindings[port] = []nat.PortBinding{{HostIP: "127.0.0.1"}}
	}
}
```

`ContainerState` gains `Ports map[int]int`, container port to host port, filled
by `InspectContainer` from `inspected.NetworkSettings.Ports`: for every entry
that has bindings, the key is `nat.Port.Int()` and the value is the first
binding's `HostPort` parsed as an integer. Leave the map nil when the container
publishes nothing.

Then update the fakes, or nothing compiles: `fakeContainer` in
`internal/httpapi/fakedocker_test.go` must keep `spec.Ports`, and a test must be
able to say which ports `InspectContainer` reports.

## 3 — `internal/codeserver`

A new package with one job. Its doc comment says what it owns:

```go
// Package codeserver keeps one code-server release on the host, so a session
// can run VS Code in the browser without every image carrying a copy of it.
package codeserver
```

```go
// Config is where the release lives and which one to fetch if it is not there.
type Config struct {
	Dir     string // install root: <Dir>/bin/code-server is what runs
	Version string // release to download, without the leading v
	BaseURL string // release download root; empty means the project's own
}

// Source hands out the directory holding the release.
type Source struct { ... }

func New(cfg Config) *Source

// Ensure returns the directory holding a usable code-server, downloading the
// configured release the first time. It is safe to call concurrently.
func (s *Source) Ensure(ctx context.Context) (string, error)
```

`Ensure`, in order:

1. If `<Dir>/bin/code-server` exists, return `Dir`. **Never** delete, overwrite
   or version-check a directory that already holds an install: that rule is what
   makes a hand-placed release work on a machine with no route to GitHub, and
   upgrading is the user deleting the directory. The README says so.
2. A `sync.Mutex` on the `Source`, so two provisionings do not download at once.
3. The architecture comes from `runtime.GOARCH`: `amd64` and `arm64` only,
   anything else is an error naming the architecture. The asset is
   `<BaseURL>/v<version>/code-server-<version>-linux-<arch>.tar.gz`, with
   `BaseURL` defaulting to
   `https://github.com/coder/code-server/releases/download`. The default version
   is a const in this package: `4.135.0`, verified to exist and to ship
   `code-server-4.135.0-linux-amd64/bin/code-server` and a bundled `lib/node`,
   so the container needs no Node of its own.
4. Download and extract in one pass into `<Dir>.partial`, then `os.Rename` it
   onto `<Dir>`, so a download that is killed never leaves a half-extracted tree
   that step 1 would accept:
   `exec.CommandContext(ctx, "tar", "-xz", "--strip-components=1", "-C", partial)`
   with the HTTP response body as its `Stdin`. Check `exec.LookPath("tar")`
   first and say plainly that it is missing; a non-200 response is an error
   quoting the status.
5. Before the rename, verify `<partial>/bin/code-server` exists.

`BaseURL` is a field so the test can point it at an `httptest` server.

## 4 — Configuration

Per AGENTS.md, each setting is four edits: a field on `config.Config` with the
names in a trailing comment, a field on `config.file`, a `pick(...)` line in
`Load`, a row in the README table, and a key in `config.example.json`.

| Field | Env, file key | Default |
|---|---|---|
| `VSCodeDir` | `HEXAGON_VSCODE_DIR`, `vscode.dir` | `<DataDir>/code-server` |
| `VSCodeVersion` | `HEXAGON_VSCODE_VERSION`, `vscode.version` | `codeserver.DefaultVersion` |

`VSCodeDir` is resolved through `expandHome`, like `WorkspaceRoot`, and its
default is built from `dataDir` the way `DatabasePath` is.

## 5 — `internal/session`

- `CreateRequest.VSCode bool`.
- The collaborator, defined here because this is the consumer:

```go
// VSCodeSource provides the code-server release to bind mount into a session
// created with the integration. It is nil when the server has no way to get
// one, which a session is told about rather than being failed silently.
type VSCodeSource interface {
	Ensure(ctx context.Context) (string, error)
}
```

  It becomes a new argument to `NewManager` and a field on `Manager`. Nil is a
  legal value, as `httpapi.Deps.Editor` is: `Create` then rejects a request with
  `VSCode: true` with a new sentinel `ErrVSCodeUnavailable`.
- `provisionSteps`: before `CreateContainer`, when `session.VSCode`, call
  `Ensure` and pass the directory on. `containerSpec` becomes
  `containerSpec(session *store.Session, homeDir, vscodeDir string, credentials provider.GitAuth)`.
  The download happens with the session in `creating`; a failure fails
  provisioning with the error text, which the session page already shows.
- `containerSpec`, when `vscodeDir != ""`: append
  `vscodeDir + ":" + dockerx.VSCodeMount + ":ro"` to `binds`, and set
  `Ports: []int{dockerx.VSCodePort}` on the spec. Read-only on purpose — the
  container gets to run the editor, not to modify it. Everything code-server
  writes goes under `$HOME`, which is already a bind mount of the session's own
  directory, so its settings and extensions survive a restart.
- `bootstrapScript(autoClaude, credentials, vscode bool)` takes a third
  parameter and, when it is set, appends the launch. **The three comments below
  are the three ways this goes wrong; keep them in the code.**

```go
// code-server is started detached with its output redirected to a file. The
// bootstrap exec only returns when nothing holds its stdout open, so a
// background process that inherited the pipe would hold provisioning open for
// as long as the editor ran.
//
// It binds 0.0.0.0 and not loopback: a published port is forwarded to the
// container's own interface, and a server on the container's loopback would
// never see a packet — a failure that looks exactly like a server that did not
// start.
//
// The guard is a pid file rather than pgrep because the bootstrap runs as
// `sh -c "<script>"`, so its own command line contains the code-server path and
// `pgrep -f` would match it every time and never start anything.
const vscodeCommand = `if ! kill -0 "$(cat /tmp/hexagon-code-server.pid 2>/dev/null)" 2>/dev/null; then
  nohup ` + dockerx.VSCodeMount + `/bin/code-server --bind-addr 0.0.0.0:8443 --auth none \
    --disable-telemetry --disable-update-check --disable-workspace-trust \
    ` + dockerx.WorkspaceMount + ` >/tmp/hexagon-code-server.log 2>&1 &
  echo $! >/tmp/hexagon-code-server.pid
fi`
```

  All four flags exist in code-server's CLI; they were checked. `--auth none` is
  deliberate: Hexagon's session cookie is the authentication, and a second
  password would be one code-server generates and nobody knows. Build the port
  into the string from `dockerx.VSCodePort` rather than repeating `8443`.
  `bootstrap` passes `session.VSCode`.
- The lookup, with sentinels `ErrVSCodeUnavailable`, `ErrVSCodeDisabled` and
  `ErrVSCodeNotReady`:

```go
// VSCodeEndpoint returns the base URL of the code-server this session's
// container publishes. It is looked up rather than stored: Docker picks a new
// host port every time the container starts.
func (m *Manager) VSCodeEndpoint(ctx context.Context, s *store.Session) (string, error)
```

  `ErrVSCodeDisabled` when the session was not created with the integration,
  `ErrNoContainer` when it has no container, `ErrVSCodeNotReady` when the
  container is not running or reports no binding for `dockerx.VSCodePort`.
  Otherwise `"http://127.0.0.1:" + strconv.Itoa(port)`.
- `cmd/hexagon/main.go`: build a `codeserver.Source` from the configuration and
  pass it to `NewManager`.

## 6 — `internal/httpapi`

- `createSessionRequest.VSCode *bool`, read as `req.VSCode != nil && *req.VSCode`
  — **off by default**, unlike the two flags before it, with a comment saying
  why: it costs a mount and a published port, and a session that never opens the
  editor should carry neither. `sessionResponse.VSCode bool`, built in
  `newSessionResponse`. `handleCreateSession` maps `session.ErrVSCodeUnavailable`
  to 503.
- Two entries in the `protected` map, both without a method so every verb
  matches:

```go
"/api/sessions/{id}/vscode":           s.handleVSCode,
"/api/sessions/{id}/vscode/{path...}": s.handleVSCode,
```

- A new `handlers_vscode.go`. `handleVSCode` uses the existing `s.sessionOr404`,
  redirects to the trailing slash when the path has none — code-server emits
  relative URLs and needs a directory as its base — resolves the target through
  `s.sessions.VSCodeEndpoint` (`ErrVSCodeDisabled` and `ErrVSCodeNotReady` both
  409), and proxies:

```go
proxy := &httputil.ReverseProxy{
	Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		// code-server is served at the root of its own port; the prefix that
		// names the session is Hexagon's, and it strips it the way the reverse
		// proxy recipe in code-server's documentation does.
		pr.Out.URL.Path = "/" + pr.In.PathValue("path")
		pr.Out.URL.RawPath = ""
		pr.SetXForwarded()
	},
	ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		s.log.Warn("vscode proxy", "session", id, "err", err)
		http.Error(w, "VS Code is not answering in this session yet. Reload in a moment.",
			http.StatusBadGateway)
	},
}
proxy.ServeHTTP(w, r)
```

  Leave `Transport` nil so the pooled `http.DefaultTransport` is used even
  though the proxy value is built per request. WebSocket upgrades survive
  because `statusRecorder` already implements `Hijack` and because `main.go`
  sets no `WriteTimeout` on the server.
- `guardStateChanges` in `middleware.go` lifts the JSON content-type rule for
  this path, and only this path, after the same-origin check has passed:

```go
// The VS Code proxy carries a whole other application's traffic, which is not
// JSON and cannot be made to be. It stays behind requireAuth and the
// same-origin check like every other endpoint; only the content type rule is
// lifted.
func isVSCodeProxyPath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/api/sessions/")
	if !ok {
		return false
	}
	_, after, ok := strings.Cut(rest, "/")
	return ok && (after == "vscode" || strings.HasPrefix(after, "vscode/"))
}
```

## 7 — Frontend

- `web/src/api.ts`: `vscode: boolean` on `Session` and `vscode?: boolean` on
  `NewSession`, each with the one-line comment its neighbours have. No new call:
  the URL is a path the view builds.
- `web/src/components/NewSessionDialog.vue`: `const vscode = ref(false)`, with a
  comment saying why it is off by default, a third toggle after the existing
  two, and the field added to **both** request bodies in `submit()` — the
  repository branch and the no-repository branch.

```html
<label class="toggle">
  <input type="checkbox" v-model="vscode" />
  <span>
    VS Code in the browser
    <em>
      Adds a button on the session page that opens VS Code on the workspace. It
      has to be chosen now: the container is built for it.
    </em>
  </span>
</label>
```

- `web/src/views/SessionView.vue`: a link in `.right`, before the `tmux keys`
  button, shown only for a session that has the integration and is running — the
  same rule the Dockerfile editor's `canAsk` follows, leave the control out
  rather than offer one that fails.

```html
<a v-if="session.vscode && session.status === 'running'" class="button"
   :href="`/api/sessions/${session.id}/vscode/`" target="_blank" rel="noopener">VS Code</a>
```

  Add an `a.button` rule to the scoped CSS beside the existing `button` ones:
  same padding, border, radius and hover, plus `text-decoration: none`.
- No change to `web/vite.config.ts`: the path is under `/api`, which the dev
  server already proxies with `ws: true`.

## 8 — Documentation

- README: the two configuration rows, and a short paragraph on the integration —
  what it does, that the release is downloaded once into the data directory and
  that deleting that directory is how it is upgraded, and the honest note that
  code-server answers on the host loopback without a password of its own, so any
  process on the machine can open a running session's editor.
- `AGENTS.md`: the HTTP API section says there is no WebSocket other than the
  terminal. Amend that sentence to name the VS Code proxy, so the conventions
  keep describing the code.
- `deploy/images/base/Dockerfile`: one sentence in the header saying code-server
  arrives as a bind mount, so nobody adds it to the image later. The image needs
  nothing else, and `internal/claudex`'s prompt stays correct as it is.

## 9 — Tests, and running them

Run everything through the Makefile: `make test`, never a bare `go test` — the
server embeds `web/dist` and the Makefile creates the placeholder first. Then
`make fmt`, `make vet`, and `make build`, which runs `vue-tsc -b` over the
frontend changes.

- `internal/store`: the migration test gains a row proving an existing session
  comes out with `vscode` off. The precedent is in
  `internal/store/provider_accounts_test.go`.
- `internal/httpapi/sessions_test.go`, which already reads back the spec the
  fake Docker was asked for: `vscode: true` produces a spec with
  `Ports: []int{8443}`, a bind ending in `/opt/code-server:ro`, and a bootstrap
  containing `code-server`; the default produces a spec with none of the three;
  and the flag survives `GET /api/sessions/{id}`.
- A new `internal/httpapi/vscode_test.go`, with the fake reporting the port of
  an `httptest` backend: `/api/sessions/{id}/vscode/foo?x=1` reaches the backend
  as `/foo?x=1` and its body comes back; without a cookie it is 401; another
  user's session is 404; a session created without the integration is 409. And,
  asserted together because they are the two halves of one decision, a `POST` to
  the proxy path with a non-JSON content type is forwarded while the same
  content type on `POST /api/sessions` is still refused.
- `internal/codeserver`: `Ensure` against an `httptest` server serving a small
  tarball extracts it, returns the directory, and makes no second request when
  called again. Skip the test when `tar` is not on `PATH`.

By hand, which is the only thing that proves the proxy really carries VS Code:
create a session with the box ticked and watch the first one download the
release into `~/.local/share/hexagon/code-server`; the button opens an editor
showing the clone; a terminal inside that editor works, which is the WebSocket;
the source control panel commits and pushes. Then stop and start the session and
open it again — the host port will have changed, which is exactly why nothing
stores it.
