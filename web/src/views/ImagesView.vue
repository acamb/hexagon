<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import type { ComponentPublicInstance } from 'vue'
import AppHeader from '../components/AppHeader.vue'
import Notice from '../components/Notice.vue'
import AskClaude from '../components/AskClaude.vue'
import Spinner from '../components/Spinner.vue'
import StatusDot from '../components/StatusDot.vue'
import {
  ApiError,
  api,
  type Image,
  type ImageSource,
  type RestoreInspection,
  type Transfer,
} from '../api'

const images = ref<Image[]>([])
const error = ref<string | null>(null)
const submitting = ref(false)

const name = ref('')
const sourceType = ref<ImageSource>('dockerfile')
const dockerfile = ref('')
const compose = ref('')
const registryRef = ref('')

// canAsk is false when the server has no way to run Claude Code at all,
// canCompose when it has no `docker compose`. Each control is left out rather
// than offered as a button that always fails. askInContainer is the case in
// between: the control works, through a container, and says so.
const canAsk = ref(false)
const askInContainer = ref(false)
const canCompose = ref(false)

const removing = ref<string | null>(null)
const openLogId = ref<string | null>(null)
const log = ref('')

// Editing an existing image's source, one at a time: opening one panel closes
// whichever other was open, the same way the log pane already works.
const editingId = ref<string | null>(null)
const editDockerfile = ref('')
const editCompose = ref('')
const rebuilding = ref<string | null>(null)

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

// Backing up an image, one panel open at a time, the same way editing does.
const backupOpenId = ref<string | null>(null)
const backupWithSpec = ref(true)
const backupWithImage = ref(false)
const backingUp = ref<string | null>(null)
// The size docker reports for the image being backed up, read from the Stats
// page's own data when the panel opens: roughly how large asking for the
// export will make the file, not exact enough to be worth its own endpoint.
const backupImageSize = ref<number | null>(null)

const transfers = ref<Transfer[]>([])
const transfersInFlight = computed(() =>
  transfers.value.some((t) => t.status === 'pending' || t.status === 'running'),
)
const removingTransfer = ref<string | null>(null)

// Restoring: a file is uploaded and inspected, then the reader chooses what to
// do with what came back.
const restoreFileInput = ref<HTMLInputElement | null>(null)
const restoring = ref(false)
const restoreInspection = ref<RestoreInspection | null>(null)
const restoreOverwriteConfirm = ref(false)
const importing = ref(false)

