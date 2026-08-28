# M1.4 — tmux cheatsheet

## What was asked

> Bottone nella pagina della sessione che apre una dialog con le scorciatoie
> tmux piu' utili.

## Reading of the request

The terminal in a Hexagon session *is* tmux, and nothing in the page says so or
says how to drive it. This is a reference card, not a feature: no server change,
no state, nothing to persist. The only decisions worth making are which keys
earn a place and what the page says about the ones that behave differently here
than in a terminal on your own machine.

## Current behaviour

The session page header holds the status dot, Start/Stop, the Claude Code switch
from M1.3 and a link back to the list. The terminal below it attaches to the
tmux session `main`, created by the bootstrap. The base image installs tmux and
ships no `tmux.conf`, so **every default binding applies and the prefix is
`Ctrl-b`** — worth stating in the dialog, because it is the one thing that makes
all the others usable.

## Design

### A dialog, opened from the header

A `TmuxCheatsheet.vue` component, in the same backdrop-and-panel shape as
`NewSessionDialog`, opened by a `tmux keys` button next to the other header
controls. It is shown whatever the session's status is: reading up on the keys
before starting a session is not a mistake to prevent.

It closes on the backdrop, on the × and on Escape. Escape is not wired into the
existing dialog and is not being added there — a read-only reference is the case
where a reader reaches for it without thinking, and a half-filled form is not.

### Shared modal chrome

`.backdrop` and the panel chrome move to `web/src/style.css`, which already
exists for "base styles shared by every view", and both dialogs use them. Two
components with the same fifteen lines of positioning CSS is exactly the
duplication that drifts. Each dialog keeps its own width: a form and a reference
card do not want the same one.

### What goes on the card

Grouped as windows, panes, scrolling, and session; every row is
`prefix` + key, with the prefix stated once at the top. The selection is what a
Claude Code session actually needs — a second window for git, a split to watch a
build, and above all **a way to scroll**, which in a browser is not obvious:

- `prefix [` (and `prefix PageUp`) enter copy mode. This is the single most
  useful entry on the card. tmux repaints the screen it owns, so the browser's
  own scrollback holds nothing worth reading and the wheel does not do what a
  wheel usually does; copy mode is the only way back through the output.
- `prefix :` then `set -g mouse on` gets the wheel and clickable panes for the
  rest of the session's life. It is a command rather than a shortcut, but it is
  the fix for the complaint the terminal provokes, so it goes on the card.
- `prefix ?` lists every binding tmux has, which is the honest end of any
  cheatsheet.

Copy mode is described with Page Up/Page Down and the arrows, not with `/` for
search: mode keys default to emacs unless `$EDITOR` says otherwise, and `/` is a
vi binding. A card that lies about one key is worth less than one that leaves it
out.

### Two notes about this terminal specifically

The card ends with what is different here, because both surprise people:

- **`prefix d` does not get you out of anything.** Detaching ends the attach, the
  page notices the socket close and reconnects on its own, and tmux hands the
  same session straight back. The way to leave is to close the tab, which is
  also how you leave it running.
- **A second tab takes the terminal over from this one**, because the attach uses
  `-D`. That is deliberate and documented, but from the inside it looks like the
  terminal froze.

## Impact

| Area | Change |
|---|---|
| Schema, API, config | none |
| UI | new `TmuxCheatsheet.vue`; a button and its dialog state in `SessionView.vue`; `.backdrop`/`.dialog` chrome moved into `style.css` and dropped from `NewSessionDialog.vue` |

## Verification

There is no frontend test setup, and this point is not the reason to introduce
one. `npm run build` (`vue-tsc -b && vite build`) has to pass, and the checks
that matter are by hand: the button opens the dialog, the three ways of closing
it work, the create dialog still looks exactly as it did after its styles moved,
and every key on the card does in the session's tmux what the card says it does.
