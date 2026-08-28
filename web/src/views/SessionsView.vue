<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import AppHeader from '../components/AppHeader.vue'
import NewSessionDialog from '../components/NewSessionDialog.vue'
import Spinner from '../components/Spinner.vue'
import StatusDot from '../components/StatusDot.vue'
import { ApiError, api, type Session } from '../api'
import { isProvisioning, sessionLabel } from '../status'

const router = useRouter()

const sessions = ref<Session[]>([])
const error = ref<string | null>(null)
const loaded = ref(false)
const dialogOpen = ref(false)
// The id of the session whose delete is armed, and whether to take the clone
// with it. Deleting the workspace is not undoable, so it is never the default.
const confirming = ref<string | null>(null)
const purge = ref(false)
const busy = ref<string | null>(null)

async function refresh() {
  try {
    sessions.value = await api.sessions.list()
    error.value = null
  } catch (e) {
    error.value = message(e)
  } finally {
    loaded.value = true
  }
}

async function act(session: Session, action: 'start' | 'stop') {
  busy.value = session.id
  try {
    await api.sessions[action](session.id)
    await refresh()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = null
  }
}

function armDelete(session: Session) {
  confirming.value = session.id
  purge.value = false
}

async function remove(session: Session) {
  busy.value = session.id
  try {
    await api.sessions.remove(session.id, purge.value)
    confirming.value = null
    await refresh()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = null
  }
}

function open(session: Session) {
  router.push({ name: 'session', params: { id: session.id } })
}

function created(session: Session) {
  dialogOpen.value = false
  // Straight to the session: provisioning is worth watching, and the view
  // shows each step as it happens.
  router.push({ name: 'session', params: { id: session.id } })
}

function message(e: unknown): string {
  if (e instanceof ApiError) return e.message
  return e instanceof Error ? e.message : String(e)
}

let timer: number
onMounted(() => {
  refresh()
  timer = window.setInterval(refresh, 3000)
})
onUnmounted(() => window.clearInterval(timer))
</script>

<template>
  <AppHeader />

  <main class="shell">
    <div class="title">
      <h1>Sessions</h1>
      <button type="button" class="primary" @click="dialogOpen = true">New session</button>
    </div>

    <p v-if="error" class="error">{{ error }}</p>

    <ul v-if="sessions.length" class="grid">
      <li v-for="session in sessions" :key="session.id" class="card">
        <div class="head">
          <div class="identity">
            <strong>{{ session.title }}</strong>
            <span class="repo">
              {{ session.repoFullName }}
              <span v-if="session.branch" class="branch">{{ session.branch }}</span>
            </span>
          </div>
          <StatusDot :status="session.status" />
        </div>

        <p class="detail">{{ sessionLabel(session.status) }}</p>
        <p v-if="session.error" class="error inline">{{ session.error }}</p>
        <p class="path"><code>{{ session.repoDir }}</code></p>

        <div v-if="confirming === session.id" class="confirm">
          <label>
            <input type="checkbox" v-model="purge" />
            also delete the clone on disk
          </label>
          <div class="actions">
            <button type="button" @click="confirming = null">Cancel</button>
            <button type="button" class="danger" :disabled="busy === session.id" @click="remove(session)">
              <Spinner v-if="busy === session.id" />Delete
            </button>
          </div>
        </div>

        <div v-else class="actions">
          <button
            type="button"
            :disabled="session.status !== 'running'"
            @click="open(session)"
          >
            Open
          </button>
          <button
            v-if="session.status === 'running'"
            type="button"
            :disabled="busy === session.id"
            @click="act(session, 'stop')"
          >
            <Spinner v-if="busy === session.id" />Stop
          </button>
          <button
            v-else-if="session.status === 'stopped'"
            type="button"
            :disabled="busy === session.id"
            @click="act(session, 'start')"
          >
            <Spinner v-if="busy === session.id" />Start
          </button>
          <span v-else-if="isProvisioning(session.status)" class="spacer" />
          <button type="button" class="danger" @click="armDelete(session)">Delete</button>
        </div>
      </li>
    </ul>

    <p v-else-if="loaded" class="hint">
      No sessions yet. Pick a repository and a base image to start one.
    </p>

    <NewSessionDialog v-if="dialogOpen" @close="dialogOpen = false" @created="created" />
  </main>
</template>

<style scoped>
.shell {
  max-width: 56rem;
  margin: 2.5rem auto;
  padding: 0 1.5rem;
}

.title {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 1.5rem;
}

h1 {
  margin: 0;
}

.grid {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  gap: 0.75rem;
  grid-template-columns: repeat(auto-fill, minmax(20rem, 1fr));
}

.card {
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
  padding: 1rem;
  border: 1px solid var(--border);
  border-radius: 8px;
}

.head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 1rem;
}

.identity {
  display: grid;
  gap: 0.15rem;
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

.detail {
  margin: 0;
  color: var(--text-muted);
  font-size: 0.9rem;
}

.path {
  margin: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.path code {
  color: var(--text-muted);
  font-size: 0.78rem;
}

.actions {
  display: flex;
  gap: 0.5rem;
  margin-top: auto;
  padding-top: 0.25rem;
}

.confirm {
  display: grid;
  gap: 0.5rem;
  margin-top: auto;
  padding-top: 0.25rem;
  color: var(--text-muted);
  font-size: 0.9rem;
}

.confirm label {
  display: flex;
  align-items: center;
  gap: 0.4rem;
  cursor: pointer;
}

.spacer {
  flex: 1;
}

button {
  padding: 0.4rem 0.8rem;
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

.primary {
  font-weight: 600;
}

.danger {
  margin-left: auto;
}

.danger:hover:not(:disabled) {
  border-color: var(--error);
  color: var(--error);
}

.error {
  padding: 0.6rem 0.8rem;
  border: 1px solid var(--error);
  border-radius: 6px;
  color: var(--error);
}

.error.inline {
  margin: 0;
  font-size: 0.85rem;
}

.hint {
  color: var(--text-muted);
}
</style>
