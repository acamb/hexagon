<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppHeader from '../components/AppHeader.vue'
import Notice from '../components/Notice.vue'
import SessionPortsDialog from '../components/SessionPortsDialog.vue'
import Spinner from '../components/Spinner.vue'
import StatusDot from '../components/StatusDot.vue'
import TerminalPane from '../components/TerminalPane.vue'
import TmuxCheatsheet from '../components/TmuxCheatsheet.vue'
import { ApiError, api, providerNames, type Session } from '../api'
import { isProvisioning, sessionLabel } from '../status'

const route = useRoute()
const router = useRouter()
const sessionId = route.params.id as string

const session = ref<Session | null>(null)
const error = ref<string | null>(null)
// Which action is in flight, not merely whether one is: all three controls are
// disabled while any of them runs, but only the one that was clicked spins.
const running = ref<'start' | 'stop' | 'autoClaude' | null>(null)
// Set when the switch is flipped on a session that is already up: the tmux
// session it is running was created with, or without, Claude Code as its
// command, so the change is only visible after a restart.
const pending = ref(false)
const cheatsheetOpen = ref(false)
// Whether the published ports are being edited, which only a stopped session
// allows: honouring the change rebuilds its container.
const portsOpen = ref(false)

// Why the session runs without credentials, which is a different sentence for a
// session that has an account it was refused and one that has no account at all.
// The published ports, shown only while the session is up: there is no binding
// to report before that, and Docker picks a new host port at every start.
const publishedPorts = computed(() =>
  session.value?.status === 'running' ? session.value.ports : [],
)

// The container ports the session is set up to publish, whether or not anything
// is up to publish them. It is what the stopped notice names, so the numbers in
// the dialog and the numbers in the page are the same list.
const portList = computed(() =>
  (session.value?.ports ?? []).map((port) => port.container).join(', '),
)

// Whether the session's ports reach beyond the machine running Hexagon, which
// decides both the caveat and where a link can usefully point.
const portsAreExposed = computed(() => {
  const address = session.value?.portAddress ?? '127.0.0.1'
  return address !== '127.0.0.1' && address !== 'localhost' && address !== '::1'
})

// The same honesty the README carries for code-server, said where the link is.
// The two cases are not the same warning: one is "anyone on this machine", the
// other is "anyone who can reach that address".
const portWarning = computed(() => {
  const address = session.value?.portAddress ?? '127.0.0.1'
  return portsAreExposed.value
    ? `Published on ${address} with nothing in front of it: anyone who can reach that address can reach this port while the session is up.`
    : 'Published on 127.0.0.1 with nothing in front of it: any other process on this machine can reach it while the session is up.'
})

// Where the link points. A port bound to 0.0.0.0 answers on every interface, so
// the useful host is the one this browser already reached Hexagon on rather than
// the address the binding names — 0.0.0.0 is not somewhere a browser can go.
function portHref(host: number): string {
  return portsAreExposed.value
    ? `${window.location.protocol}//${window.location.hostname}:${host}`
    : `http://127.0.0.1:${host}`
}

const noToken = computed(() => {
  const current = session.value
  if (!current || current.propagateToken) return ''
  return current.provider
    ? `This session has no ${providerNames[current.provider]} token: it can read the clone, but not fetch or push.`
    : 'This session carries no account credentials: nothing in it can fetch or push.'
})

async function refresh() {
  try {
    session.value = await api.sessions.get(sessionId)
  } catch (e) {
    error.value = message(e)
  }
  schedule()
}

async function setAutoClaude(auto: boolean) {
  running.value = 'autoClaude'
  try {
    session.value = await api.sessions.update(sessionId, { autoClaude: auto })
    pending.value = session.value.status === 'running'
  } catch (e) {
    error.value = message(e)
  } finally {
    running.value = null
  }
}

function portsChanged(updated: Session) {
  session.value = updated
  portsOpen.value = false
}

async function start() {
  running.value = 'start'
  pending.value = false
  try {
    session.value = await api.sessions.start(sessionId)
  } catch (e) {
    error.value = message(e)
  } finally {
    running.value = null
  }
}

async function stop() {
  running.value = 'stop'
  pending.value = false
  try {
    session.value = await api.sessions.stop(sessionId)
  } catch (e) {
    error.value = message(e)
  } finally {
    running.value = null
  }
}

function message(e: unknown): string {
  if (e instanceof ApiError) return e.message
  return e instanceof Error ? e.message : String(e)
}

// Poll quickly while the session is being set up, then leave it alone: once it
// is running the terminal's own socket is what matters.
let timer: number
function schedule() {
  window.clearTimeout(timer)
  const status = session.value?.status
  if (!status) return
  const delay = isProvisioning(status) ? 1000 : 10000
  timer = window.setTimeout(refresh, delay)
}

onMounted(refresh)
onUnmounted(() => window.clearTimeout(timer))
</script>

