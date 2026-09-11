<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import AppHeader from '../components/AppHeader.vue'
import Notice from '../components/Notice.vue'
import Spinner from '../components/Spinner.vue'
import { ApiError, api, type DockerImage, type Stats } from '../api'

const stats = ref<Stats | null>(null)
const error = ref<string | null>(null)
const notice = ref<{ kind: 'error' | 'success'; text: string } | null>(null)
const pruningImages = ref(false)
const pruningContainers = ref(false)

async function refresh() {
  try {
    stats.value = await api.stats.get()
    // A refresh must never clear a notice: it reports what a request answered,
    // and a poll succeeding a second after the user read it is not that.
    error.value = null
  } catch (e) {
    error.value = message(e)
  }
}

function message(e: unknown): string {
  if (e instanceof ApiError) return e.message
  return e instanceof Error ? e.message : String(e)
}

// Sorted largest first: the page exists to answer "what is eating the disk".
const sortedImages = computed(() =>
  [...(stats.value?.docker.images ?? [])].sort((a, b) => b.size - a.size),
)

// What the images prune button would remove right now, computed the same way
// the server decides it: not in use, and not a row in the images table. Shown
// in the confirmation so the button never surprises anyone about to press it.
const unusedImages = computed(() => sortedImages.value.filter((i) => !i.inUse && !i.registered))
const unusedImagesBytes = computed(() => unusedImages.value.reduce((sum, i) => sum + i.size, 0))

