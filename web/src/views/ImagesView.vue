<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import type { ComponentPublicInstance } from 'vue'
import AppHeader from '../components/AppHeader.vue'
import StatusDot from '../components/StatusDot.vue'
import { ApiError, api, type Image, type ImageSource } from '../api'

const images = ref<Image[]>([])
const error = ref<string | null>(null)
const submitting = ref(false)

const name = ref('')
const sourceType = ref<ImageSource>('dockerfile')
const dockerfile = ref('')
const registryRef = ref('')

const openLogId = ref<string | null>(null)
const log = ref('')

// The log pane follows the output while it is being appended, the way `tail -f`
// does, and lets go the moment the reader scrolls away to look at something.
const logEl = ref<HTMLElement | null>(null)
const autoScroll = ref(true)

// A plain `ref="logEl"` would not work here: inside a v-for Vue collects refs
// into an array, so a function ref is what binds the single open pane.
function setLogEl(el: Element | ComponentPublicInstance | null) {
  logEl.value = (el as HTMLElement) ?? null
}
// How close to the bottom still counts as "at the bottom": fractional scroll
// positions from zoom or high-DPI displays never land exactly on zero.
const bottomThreshold = 16

function scrollToBottom() {
  const el = logEl.value
  if (el) el.scrollTop = el.scrollHeight
}

// Both the reader and scrollToBottom trigger this. A programmatic scroll lands
// at the bottom and therefore leaves following enabled; scrolling up turns it
// off, and scrolling back down turns it on again.
function onLogScroll() {
  const el = logEl.value
  if (!el) return
  autoScroll.value = el.scrollHeight - el.scrollTop - el.clientHeight <= bottomThreshold
}

watch(log, () => {
  if (autoScroll.value) scrollToBottom()
}, { flush: 'post' })

watch(autoScroll, (following) => {
  if (following) scrollToBottom()
})

const building = computed(() => images.value.some((i) => i.status === 'building'))

async function refresh() {
  try {
    images.value = await api.images.list()
    error.value = null
  } catch (e) {
    error.value = message(e)
  }
}

async function create() {
  submitting.value = true
  error.value = null
  try {
    const created = await api.images.create({
      name: name.value,
      sourceType: sourceType.value,
      dockerfile: sourceType.value === 'dockerfile' ? dockerfile.value : undefined,
      registryRef: sourceType.value === 'registry' ? registryRef.value : undefined,
    })
    name.value = ''
    registryRef.value = ''
    // Opening the log of a build that just started: follow it.
    openLogId.value = created.id
    log.value = ''
    autoScroll.value = true
    await refresh()
  } catch (e) {
    error.value = message(e)
  } finally {
    submitting.value = false
  }
}

async function remove(image: Image) {
  if (!window.confirm(`Delete image "${image.name}"?`)) return
  try {
    await api.images.remove(image.id)
    if (openLogId.value === image.id) openLogId.value = null
    await refresh()
  } catch (e) {
    error.value = message(e)
  }
}

function toggleLog(image: Image) {
  openLogId.value = openLogId.value === image.id ? null : image.id
  log.value = ''
  autoScroll.value = true
  if (openLogId.value) refreshLog()
}

async function refreshLog() {
  const id = openLogId.value
  if (!id) return
  try {
    log.value = (await api.images.log(id)).log
  } catch (e) {
    log.value = message(e)
  }
}

function message(e: unknown): string {
  if (e instanceof ApiError) return e.message
  return e instanceof Error ? e.message : String(e)
}

// The build runs on the server, so the page follows it by polling: every second
// while something is building, every five otherwise.
let timer: number
function schedule() {
  window.clearInterval(timer)
  timer = window.setInterval(
    async () => {
      await refresh()
      if (openLogId.value) await refreshLog()
    },
    building.value || openLogId.value ? 1000 : 5000,
  )
}
watch([building, openLogId], schedule)

onMounted(async () => {
  await refresh()
  schedule()
  try {
    dockerfile.value = (await api.images.template()).dockerfile
  } catch {
    // The template is a convenience; the form still works without it.
  }
})
onUnmounted(() => window.clearInterval(timer))
</script>