function formatBytes(bytes: number): string {
  if (bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const exp = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
  const value = bytes / 1024 ** exp
  return `${exp === 0 ? value : value.toFixed(1)} ${units[exp]}`
}

async function refreshTransfers() {
  try {
    transfers.value = await api.transfers.list()
  } catch (e) {
    error.value = message(e)
  }
}

async function removeTransfer(t: Transfer) {
  removingTransfer.value = t.id
  try {
    await api.transfers.remove(t.id)
    await refreshTransfers()
  } catch (e) {
    error.value = message(e)
  } finally {
    removingTransfer.value = null
  }
}

async function toggleBackup(image: Image) {
  if (backupOpenId.value === image.id) {
    backupOpenId.value = null
    return
  }
  backupOpenId.value = image.id
  backupWithSpec.value = image.sourceType !== 'registry'
  backupWithImage.value = image.status === 'ready'
  backupImageSize.value = null
  try {
    const stats = await api.stats.get()
    backupImageSize.value = stats.docker.images.find((i) => i.tags.includes(image.imageRef ?? ''))?.size ?? null
  } catch {
    // The size is a convenience; the dialog works without it.
  }
}

async function startBackup(image: Image) {
  backingUp.value = image.id
  try {
    await api.images.backup(image.id, { withSpec: backupWithSpec.value, withImage: backupWithImage.value })
    backupOpenId.value = null
    await refreshTransfers()
  } catch (e) {
    error.value = message(e)
  } finally {
    backingUp.value = null
  }
}

function pickRestoreFile() {
  restoreFileInput.value?.click()
}

async function onRestoreFileChosen(e: Event) {
  const input = e.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return

  restoring.value = true
  error.value = null
  restoreInspection.value = null
  restoreOverwriteConfirm.value = false
  try {
    restoreInspection.value = await api.images.restore(file)
  } catch (e) {
    error.value = message(e)
  } finally {
    restoring.value = false
  }
}

// Spec-only restore never creates anything: it fills in the create form
// exactly as if the Dockerfile and compose file had been typed by hand, and
// the reader presses "Build image" themselves.
function loadRestoredFilesIntoForm() {
  const insp = restoreInspection.value
  if (!insp) return
  name.value = insp.name
  sourceType.value = insp.sourceType
  dockerfile.value = insp.dockerfile ?? ''
  compose.value = insp.compose ?? ''
  cancelRestore()
}

async function importRestoredImage() {
  const insp = restoreInspection.value
  if (!insp) return
  if (insp.nameExists && !restoreOverwriteConfirm.value) {
    restoreOverwriteConfirm.value = true
    return
  }
  importing.value = true
  error.value = null
  try {
    await api.images.import(insp.id, { name: insp.name, overwrite: insp.nameExists })
    cancelRestore()
    await refresh()
    await refreshTransfers()
  } catch (e) {
    error.value = message(e)
  } finally {
    importing.value = false
  }
}

function cancelRestore() {
  restoreInspection.value = null
  restoreOverwriteConfirm.value = false
}

async function refresh() {
  try {
    images.value = await api.images.list()
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
      dockerfile: sourceType.value === 'registry' ? undefined : dockerfile.value,
      compose: sourceType.value === 'compose' ? compose.value : undefined,
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
  removing.value = image.id
  try {
    await api.images.remove(image.id)
    if (openLogId.value === image.id) openLogId.value = null
    await refresh()
  } catch (e) {
    error.value = message(e)
  } finally {
    removing.value = null
  }
}

function toggleLog(image: Image) {
  openLogId.value = openLogId.value === image.id ? null : image.id
  log.value = ''
  autoScroll.value = true
  if (openLogId.value) refreshLog()
}

function toggleEdit(image: Image) {
  editingId.value = editingId.value === image.id ? null : image.id
  if (editingId.value) {
    editDockerfile.value = image.dockerfile ?? ''
    editCompose.value = image.compose ?? ''
  }
}

async function rebuild(image: Image) {
  rebuilding.value = image.id
  try {
    await api.images.rebuild(image.id, {
      dockerfile: editDockerfile.value,
      compose: image.sourceType === 'compose' ? editCompose.value : undefined,
    })
    editingId.value = null
    // Follow the new build, the way starting one from the form already does.
    openLogId.value = image.id
    log.value = ''
    autoScroll.value = true
    await refresh()
  } catch (e) {
    error.value = message(e)
  } finally {
    rebuilding.value = null
  }
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
// while something is building or a transfer is in flight, every five otherwise.
let timer: number
function schedule() {
  window.clearInterval(timer)
  timer = window.setInterval(
    async () => {
      await refresh()
      await refreshTransfers()
      if (openLogId.value) await refreshLog()
    },
    building.value || openLogId.value || transfersInFlight.value ? 1000 : 5000,
  )
}
watch([building, openLogId, transfersInFlight], schedule)

onMounted(async () => {
  await refresh()
  await refreshTransfers()
  schedule()
  try {
    const template = await api.images.template()
    dockerfile.value = template.dockerfile
    compose.value = template.compose
    canAsk.value = template.canAsk
    askInContainer.value = template.askInContainer
    canCompose.value = template.canCompose
  } catch {
    // The template is a convenience; the form still works without it.
  }
})
onUnmounted(() => window.clearInterval(timer))
</script>

<template>
  <AppHeader />

  <main class="shell">
    <div class="heading">
      <div>
        <h1>Images</h1>
        <p class="intro">
          Base images sessions run from. An image must provide <code>git</code>, <code>tmux</code> and
          <code>claude</code> on the <code>PATH</code>; the repository is not baked in, it arrives as a
          bind mount on <code>/workspace</code>.
        </p>
      </div>
      <div>
        <button type="button" :disabled="restoring" @click="pickRestoreFile">
          <Spinner v-if="restoring" />Restore from backup…
        </button>
        <input
          ref="restoreFileInput"
          type="file"
          accept=".gz,.tar.gz,application/gzip"
          class="visually-hidden"
          @change="onRestoreFileChosen"
        />
      </div>
    </div>

    <Notice v-if="error" kind="error" :message="error" class="alert" @dismiss="error = null" />

    <div v-if="restoreInspection" class="restore-panel">
      <h2>Restoring "{{ restoreInspection.name }}"</h2>
      <p class="hint">
        Source: {{ restoreInspection.sourceType }}<template v-if="restoreInspection.hasImage">
          · image export included, {{ formatBytes(restoreInspection.imageSize ?? 0) }}</template>
      </p>

      <div v-if="!restoreOverwriteConfirm" class="restore-choices">
        <button type="button" @click="loadRestoredFilesIntoForm">Load the files into the form</button>
        <button
          v-if="restoreInspection.hasImage"
          type="button"
          :disabled="importing"
          @click="importRestoredImage"
        >
          <Spinner v-if="importing" />Import the image
        </button>
        <button type="button" class="link" @click="cancelRestore">Cancel</button>
      </div>

      <div v-else class="restore-overwrite">
        <p>
          An image named "{{ restoreInspection.name }}" already exists. Importing replaces its
          Dockerfile, compose file and image with this backup's. Existing sessions keep their current
          container until it is rebuilt.
        </p>
        <button type="button" class="danger" :disabled="importing" @click="importRestoredImage">
          <Spinner v-if="importing" />Overwrite and import
        </button>
        <button type="button" @click="cancelRestore">Cancel</button>
      </div>
    </div>

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
          <label v-if="canCompose">
            <input type="radio" value="compose" v-model="sourceType" /> Dockerfile and compose
          </label>
        </div>
      </div>

      <div v-if="sourceType !== 'registry'" class="field">
        <label>
          <span>Dockerfile</span>
          <textarea v-model="dockerfile" rows="14" spellcheck="false" required></textarea>
        </label>

        <AskClaude
          v-if="canAsk"
          v-model="dockerfile"
          kind="dockerfile"
          placeholder="Ask Claude to change it: add the Go toolchain"
          :in-container="askInContainer"
          @failed="error = $event"
        />
      </div>

      <div v-if="sourceType === 'compose'" class="field">
        <label>
          <span>Compose file</span>
          <textarea v-model="compose" rows="14" spellcheck="false" required></textarea>
        </label>
        <p class="hint">
          The services a session needs beside the agent — a database, a cache, a queue. Hexagon
          adds the agent itself as a service named <code>hexagon</code>, starting after these and
          reaching them by their service name. The file is refused if any service uses
          <code>build</code>, bind mounts a host path, fixes a host port, asks for
          <code>privileged</code>, <code>cap_add</code>, <code>security_opt</code> or
          <code>devices</code>, puts <code>network_mode</code>, <code>pid</code>,
          <code>ipc</code> or <code>uts</code> on <code>host</code>, or runs as root.
        </p>

        <AskClaude
          v-if="canAsk"
          v-model="compose"
          kind="compose"
          placeholder="Ask Claude to change it: add a postgres 16"
          :in-container="askInContainer"
          @failed="error = $event"
        />
      </div>

      <label v-if="sourceType === 'registry'" class="field">
        <span>Image reference</span>
        <input v-model="registryRef" required placeholder="node:22-bookworm-slim" />
      </label>

      <button type="submit" :disabled="submitting">
        <Spinner v-if="submitting" />{{ sourceType === 'registry' ? 'Pull image' : 'Build image' }}
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
            <button
              v-if="image.sourceType !== 'registry'"
              type="button"
              :disabled="image.status === 'building' || image.status === 'pending'"
              @click="toggleEdit(image)"
            >
              {{ editingId === image.id ? 'Cancel edit' : 'Edit' }}
            </button>
            <button type="button" @click="toggleLog(image)">
              {{ openLogId === image.id ? 'Hide log' : 'Log' }}
            </button>
            <button type="button" @click="toggleBackup(image)">
              {{ backupOpenId === image.id ? 'Cancel backup' : 'Backup' }}
            </button>
            <button
              type="button"
              class="danger"
              :disabled="removing === image.id"
              @click="remove(image)"
            >
              <Spinner v-if="removing === image.id" />Delete
            </button>
          </div>
        </div>

        <p v-if="image.error" class="error inline">{{ image.error }}</p>

        <form v-if="editingId === image.id" class="edit-pane" @submit.prevent="rebuild(image)">
          <label class="field">
            <span>Dockerfile</span>
            <textarea v-model="editDockerfile" rows="14" spellcheck="false" required></textarea>
          </label>
          <AskClaude
            v-if="canAsk"
            v-model="editDockerfile"
            kind="dockerfile"
            placeholder="Ask Claude to change it: add the Go toolchain"
            :in-container="askInContainer"
            @failed="error = $event"
          />

          <template v-if="image.sourceType === 'compose'">
            <label class="field">
              <span>Compose file</span>
              <textarea v-model="editCompose" rows="14" spellcheck="false" required></textarea>
            </label>
            <AskClaude
              v-if="canAsk"
              v-model="editCompose"
              kind="compose"
              placeholder="Ask Claude to change it: add a postgres 16"
              :in-container="askInContainer"
              @failed="error = $event"
            />
          </template>

          <p class="hint">
            Existing sessions keep the image they were built from; only sessions started after this
            rebuild use the new content.
          </p>

          <button type="submit" :disabled="rebuilding === image.id">
            <Spinner v-if="rebuilding === image.id" />Rebuild image
          </button>
        </form>

        <div v-if="backupOpenId === image.id" class="backup-pane">
          <label class="checkbox">
            <input type="checkbox" v-model="backupWithSpec" />
            Dockerfile / compose
          </label>
          <label class="checkbox">
            <input type="checkbox" v-model="backupWithImage" :disabled="image.status !== 'ready'" />
            Image export
            <span v-if="image.status !== 'ready'" class="hint">(only once the image is ready)</span>
            <span v-else-if="backupImageSize" class="hint">(roughly {{ formatBytes(backupImageSize) }})</span>
          </label>
          <button
            type="button"
            :disabled="backingUp === image.id || (!backupWithSpec && !backupWithImage)"
            @click="startBackup(image)"
          >
            <Spinner v-if="backingUp === image.id" />Start backup
          </button>
        </div>

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

    <template v-if="transfers.length">
      <h2 class="transfers-heading">Backups and restores</h2>
      <ul class="list">
        <li v-for="t in transfers" :key="t.id">
          <div class="row">
            <div class="identity">
              <strong>{{ t.name || '(unnamed)' }}</strong>
              <code>{{ t.direction }}{{ t.withImage ? ' · with image' : ' · spec only' }}</code>
            </div>
            <StatusDot :status="t.status" />
            <div class="actions">
              <a
                v-if="t.status === 'ready' && t.direction === 'backup'"
                :href="api.transfers.downloadUrl(t.id)"
                download
              >
                Download{{ t.size ? ` (${formatBytes(t.size)})` : '' }}
              </a>
              <button
                type="button"
                class="danger"
                :disabled="removingTransfer === t.id"
                @click="removeTransfer(t)"
              >
                <Spinner v-if="removingTransfer === t.id" />Delete
              </button>
            </div>
          </div>
          <p v-if="t.error" class="error inline">{{ t.error }}</p>
        </li>
      </ul>
    </template>
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

.heading {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 1rem;
  flex-wrap: wrap;
}

.visually-hidden {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0, 0, 0, 0);
  white-space: nowrap;
  border: 0;
}

.restore-panel {
  display: grid;
  gap: 0.75rem;
  padding: 1.25rem;
  margin-bottom: 2rem;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--surface);
}

.restore-panel h2 {
  margin: 0;
  font-size: 1.05rem;
}

.restore-choices,
.restore-overwrite {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 0.75rem;
}

.restore-overwrite {
  flex-direction: column;
  align-items: flex-start;
}

.restore-overwrite p {
  margin: 0;
  color: var(--text-muted);
}

/* Scoped styles reach a child component's root element, which is how a page
   whose blocks stack with margins spaces a notice of its own. */
.alert {
  margin: 1rem 0;
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

.field,
/* The Dockerfile field wraps its label so the ask-Claude row can sit under the
   textarea without the click target of the label covering it. */
.field > label {
  display: grid;
  gap: 0.35rem;
}

.field > span,
.field > label > span {
  font-weight: 600;
  font-size: 0.9rem;
}

.hint {
  margin: 0;
  color: var(--text-muted);
  font-size: 0.85rem;
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
  flex-wrap: wrap;
  row-gap: 0.5rem;
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
  flex-wrap: wrap;
  row-gap: 0.4rem;
  gap: 0.5rem;
}

.edit-pane {
  display: grid;
  gap: 1rem;
  margin-top: 0.75rem;
  padding-top: 0.9rem;
  border-top: 1px solid var(--border);
}

.edit-pane > button {
  justify-self: start;
  font-weight: 600;
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

.backup-pane {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 1rem;
  margin-top: 0.75rem;
  padding-top: 0.9rem;
  border-top: 1px solid var(--border);
}

.checkbox {
  display: flex;
  align-items: center;
  gap: 0.4rem;
  cursor: pointer;
}

.checkbox .hint {
  margin: 0;
}

.transfers-heading {
  margin: 2.5rem 0 0.75rem;
  font-size: 1.1rem;
}
</style>