function formatBytes(bytes: number): string {
  if (bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const exp = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
  const value = bytes / 1024 ** exp
  return `${exp === 0 ? value : value.toFixed(1)} ${units[exp]}`
}

function formatAge(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime()
  const days = Math.floor(ms / 86_400_000)
  if (days >= 1) return `${days} day${days === 1 ? '' : 's'} ago`
  const hours = Math.floor(ms / 3_600_000)
  if (hours >= 1) return `${hours} hour${hours === 1 ? '' : 's'} ago`
  const minutes = Math.max(0, Math.floor(ms / 60_000))
  return `${minutes} minute${minutes === 1 ? '' : 's'} ago`
}

function shortID(id: string): string {
  return id.replace(/^sha256:/, '').slice(0, 12)
}

function label(image: DockerImage): string {
  return image.tags[0] ?? shortID(image.id)
}

function usedFraction(total: number, free: number): number {
  if (total <= 0) return 0
  return Math.min(1, Math.max(0, (total - free) / total))
}

async function pruneImages() {
  const images = unusedImages.value
  const question =
    images.length === 0
      ? null
      : `Remove ${images.length} unused image${images.length === 1 ? '' : 's'}, freeing ${formatBytes(unusedImagesBytes.value)}?`
  if (images.length === 0) {
    notice.value = { kind: 'success', text: 'Nothing to remove: every image is either registered or in use.' }
    return
  }
  if (!window.confirm(question!)) return

  pruningImages.value = true
  try {
    const result = await api.stats.pruneImages()
    notice.value = {
      kind: 'success',
      text: `Removed ${result.removed} image${result.removed === 1 ? '' : 's'}, freeing ${formatBytes(result.reclaimed)}.`,
    }
    await refresh()
  } catch (e) {
    notice.value = { kind: 'error', text: message(e) }
  } finally {
    pruningImages.value = false
  }
}

async function pruneContainers() {
  if (!window.confirm('Remove every stopped container Hexagon does not manage?')) return

  pruningContainers.value = true
  try {
    const result = await api.stats.pruneContainers()
    notice.value = {
      kind: 'success',
      text: `Removed ${result.removed} container${result.removed === 1 ? '' : 's'}, freeing ${formatBytes(result.reclaimed)}.`,
    }
    await refresh()
  } catch (e) {
    notice.value = { kind: 'error', text: message(e) }
  } finally {
    pruningContainers.value = false
  }
}

let timer: number
onMounted(() => {
  refresh()
  timer = window.setInterval(refresh, 5000)
})
onUnmounted(() => window.clearInterval(timer))
</script>

<template>
  <AppHeader />

  <main class="shell">
    <h1>Stats</h1>

    <Notice v-if="error" kind="error" :message="error" class="alert" @dismiss="error = null" />
    <Notice
      v-if="notice"
      :kind="notice.kind"
      :message="notice.text"
      class="alert"
      @dismiss="notice = null"
    />

    <section v-if="stats" class="host">
      <h2>This machine</h2>

      <div v-if="!stats.host.available" class="hint">
        Host figures are not available on this platform.
      </div>
      <div v-else class="gauges">
        <div class="gauge">
          <div class="gauge-label">
            <span>CPU</span>
            <span v-if="stats.host.cpu!.usedPercent !== undefined" class="gauge-value">
              {{ stats.host.cpu!.usedPercent!.toFixed(0) }}%
            </span>
            <span v-else class="gauge-value">load {{ stats.host.cpu!.load.map((l) => l.toFixed(2)).join(', ') }}</span>
          </div>
          <div class="bar">
            <div
              v-if="stats.host.cpu!.usedPercent !== undefined"
              class="fill"
              :style="{ width: stats.host.cpu!.usedPercent + '%' }"
            />
          </div>
          <div class="gauge-detail">{{ stats.host.cpu!.cores }} core{{ stats.host.cpu!.cores === 1 ? '' : 's' }}</div>
        </div>

        <div class="gauge">
          <div class="gauge-label">
            <span>Memory</span>
            <span class="gauge-value">
              {{ formatBytes(stats.host.memory!.total - stats.host.memory!.available) }} / {{ formatBytes(stats.host.memory!.total) }}
            </span>
          </div>
          <div class="bar">
            <div
              class="fill"
              :style="{ width: usedFraction(stats.host.memory!.total, stats.host.memory!.available) * 100 + '%' }"
            />
          </div>
          <div class="gauge-detail">{{ formatBytes(stats.host.memory!.available) }} available</div>
        </div>

        <div v-for="fs in stats.host.filesystems" :key="fs.path" class="gauge">
          <div class="gauge-label">
            <span>Disk</span>
            <span class="gauge-value">{{ formatBytes(fs.total - fs.free) }} / {{ formatBytes(fs.total) }}</span>
          </div>
          <div class="bar">
            <div class="fill" :style="{ width: usedFraction(fs.total, fs.free) * 100 + '%' }" />
          </div>
          <div class="gauge-detail"><code>{{ fs.path }}</code></div>
        </div>
      </div>
    </section>

    <section v-if="stats" class="docker">
      <h2>Docker</h2>
      <p v-if="!stats.docker.sameMachine" class="hint">
        Docker runs on {{ stats.docker.host || 'another host' }}, a different machine from this
        server: the figures above and the ones below are not the same computer.
      </p>

      <div class="usage">
        <div class="usage-figure">
          <span class="usage-value">{{ formatBytes(stats.docker.usage.images) }}</span>
          <span class="usage-label">Images</span>
        </div>
        <div class="usage-figure">
          <span class="usage-value">{{ formatBytes(stats.docker.usage.containers) }}</span>
          <span class="usage-label">Containers</span>
        </div>
        <div class="usage-figure">
          <span class="usage-value">{{ formatBytes(stats.docker.usage.volumes) }}</span>
          <span class="usage-label">Volumes</span>
        </div>
        <div class="usage-figure">
          <span class="usage-value">{{ formatBytes(stats.docker.usage.buildCache) }}</span>
          <span class="usage-label">Build cache</span>
        </div>
      </div>
      <p class="hint">
        The buttons below can reclaim images and containers. Volumes and build cache are not
        touched: a volume is a session's own data, and build cache is what makes the next build
        fast.
      </p>

      <div class="prune-actions">
        <button type="button" :disabled="pruningImages" @click="pruneImages">
          <Spinner v-if="pruningImages" />Prune unused images
        </button>
        <button type="button" :disabled="pruningContainers" @click="pruneContainers">
          <Spinner v-if="pruningContainers" />Prune stopped containers
        </button>
      </div>

      <table v-if="sortedImages.length" class="images">
        <thead>
          <tr>
            <th>Image</th>
            <th>Size</th>
            <th>Age</th>
            <th>Containers</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="image in sortedImages" :key="image.id">
            <td><code>{{ label(image) }}</code></td>
            <td>{{ formatBytes(image.size) }}</td>
            <td>{{ formatAge(image.created) }}</td>
            <td>{{ image.containers }}</td>
            <td>
              <span v-if="image.inUse" class="badge in-use">In use</span>
              <span v-if="image.registered" class="badge registered">Registered</span>
              <span v-if="image.dangling" class="badge dangling">Dangling</span>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-else class="hint">No images on the daemon.</p>
    </section>
  </main>
</template>

<style scoped>
.shell {
  max-width: 56rem;
  margin: 2.5rem auto;
  padding: 0 1.5rem;
}

h1 {
  margin: 0 0 1.5rem;
}

h2 {
  margin: 0 0 0.75rem;
  font-size: 1.05rem;
}

section {
  margin-bottom: 2rem;
}

.hint {
  color: var(--text-muted);
}

.gauges {
  display: grid;
  gap: 1.25rem;
  grid-template-columns: repeat(auto-fill, minmax(min(16rem, 100%), 1fr));
}

.gauge-label {
  display: flex;
  justify-content: space-between;
  font-size: 0.9rem;
  margin-bottom: 0.3rem;
}

.gauge-value {
  color: var(--text-muted);
}

.bar {
  height: 0.5rem;
  border-radius: 999px;
  background: var(--surface);
  border: 1px solid var(--border);
  overflow: hidden;
}

.fill {
  height: 100%;
  background: var(--accent);
}

.gauge-detail {
  margin-top: 0.3rem;
  font-size: 0.8rem;
  color: var(--text-muted);
}

.usage {
  display: grid;
  gap: 1rem;
  grid-template-columns: repeat(auto-fill, minmax(min(10rem, 100%), 1fr));
  margin-bottom: 0.75rem;
}

.usage-figure {
  display: flex;
  flex-direction: column;
  padding: 0.75rem 1rem;
  border: 1px solid var(--border);
  border-radius: 8px;
}

.usage-value {
  font-size: 1.2rem;
  font-weight: 600;
}

.usage-label {
  color: var(--text-muted);
  font-size: 0.85rem;
}

.prune-actions {
  display: flex;
  gap: 0.5rem;
  margin: 1rem 0;
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

.images {
  width: 100%;
  border-collapse: collapse;
  font-size: 0.9rem;
}

.images th,
.images td {
  padding: 0.4rem 0.6rem;
  text-align: left;
  border-bottom: 1px solid var(--border);
}

.images th {
  color: var(--text-muted);
  font-weight: 500;
}

.badge {
  padding: 0.1rem 0.5rem;
  border: 1px solid var(--border);
  border-radius: 999px;
  font-size: 0.75rem;
  white-space: nowrap;
}

.badge.in-use {
  border-color: var(--ok);
  color: var(--ok);
}

.badge.registered {
  border-color: var(--accent);
  color: var(--accent);
}

.badge.dangling {
  border-color: var(--warning);
  color: var(--warning);
}

.alert {
  margin: 1rem 0;
}
</style>