<template>
  <AppHeader />

  <main class="shell">
    <h1>Images</h1>
    <p class="intro">
      Base images sessions run from. An image must provide <code>git</code>, <code>tmux</code> and
      <code>claude</code> on the <code>PATH</code>; the repository is not baked in, it arrives as a
      bind mount on <code>/workspace</code>.
    </p>

    <p v-if="error" class="error">{{ error }}</p>

    <form class="form" @submit.prevent="create">
      <label class="field">
        <span>Name</span>
        <input v-model="name" required maxlength="64" placeholder="base" />
      </label>

      <div class="field">
        <span>Source</span>
        <div class="choices">
          <label><input type="radio" value="dockerfile" v-model="sourceType" /> Dockerfile</label>
          <label><input type="radio" value="registry" v-model="sourceType" /> Registry image</label>
        </div>
      </div>

      <label v-if="sourceType === 'dockerfile'" class="field">
        <span>Dockerfile</span>
        <textarea v-model="dockerfile" rows="14" spellcheck="false" required></textarea>
      </label>

      <label v-else class="field">
        <span>Image reference</span>
        <input v-model="registryRef" required placeholder="node:22-bookworm-slim" />
      </label>

      <button type="submit" :disabled="submitting">
        {{ sourceType === 'dockerfile' ? 'Build image' : 'Pull image' }}
      </button>
    </form>

    <ul class="list">
      <li v-for="image in images" :key="image.id">
        <div class="row">
          <div class="identity">
            <strong>{{ image.name }}</strong>
            <code>{{ image.imageRef || image.registryRef }}</code>
          </div>
          <StatusDot :status="image.status" />
          <div class="actions">
            <button type="button" @click="toggleLog(image)">
              {{ openLogId === image.id ? 'Hide log' : 'Log' }}
            </button>
            <button type="button" class="danger" @click="remove(image)">Delete</button>
          </div>
        </div>

        <p v-if="image.error" class="error inline">{{ image.error }}</p>

        <div v-if="openLogId === image.id" class="log-pane">
          <label class="follow">
            <input type="checkbox" v-model="autoScroll" />
            Follow output
          </label>
          <pre :ref="setLogEl" class="log" @scroll="onLogScroll">{{ log || 'No output yet.' }}</pre>
        </div>
      </li>
    </ul>

    <p v-if="!images.length" class="empty">No images yet.</p>
  </main>
</template>

<style scoped>
.shell {
  max-width: 56rem;
  margin: 2.5rem auto;
  padding: 0 1.5rem;
}

h1 {
  margin: 0 0 0.25rem;
}

.intro {
  margin: 0 0 2rem;
  color: var(--text-muted);
}

.error {
  padding: 0.6rem 0.8rem;
  border: 1px solid var(--error);
  border-radius: 6px;
  color: var(--error);
}

.error.inline {
  margin: 0.5rem 0 0;
  font-size: 0.9rem;
}

.form {
  display: grid;
  gap: 1rem;
  padding: 1.25rem;
  margin-bottom: 2.5rem;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--surface);
}

.field {
  display: grid;
  gap: 0.35rem;
}

.field > span {
  font-weight: 600;
  font-size: 0.9rem;
}

.choices {
  display: flex;
  gap: 1.25rem;
  color: var(--text-muted);
}

input,
textarea {
  padding: 0.5rem 0.6rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: var(--bg);
  color: var(--text);
  font: inherit;
}

textarea {
  font-family: var(--mono);
  font-size: 0.85rem;
  resize: vertical;
}

button {
  padding: 0.5rem 0.9rem;
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
  opacity: 0.5;
  cursor: default;
}

.form > button {
  justify-self: start;
  font-weight: 600;
}

.danger:hover {
  border-color: var(--error);
  color: var(--error);
}

.list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  gap: 0.75rem;
}

.list > li {
  padding: 0.9rem 1rem;
  border: 1px solid var(--border);
  border-radius: 8px;
}

.row {
  display: flex;
  align-items: center;
  gap: 1rem;
}

.identity {
  display: grid;
  gap: 0.15rem;
  flex: 1;
  min-width: 0;
}

.identity code {
  color: var(--text-muted);
  overflow-wrap: anywhere;
}

.actions {
  display: flex;
  gap: 0.5rem;
}

.log-pane {
  margin-top: 0.75rem;
}

.follow {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 0.4rem;
  margin-bottom: 0.35rem;
  color: var(--text-muted);
  font-size: 0.85rem;
  cursor: pointer;
  user-select: none;
}

.log {
  margin: 0;
  padding: 0.75rem;
  max-height: 22rem;
  overflow: auto;
  border-radius: 6px;
  background: var(--surface);
  font-size: 0.8rem;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

.empty {
  color: var(--text-muted);
}
</style>
