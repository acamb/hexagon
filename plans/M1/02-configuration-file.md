# M1.2 — Configuration file

## What was asked

> Hexagon puo' partire con un file di configurazione che contiene i valori
> necessari invece di leggerli da env.

## Reading of the request

"Instead of" is about the *user's* interface, not an amputation: today the only
way to configure Hexagon is to export a dozen `HEXAGON_*` variables before every
run, which is fine for `make dev` and poor for anything else — secrets end up in
shell history, and there is no single artefact to back up or copy to another
machine. The file must therefore be able to carry **every** setting, so a user
can start the server with no environment at all.

Environment variables stay, for three reasons: `make dev` sets two of them,
`DOCKER_HOST` and `ANTHROPIC_API_KEY` are conventions owned by other tools, and a
one-off override without editing a file is worth keeping. So this point adds a
layer, it does not replace one.

## Current behaviour

`config.Load()` reads the environment directly into `config.Config`, applying a
default per field through the `env(key, def)` helper, then creates the data
directories and resolves the secret key. `cmd/hexagon` additionally reads
`HEXAGON_DEBUG` on its own, inside `newLogger`.

## Design

### Format: JSON

JSON is in the standard library, and the repository's rule is that a dependency
is a decision to justify. It also has the property this configuration will need
before M1 is over: **structure**. Point 6 adds a second repository provider and
point 10 several Claude accounts; a list of objects is natural in JSON and
miserable in environment variables. Making the file the place where structured
configuration lives is what makes those points cheap later.

Rejected:

- **TOML/YAML** — better to write by hand, but each is a dependency, and the
  comments they would buy are worth less than the dependency costs here.
- **A dotenv-style `KEY=value` file** — no dependency and comments allowed, but
  it is the environment again, with the same flat string-only shape. It would
  have to be replaced to express the providers of point 6.

### Layering

`defaults < config file < environment`.

The environment wins so that `make dev` keeps working over any file a developer
has, and so that a one-off override stays a one-off override. It is the ordering
every tool with both mechanisms uses, and the surprising alternative — a file
that silently loses to a variable *nobody set* — cannot happen, because an unset
variable is not a value.

### Where the file comes from

1. `-config <path>` on the command line;
2. `HEXAGON_CONFIG`;
3. `<user config dir>/hexagon/config.json`, i.e. `~/.config/hexagon/config.json`.

A file named by (1) or (2) that does not exist is a startup error: the user
asked for it, and silently ignoring it would start a server configured by
accident. The default path is optional, so a tree with no file behaves exactly as
it does today.

The default lookup deliberately does not live under `HEXAGON_DATA_DIR`: the data
directory is itself one of the values the file sets, and looking for the file
inside a directory the file chooses is circular. Config in the XDG config dir,
data in the XDG data dir.

### Flag parsing

The project has no flags today. `-config` is added with the standard `flag`
package in `cmd/hexagon`, which parses it and passes the path to
`config.Load(path)`. The `config` package keeps knowing nothing about the command
line — it takes a path and layers file over environment.

### Unknown fields are an error

Decoding uses `DisallowUnknownFields`. A typo in a key would otherwise be
silently ignored, and a silently ignored `allowedUsers` is an authentication
bypass. This is the same fail-closed rule the server already applies by refusing
to start without an allowlist.

### The file is a secret

It can hold the OAuth client secret, the server secret key, an Anthropic API key
and a GitHub PAT — the same class of material as `secret.key`, which the process
writes `0600`. So a config file readable by group or others is a startup error
with a message naming the fix. Warning instead was rejected: a warning at startup
is read once and never again, and the whole point of the file is that secrets
stop living in the shell.

### Paths starting with `~`

Expanded, for both layers. A hand-written config file is exactly where `~` gets
typed, and the current failure mode is silent and ugly: `os.MkdirAll` happily
creates a directory literally named `~`. Applies to the path-valued settings —
data directory, workspace root, Claude credentials.

### Two incidental fixes

- **`HEXAGON_DEBUG` moves into `config.Config`** as `Debug`, so the file can
  carry it like everything else. `run()` loads the configuration before building
  the logger; `Load` errors are reported on stderr and never needed it.
- **An explicitly empty Claude credentials path now disables the mount**, as the
  README has always claimed. It does not today: `env()` treats an empty variable
  as unset, so the default comes back and the mount happens anyway. The
  environment layer switches to `os.LookupEnv` for that field and the file models
  it as a pointer, so "set to empty" is distinguishable from "not set".

## Shape of the file

```json
{
  "addr": "127.0.0.1:8080",
  "publicUrl": "http://localhost:5173",
  "dataDir": "~/.local/share/hexagon",
  "workspaceRoot": "",
  "secretKey": "",
  "debug": false,
  "github": {
    "clientId": "Iv23li...",
    "clientSecret": "...",
    "allowedUsers": ["andrea"],
    "apiUrl": ""
  },
  "claude": {
    "credentials": "~/.claude/.credentials.json",
    "anthropicApiKey": ""
  },
  "git": { "userName": "", "userEmail": "" },
  "docker": { "host": "" },
  "dev": { "user": "", "githubToken": "" }
}
```

Keys are camelCase, matching the JSON convention the API already uses. The
grouping mirrors the groups already commented in `config.Config`; the flat
top-level fields are the ones whose environment variables have no common prefix
beyond `HEXAGON_`.

## Impact

| Area | Change |
|---|---|
| Schema | none |
| API | none |
| Configuration | `config.Load(path)` takes the config path; `Config` gains `Debug`; new `HEXAGON_CONFIG`; `-config` flag |
| UI | none |
| Docs | README gains a "Configuration file" section and rows for `HEXAGON_CONFIG`/`-config`; `config.example.json` at the root, with `/config.json` ignored by git |

`internal/config` is the only package that changes, plus the twelve lines in
`cmd/hexagon` that parse the flag and build the logger from `cfg.Debug`.

## Verification

`internal/config/config_test.go` gains cases for: a file supplying values with an
empty environment; the environment overriding the file; `allowedUsers` as a JSON
array; an unknown key rejected; a named file that does not exist rejected; a
missing default file ignored; a group-readable file rejected; `~` expanded; and
an explicitly empty Claude credentials path surviving as empty.

By hand: `./bin/hexagon -config config.json` with no `HEXAGON_*` variable set at
all must sign in and start a session exactly as the documented environment setup
does.
