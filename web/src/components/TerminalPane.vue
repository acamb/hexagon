<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, shallowRef } from 'vue'
import { FitAddon } from '@xterm/addon-fit'
import { Terminal } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'

const props = defineProps<{ sessionId: string }>()

type Connection = 'connecting' | 'open' | 'closed'

const host = ref<HTMLElement | null>(null)
const state = ref<Connection>('connecting')
const term = shallowRef<Terminal | null>(null)
const fit = shallowRef<FitAddon | null>(null)
const socket = shallowRef<WebSocket | null>(null)

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
    background: read('--bg', '#131316'),
    foreground: read('--text', '#ececf0'),
    cursor: read('--accent', '#a78bfa'),
  }
}

function socketURL(): string {
  const scheme = window.location.protocol === 'https:' ? 'wss' : 'ws'
  const size = fit.value?.proposeDimensions()
  const query = size ? `?cols=${size.cols}&rows=${size.rows}` : ''
  return `${scheme}://${window.location.host}/api/sessions/${props.sessionId}/terminal${query}`
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

  terminal.onData((data) => {
    const ws = socket.value
    if (ws?.readyState === WebSocket.OPEN) ws.send(encoder.encode(data))
  })

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
  </div>
</template>

<style scoped>
.pane {
  position: relative;
  height: 100%;
  min-height: 0;
  background: var(--bg);
}

.screen {
  height: 100%;
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
</style>
