<script setup lang="ts">
// The terminal in a session is tmux, and nothing else in the page says so. This
// is the reference card for driving it: the keys a Claude Code session actually
// needs, plus the two things that behave differently in a browser than in a
// terminal on your own machine.
import { onMounted, onUnmounted } from 'vue'

const emit = defineEmits<{ close: [] }>()

// The base image ships no tmux.conf, so every default binding applies and the
// prefix is the stock one.
const prefix = 'Ctrl-b'

interface Shortcut {
  keys: string
  what: string
}

const groups: { title: string; shortcuts: Shortcut[] }[] = [
  {
    title: 'Windows',
    shortcuts: [
      { keys: 'c', what: 'New window' },
      { keys: 'n / p', what: 'Next / previous window' },
      { keys: '0 … 9', what: 'Go to a window by number' },
      { keys: 'w', what: 'Pick a window from a list' },
      { keys: ',', what: 'Rename the window' },
      { keys: '&', what: 'Close the window (asks first)' },
    ],
  },
  {
    title: 'Panes',
    shortcuts: [
      { keys: '%', what: 'Split left and right' },
      { keys: '"', what: 'Split top and bottom' },
      { keys: '← ↑ ↓ →', what: 'Move to the pane in that direction' },
      { keys: 'z', what: 'Zoom the pane to the whole window, and back' },
      { keys: 'Space', what: 'Cycle through the layouts' },
      { keys: 'x', what: 'Close the pane (asks first)' },
    ],
  },
  {
    title: 'Scrolling back',
    shortcuts: [
      { keys: '[', what: 'Enter copy mode: Page Up, Page Down and the arrows scroll, q leaves' },
      { keys: 'Page Up', what: 'Enter copy mode already scrolling up' },
      { keys: ']', what: 'Paste what you copied' },
    ],
  },
  {
    title: 'The rest of tmux',
    shortcuts: [
      { keys: ':', what: 'Command prompt — try set -g mouse on' },
      { keys: '?', what: 'Every binding tmux has' },
    ],
  },
]

function onKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') emit('close')
}

onMounted(() => window.addEventListener('keydown', onKeydown))
onUnmounted(() => window.removeEventListener('keydown', onKeydown))
</script>

<template>
  <div class="backdrop" @click.self="emit('close')">
    <section class="dialog sheet">
      <header>
        <h2>tmux keys</h2>
        <button type="button" class="icon" @click="emit('close')" aria-label="Close">×</button>
      </header>

      <p class="lead">
        Every shortcut starts with the prefix: press <kbd>{{ prefix }}</kbd>, let go, then the
        key.
      </p>

      <div class="card">
        <section v-for="group in groups" :key="group.title" class="group">
          <h3>{{ group.title }}</h3>
          <dl>
            <template v-for="shortcut in group.shortcuts" :key="shortcut.keys">
              <dt><kbd>{{ prefix }}</kbd> <kbd>{{ shortcut.keys }}</kbd></dt>
              <dd>{{ shortcut.what }}</dd>
            </template>
          </dl>
        </section>
      </div>

      <section class="hexagon">
        <h3>In Hexagon</h3>
        <p class="note">
          <kbd>{{ prefix }}</kbd> <kbd>d</kbd> detaches, but the page reconnects by itself and
          tmux hands the same session straight back. To leave, close the tab: the session keeps
          running without you.
        </p>
        <p class="note">
          Opening this session in another tab takes the terminal over from this one, which from
          here looks like the terminal froze.
        </p>
      </section>
    </section>
  </div>
</template>

<style scoped>
.sheet {
  width: min(42rem, 100%);
}

.lead {
  margin: 0;
  color: var(--text-muted);
}

/* One grid for the whole card, with the groups dissolved into it: the key
   column is then as wide as the widest shortcut anywhere, so the descriptions
   line up across groups instead of stepping in and out. */
.card {
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: 0.35rem 1rem;
  align-items: baseline;
}

.group,
.card dl {
  display: contents;
}

.hexagon {
  display: grid;
  gap: 0.5rem;
}

h3 {
  grid-column: 1 / -1;
  margin: 0;
  font-size: 0.8rem;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--text-muted);
}

dt {
  white-space: nowrap;
}

/* The first heading already sits under the lead paragraph. */
.card > .group:not(:first-child) h3 {
  margin-top: 0.6rem;
}

dd {
  margin: 0;
  min-width: 0;
}

kbd {
  padding: 0.05rem 0.35rem;
  border: 1px solid var(--border);
  border-bottom-width: 2px;
  border-radius: 4px;
  background: var(--surface);
  font-family: var(--mono);
  font-size: 0.85em;
  white-space: nowrap;
}

.note {
  margin: 0;
  color: var(--text-muted);
  font-size: 0.9rem;
}
</style>
