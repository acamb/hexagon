<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppHeader from '../components/AppHeader.vue'
import StatusDot from '../components/StatusDot.vue'
import TerminalPane from '../components/TerminalPane.vue'
import { ApiError, api, type Session } from '../api'
import { isProvisioning, sessionLabel } from '../status'

const route = useRoute()
const router = useRouter()
const sessionId = route.params.id as string

const session = ref<Session | null>(null)
const error = ref<string | null>(null)
const busy = ref(false)

async function refresh() {
  try {
    session.value = await api.sessions.get(sessionId)
    error.value = null
  } catch (e) {
    error.value = message(e)
  }
  schedule()
}

async function start() {
  busy.value = true
  try {
    session.value = await api.sessions.start(sessionId)
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}

async function stop() {
  busy.value = true
  try {
    session.value = await api.sessions.stop(sessionId)
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
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
            {{ session.repoFullName }}
            <span v-if="session.branch" class="branch">{{ session.branch }}</span>
          </span>
        </div>

        <div class="right">
          <StatusDot :status="session.status" />
          <button
            v-if="session.status === 'running'"
            type="button"
            :disabled="busy"
            @click="stop"
          >
            Stop
          </button>
          <button
            v-else-if="session.status === 'stopped'"
            type="button"
            :disabled="busy"
            @click="start"
          >
            Start
          </button>
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
          The clone is still on disk at <code>{{ session.repoDir }}</code>. Starting the session
          brings the container back, with a fresh tmux.
        </p>
        <p v-else-if="session.status === 'gone'" class="hint">
          The container behind this session no longer exists. The clone is still at
          <code>{{ session.repoDir }}</code>.
        </p>
      </div>
    </template>

    <div v-else class="notice">Loading…</div>
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
