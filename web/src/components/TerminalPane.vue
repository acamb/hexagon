<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, shallowRef } from 'vue'
import { FitAddon } from '@xterm/addon-fit'
import { Terminal } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'

const props = defineProps<{ url: string }>()

type Connection = 'connecting' | 'open' | 'closed'

const host = ref<HTMLElement | null>(null)
const state = ref<Connection>('connecting')
const term = shallowRef<Terminal | null>(null)
const fit = shallowRef<FitAddon | null>(null)
const socket = shallowRef<WebSocket | null>(null)

// Sticky, not held: a touchscreen has no keydown/keyup pair to hold a
// modifier through, so a tap arms it for exactly the next byte through
// press() instead — whether that byte comes from the OS keyboard or from one
// of the Tab/Esc/arrow buttons below.
const ctrlArmed = ref(false)
const altArmed = ref(false)

const encoder = new TextEncoder()
let observer: ResizeObserver | null = null
let resizeTimer: number | undefined
let reconnectTimer: number | undefined
let attempt = 0
let disposed = false

// Terminal colours come from the page, so the pane matches whichever theme the
// browser is in.
function theme() {
  const styles = getComputedStyle(document.documentElement)
  const read = (name: string, fallback: string) => styles.getPropertyValue(name).trim() || fallback
  return {
    background: read('--bg', '#101519'),
    foreground: read('--text', '#e6edf3'),
    cursor: read('--accent', '#79bae9'),
  }
}

function socketURL(): string {
  const scheme = window.location.protocol === 'https:' ? 'wss' : 'ws'
  const size = fit.value?.proposeDimensions()
  // The login terminal already carries a query, the session one does not.
  const sep = props.url.includes('?') ? '&' : '?'
  const query = size ? `${sep}cols=${size.cols}&rows=${size.rows}` : ''
  return `${scheme}://${window.location.host}${props.url}${query}`
}

function connect() {
  if (disposed) return
  state.value = 'connecting'

  const ws = new WebSocket(socketURL())
  ws.binaryType = 'arraybuffer'
  socket.value = ws

  ws.onopen = () => {
    attempt = 0
    state.value = 'open'
    sendResize()
    term.value?.focus()
  }

  ws.onmessage = (event) => {
    if (event.data instanceof ArrayBuffer) {
      term.value?.write(new Uint8Array(event.data))
    } else if (typeof event.data === 'string') {
      term.value?.write(event.data)
    }
  }

  ws.onclose = () => {
    state.value = 'closed'
    socket.value = null
    scheduleReconnect()
  }

  // onerror is always followed by onclose, which is where reconnecting happens.
  ws.onerror = () => ws.close()
}

// The session lives in tmux on the server, so reconnecting picks up exactly
// where the connection dropped. Backoff caps at 10s.
function scheduleReconnect() {
  if (disposed) return
  attempt += 1
  const delay = Math.min(500 * 2 ** (attempt - 1), 10000)
  window.clearTimeout(reconnectTimer)
  reconnectTimer = window.setTimeout(connect, delay)
}

function sendResize() {
  const ws = socket.value
  const size = term.value
  if (!ws || ws.readyState !== WebSocket.OPEN || !size) return
  ws.send(JSON.stringify({ type: 'resize', cols: size.cols, rows: size.rows }))
}

function onContainerResize() {
  window.clearTimeout(resizeTimer)
  resizeTimer = window.setTimeout(() => {
    fit.value?.fit()
    sendResize()
  }, 100)
}

function sendInput(data: string) {
  const ws = socket.value
  if (ws?.readyState === WebSocket.OPEN) ws.send(encoder.encode(data))
}

// A-Z only: covers every tmux/readline/shell binding the app actually needs
// (Ctrl-b for the tmux prefix, Ctrl-c, Ctrl-d, Ctrl-a, Ctrl-r...). Anything
// else passes through unarmed rather than being silently dropped.
function toControl(ch: string): string | null {
  if (ch.length !== 1 || !/[a-zA-Z]/.test(ch)) return null
  return String.fromCharCode(ch.toUpperCase().charCodeAt(0) - 64)
}

