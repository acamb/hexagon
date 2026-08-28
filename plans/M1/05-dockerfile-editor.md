# M1.5 — Asking Claude Code to edit the Dockerfile

## What was asked

> Editor docker file in creazione immagine. Deve essere possibile chiedere a
> claude di modificarlo (tramite `claude -p`) prima di creare l'immagine (oltre a
> poterlo modificare a mano).

## Reading of the request

Editing by hand already exists: the Images page has a textarea, seeded with the
reference Dockerfile from `deploy/images/base/Dockerfile`. What is missing is the
other half — saying "add the Rust toolchain" and getting the edited Dockerfile
back, before pressing Build.

So this point is one operation: **text in, text out**. Given a Dockerfile and an
instruction in English, return the modified Dockerfile. Nothing is built, nothing
is stored, and nothing about the image lifecycle changes.

## Design

### `claude -p` runs on the host, with no tools at all

The invocation is:

```
claude -p --safe-mode --strict-mcp-config --tools "" --output-format json --json-schema <schema>
```

with the prompt on stdin, and it takes about four seconds. Each flag is load
bearing:

- **`--tools ""`** removes every built-in tool. This is what makes running the
  agent on the host uninteresting to attack: there is no Bash, no file access, no
  web fetch. The Dockerfile goes in through the prompt and comes back in the
  answer; the model has nothing else to reach for.
- **`--safe-mode`** drops CLAUDE.md, skills, plugins, hooks, custom agents and MCP
  servers, and `--strict-mcp-config` makes sure of the last one. The developer's
  own Claude Code configuration must not change what a Hexagon request does.
- **`--output-format json` with `--json-schema`** returns
  `{"dockerfile": …, "summary": …}`. Without a schema the answer comes back in a
  ```dockerfile fence however firmly the prompt asks otherwise — verified, it
  does — and stripping fences with a regexp is guesswork about the shape of a
  reply. The schema also buys the one-line summary of what changed, which is what
  the UI shows next to the result.

Authentication is the host's own: the process runs as the server user, so it uses
the same `~/.claude/.credentials.json` Hexagon already mounts into sessions.

**Running it in a container was rejected**, even though it is the reflex for
"execute an agent somewhere safe". At image-creation time there may be no image
to run it in: the first image a user builds is the one that installs `claude`, so
the feature would depend on the thing it exists to help create. Bootstrapping a
separate helper image to edit a Dockerfile is a circular dependency around a call
that, with no tools, has nothing to escape from.

The other side of the trade is honest: any signed-in user can now spend the
host's Claude Code quota. That sits inside the trust boundary the allowlist
already draws — the same user can start a container with those credentials
mounted and drive Claude Code by hand.

### A new package, `internal/claudex`

Named and shaped like `dockerx` and `gitops`: it wraps one external tool and
holds no product logic. It knows how to build the argv, feed the prompt, parse
the JSON, and turn the three failure modes — binary missing, non-zero exit,
`is_error` in the payload — into one error the handler can show.

The binary is resolved once at startup: `claude.binary` from the configuration
file, else `claude` on `PATH`, else `~/.local/bin/claude`, which is where the
official installer puts it and which a server started from a desktop session may
well not have on its `PATH`.

### `POST /api/images/dockerfile`

`{"dockerfile": …, "instruction": …}` → `{"dockerfile": …, "summary": …}`.

Not under `/api/images/{id}`: the image does not exist yet, and may never. It is
a POST because it is neither safe nor idempotent — it spends money and returns a
different answer each time — which also puts it behind the origin and content
type checks every mutating call goes through.

`GET /api/images/template` gains `"canAsk"`, so the page can leave the control
out entirely when the server has no `claude` binary, rather than offering a
button that always fails.

### The interaction

An instruction field under the Dockerfile, with an **Ask Claude** button. The
answer replaces the textarea, the summary appears next to it, and **Undo** puts
back exactly what was there before. Replacing rather than showing a diff to
accept: the textarea *is* the working copy, the user is going to read the result
before pressing Build anyway, and one undo covers the case where the answer is
worse than what it replaced.

The request blocks until it is done, with the button showing progress — no job
row, no polling. It takes seconds, unlike the builds this page already polls for,
and a job that leaves no trace is not worth a table.

## Impact

| Area | Change |
|---|---|
| Schema | none |
| Config | `claude.binary` / `HEXAGON_CLAUDE_BINARY`, `claude.model` / `HEXAGON_CLAUDE_MODEL` |
| Go | new `internal/claudex`; `httpapi.DockerfileEditor` in `Deps`; one handler and one route; `canAsk` on the template response |
| UI | instruction field, Ask Claude, summary and Undo in the Images form |

## Verification

- `internal/claudex`: the binary is a path, so the tests point it at a shell
  script in `t.TempDir()` that records its argv and prints a canned payload. That
  covers the flags actually being passed (`--tools ""` above all), the prompt
  reaching stdin, the good answer being parsed, and each of the three failure
  modes producing an error that names what went wrong. No API call, no network.
- `internal/httpapi`: a fake editor in `testEnv` — the edited Dockerfile comes
  back, an empty instruction is a 400, an editor failure is a 502 rather than a
  500 (the failure is in something the server called, not in the server), and the
  endpoint is behind authentication like everything else.
- By hand: on the Images page, "add ripgrep and the go toolchain", read the
  result, press Build.
