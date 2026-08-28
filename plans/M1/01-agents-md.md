# M1.1 — AGENTS.md and the milestone analysis convention

## What was asked

> Generare un agents.md con le convenzioni del progetto. Inoltre le milestones
> in plans/milestones devono generare delle analisi nelle loro relative
> sottocartelle da creare (esempio: M1 -> plans/M1/xxx.md).

Two deliverables: a conventions document for whoever — human or agent — works on
the repository, and a working convention that turns each milestone point into an
analysis document under its own folder.

## Reading of the request

The conventions were not written down anywhere, but they exist: M0 to M6 were
implemented consistently enough that they can be read off the code rather than
invented. So `AGENTS.md` **describes what the repository already does**, and only
states as a rule what the existing code already follows. Nothing in it asks for a
change to the current tree.

Where the code was silent the document stays silent too: there is no frontend
test setup, no linter beyond `go vet` and `vue-tsc`, no branching or release
policy, so `AGENTS.md` says so instead of inventing one.

## What was produced

### `AGENTS.md` at the repository root

Sections, and where each one comes from:

| Section | Source |
|---|---|
| English-only rule | `plans/milestones/start-plan.md`, stated there in bold |
| Commands | `Makefile`, in particular why `make test` and not `go test ./...` |
| Layout and dependency policy | `README.md`, `go.mod`, the plan's dependency section |
| Comments | the existing comments, which are uniformly about *why* |
| Go | error wrapping, sentinels, consumer-side interfaces, `slog`, fail-closed defaults, all read off `internal/*` |
| HTTP API | `router.go`, `json.go`, `middleware.go`, `handlers_sessions.go` |
| Store | `store.go`, `models.go`, `001_init.sql` |
| Configuration | `config.go` plus the README table it has to stay in sync with |
| Frontend | `api.ts`, `router.ts`, `session.ts`, `status.ts`, `style.css`, `SessionsView.vue` |
| Tests | `store_test.go`, `sessions_test.go`, `fakedocker_test.go` |
| Security invariants | the plan's authentication section and the README's security notes |
| Plans and milestones | this point |
| Commits | `git log` |

The comment section is given the most weight on purpose. It is the convention
most likely to erode, because a plausible-looking comment that restates the code
passes review, and the codebase's habit of recording the rejected alternative is
what makes it readable a milestone later.

`CLAUDE.md` is a symlink to `AGENTS.md`, so Claude Code picks the file up
automatically without a second copy to keep in sync.

### `plans/M1/`

- `README.md` — indexes the ten points of M1, links each to its analysis, and
  tracks state.
- `01-agents-md.md` — this document.

## The convention

- `plans/milestones/<milestone>.md` stays the user's statement of intent and is
  not rewritten.
- `plans/<milestone>/` holds the analyses, one file per point, named
  `NN-slug.md` from the point's number and an English slug of its subject.
- `plans/<milestone>/README.md` is the index and the state of the milestone.
- The analysis is written **before** the point is implemented, and covers: what
  is asked and how it is read, the current behaviour it changes, the design with
  the alternatives rejected, the impact on schema, API, configuration and UI, and
  how it will be verified.

### Alternatives rejected

**Analyses inside the milestone file.** `M1.md` would become a document with two
authors and two purposes; the statement of intent stops being stable, and a
diff on it no longer means the requirements changed.

**One analysis per milestone rather than per point.** The ten points of M1 are
independent — a tmux cheatsheet dialog and a second repository provider share
nothing. One file per point keeps each analysis short enough to be written
before its implementation instead of after.

**Numbering the files freely.** Tying `NN` to the number in the milestone
statement means the mapping never has to be looked up, and a missing analysis is
visible as a gap in the sequence.

## Impact

Documentation only: no schema, API, configuration or UI change. The one addition
to the tree beyond the documents is the `CLAUDE.md` symlink.

## Verification

- `make test` and `make vet` still pass — nothing in the build was touched.
- Every claim in `AGENTS.md` was taken from a file in the tree, not from habit;
  the table above is the audit trail.
- The convention is verified by being used: this document is its first instance.