// The one path every keystroke takes, whether it came from the OS keyboard
// or a tap on the key row below: sticky Ctrl/Alt are applied here and
// disarmed, so they compose with a real key exactly once.
function press(bytes: string) {
  let out = bytes
  if (ctrlArmed.value) {
    out = toControl(out) ?? out
    ctrlArmed.value = false
  }
  if (altArmed.value) {
    out = '\x1b' + out
    altArmed.value = false
  }
  sendInput(out)
  term.value?.focus()
}

onMounted(() => {
  const terminal = new Terminal({
    cursorBlink: true,
    fontFamily: 'ui-monospace, "JetBrains Mono", Consolas, monospace',
    fontSize: 13,
    scrollback: 10000,
    theme: theme(),
  })
  const fitAddon = new FitAddon()
  terminal.loadAddon(fitAddon)
  terminal.open(host.value!)
  fitAddon.fit()

  terminal.onData((data) => press(data))

  term.value = terminal
  fit.value = fitAddon

  observer = new ResizeObserver(onContainerResize)
  observer.observe(host.value!)

  connect()
})

onBeforeUnmount(() => {
  disposed = true
  window.clearTimeout(resizeTimer)
  window.clearTimeout(reconnectTimer)
  observer?.disconnect()
  socket.value?.close()
  term.value?.dispose()
})
</script>

<template>
  <div class="pane">
    <div ref="host" class="screen" />
    <p v-if="state !== 'open'" class="overlay">
      {{ state === 'connecting' ? 'Connecting…' : 'Disconnected, reconnecting…' }}
    </p>

    <!-- The on-screen keyboard has none of these; below the breakpoint,
         the terminal is otherwise unusable (tmux's own prefix is Ctrl-b). -->
    <div class="keys">
      <button type="button" @mousedown.prevent @click="press('\x1b')">Esc</button>
      <button type="button" @mousedown.prevent @click="press('\t')">Tab</button>
      <button
        type="button"
        class="modifier"
        :class="{ armed: ctrlArmed }"
        @mousedown.prevent
        @click="ctrlArmed = !ctrlArmed"
      >
        Ctrl
      </button>
      <button
        type="button"
        class="modifier"
        :class="{ armed: altArmed }"
        @mousedown.prevent
        @click="altArmed = !altArmed"
      >
        Alt
      </button>
      <button type="button" @mousedown.prevent @click="press('\x1b[A')">↑</button>
      <button type="button" @mousedown.prevent @click="press('\x1b[B')">↓</button>
      <button type="button" @mousedown.prevent @click="press('\x1b[D')">←</button>
      <button type="button" @mousedown.prevent @click="press('\x1b[C')">→</button>
    </div>
  </div>
</template>

<style scoped>
.pane {
  position: relative;
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
  background: var(--bg);
}

.screen {
  flex: 1;
  min-height: 0;
  padding: 0.5rem;
  box-sizing: border-box;
}

.overlay {
  position: absolute;
  top: 0.75rem;
  right: 0.75rem;
  margin: 0;
  padding: 0.3rem 0.7rem;
  border: 1px solid var(--border);
  border-radius: 999px;
  background: var(--surface);
  color: var(--text-muted);
  font-size: 0.8rem;
}

/* The real keys exist above this breakpoint, so the row renders nothing
   there. */
.keys {
  display: none;
}

@media (max-width: 640px) {
  .keys {
    display: flex;
    flex: 0 0 auto;
    gap: 0.4rem;
    padding: 0.4rem 0.5rem;
    overflow-x: auto;
    border-top: 1px solid var(--border);
    background: var(--surface);
  }

  .keys button {
    flex: 0 0 auto;
    padding: 0.4rem 0.6rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    background: var(--bg);
    color: var(--text);
    font: inherit;
    font-size: 0.85rem;
    white-space: nowrap;
    cursor: pointer;
  }

  .keys .modifier.armed {
    background: var(--accent);
    border-color: var(--accent);
    color: var(--bg);
  }
}
</style>
