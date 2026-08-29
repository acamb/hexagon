<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppHeader from '../components/AppHeader.vue'
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

// Why the session runs without credentials, which is a different sentence for a
// session that has an account it was refused and one that has no account at all.
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
    error.value = null
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

    <div v-if="error && !session" class="notice error">{{ error }}</div>

    <template v-else-if="session">
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
          <button type="button" @click="cheatsheetOpen = true">tmux keys</button>
          <button type="button" @click="router.push({ name: 'sessions' })">All sessions</button>
        </div>
      </div>

      <TerminalPane v-if="session.status === 'running'" :session-id="session.id" class="terminal" />

      <div v-else class="notice">
        <p class="lead">{{ sessionLabel(session.status) }}</p>
        <p v-if="session.error" class="error inline">{{ session.error }}</p>
        <p v-if="isProvisioning(session.status)" class="hint">
          The terminal opens as soon as the container is up.
        </p>
        <p v-else-if="session.status === 'stopped'" class="hint">
          The workspace is still on disk at <code>{{ session.repoDir }}</code>. Starting the
          session brings the container back, with a fresh tmux.
        </p>
        <p v-else-if="session.status === 'gone'" class="hint">
          The container behind this session no longer exists. The workspace is still at
          <code>{{ session.repoDir }}</code>.
        </p>
      </div>
    </template>

    <div v-else class="notice">Loading…</div>

    <TmuxCheatsheet v-if="cheatsheetOpen" @close="cheatsheetOpen = false" />
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

.notice.error {
  color: var(--error);
}

.error.inline {
  margin: 0 0 0.35rem;
  color: var(--error);
}
</style>