<template>
  <div class="layout">
    <AppHeader />

    <Notice v-if="error" kind="error" :message="error" class="banner" @dismiss="error = null" />

    <template v-if="session">
      <div class="meta">
        <div class="identity">
          <strong>{{ session.title }}</strong>
          <span class="repo">
            {{ session.repoFullName || 'No repository' }}
            <span v-if="session.branch" class="branch">{{ session.branch }}</span>
            <span v-if="session.provider" class="branch">{{ providerNames[session.provider] }}</span>
            <!-- A fact about the session rather than a control: the credentials
                 are part of the container's environment, fixed when it was
                 created. -->
            <span v-if="!session.propagateToken" class="branch" :title="noToken">no token</span>
          </span>
        </div>

        <div class="right">
          <span v-if="pending" class="pending">Applies the next time this session starts</span>
          <label class="toggle" title="Start Claude Code in this session's tmux, or leave a shell">
            <input
              type="checkbox"
              :checked="session.autoClaude"
              :disabled="running !== null"
              @change="setAutoClaude(($event.target as HTMLInputElement).checked)"
            />
            <Spinner v-if="running === 'autoClaude'" />
            Start Claude
          </label>

          <StatusDot :status="session.status" />
          <button
            v-if="session.status === 'running'"
            type="button"
            :disabled="running !== null"
            @click="stop"
          >
            <Spinner v-if="running === 'stop'" />Stop
          </button>
          <button
            v-else-if="session.status === 'stopped'"
            type="button"
            :disabled="running !== null"
            @click="start"
          >
            <Spinner v-if="running === 'start'" />Start
          </button>
          <a
            v-if="session.vscode && session.status === 'running'"
            class="button"
            :href="`/api/sessions/${session.id}/vscode/`"
            target="_blank"
            rel="noopener"
          >
            VS Code
          </a>
          <span
            v-for="port in publishedPorts"
            :key="port.container"
            class="port"
            :class="{ exposed: portsAreExposed }"
          >
            <a v-if="port.host" :href="portHref(port.host)" target="_blank" rel="noopener"
              :title="portWarning">
              {{ port.container }} → {{ session.portAddress }}:{{ port.host }}
            </a>
            <span v-else :title="portWarning">{{ port.container }} → not published yet</span>
          </span>
          <button
            v-if="session.status === 'stopped'"
            type="button"
            :disabled="running !== null"
            @click="portsOpen = true"
          >
            Ports
          </button>
          <button type="button" @click="cheatsheetOpen = true">tmux keys</button>
          <button type="button" @click="router.push({ name: 'sessions' })">All sessions</button>
        </div>
      </div>

      <TerminalPane
        v-if="session.status === 'running'"
        :url="`/api/sessions/${session.id}/terminal`"
        class="terminal"
      />

      <div v-else class="notice">
        <p class="lead">{{ sessionLabel(session.status) }}</p>
        <p v-if="session.error" class="error inline">{{ session.error }}</p>
        <p v-if="isProvisioning(session.status)" class="hint">
          The terminal opens as soon as the container is up.
        </p>
        <p v-else-if="session.status === 'stopped'" class="hint">
          The workspace is still on disk at <code>{{ session.repoDir }}</code>. Starting the
          session brings the container back, with a fresh tmux.
          <template v-if="session.ports.length">
            It publishes <code>{{ portList }}</code> on <code>{{ session.portAddress }}</code>,
            and that is editable from here while it is down.
          </template>
          <template v-else>
            It publishes no ports, which is editable from here while it is down.
          </template>
        </p>
        <p v-else-if="session.status === 'gone'" class="hint">
          The container behind this session no longer exists. The workspace is still at
          <code>{{ session.repoDir }}</code>.
        </p>
      </div>
    </template>

    <!-- Only while nothing has failed: a session that could not be loaded has
         its reason on the screen already, and "Loading…" under it would be a
         second, wrong answer. -->
    <div v-else-if="!error" class="notice">Loading…</div>

    <TmuxCheatsheet v-if="cheatsheetOpen" @close="cheatsheetOpen = false" />
    <SessionPortsDialog
      v-if="portsOpen && session"
      :session="session"
      @close="portsOpen = false"
      @updated="portsChanged"
    />
  </div>
</template>

<style scoped>
.layout {
  display: flex;
  flex-direction: column;
  height: 100svh;
}

.meta {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 1rem;
  padding: 0.6rem 1.5rem;
  border-bottom: 1px solid var(--border);
}

.identity {
  display: grid;
  gap: 0.1rem;
  min-width: 0;
}

.repo {
  color: var(--text-muted);
  font-size: 0.9rem;
}

.branch {
  margin-left: 0.35rem;
  padding: 0 0.4rem;
  border: 1px solid var(--border);
  border-radius: 999px;
  font-size: 0.8rem;
}

.right {
  display: flex;
  align-items: center;
  gap: 0.75rem;
}

.toggle {
  display: flex;
  align-items: center;
  gap: 0.35rem;
  font-size: 0.9rem;
  cursor: pointer;
}

.pending {
  color: var(--text-muted);
  font-size: 0.85rem;
}

.port {
  padding: 0.1rem 0.5rem;
  border: 1px solid var(--border);
  border-radius: 999px;
  color: var(--text-muted);
  font-size: 0.8rem;
  white-space: nowrap;
}

.port a {
  color: var(--accent);
}

/* A port anyone on the network can reach does not look like one only this
   machine can. */
.port.exposed {
  border-color: var(--warning);
  color: var(--warning);
}

.port.exposed a {
  color: var(--warning);
}

button {
  padding: 0.35rem 0.75rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: var(--bg);
  color: var(--text);
  font: inherit;
  cursor: pointer;
}

button:hover:not(:disabled) {
  border-color: var(--accent);
}

button:disabled {
  opacity: 0.45;
  cursor: default;
}

a.button {
  padding: 0.35rem 0.75rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: var(--bg);
  color: var(--text);
  font: inherit;
  text-decoration: none;
}

a.button:hover {
  border-color: var(--accent);
}

.terminal {
  flex: 1;
  min-height: 0;
}

.notice {
  padding: 1.5rem;
  color: var(--text-muted);
}

.lead {
  margin: 0 0 0.35rem;
  color: var(--text);
  font-weight: 600;
}

.hint {
  margin: 0;
  max-width: 42rem;
}

/* Scoped styles reach a child component's root element. The layout is a flex
   column with no gap, so the banner brings its own room. */
.banner {
  margin: 0.75rem 1.5rem;
}

.error.inline {
  margin: 0 0 0.35rem;
  color: var(--error);
}
</style>
