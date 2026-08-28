<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import Spinner from './Spinner.vue'
import { ApiError, api, type Image, type Repo, type Session } from '../api'

const emit = defineEmits<{ close: []; created: [session: Session] }>()

const repos = ref<Repo[]>([])
const images = ref<Image[]>([])
const loading = ref(true)
const submitting = ref(false)
const error = ref<string | null>(null)

const filter = ref('')
const selected = ref<Repo | null>(null)
const branch = ref('')
const imageId = ref('')
const title = ref('')
// On by default: running Claude Code is what a session is for. Turning it off
// leaves the tmux session at a shell prompt.
const autoClaude = ref(true)

// Only images that finished building can start a session.
const usableImages = computed(() => images.value.filter((i) => i.status === 'ready'))

const matches = computed(() => {
  const needle = filter.value.trim().toLowerCase()
  const list = needle
    ? repos.value.filter((r) => r.fullName.toLowerCase().includes(needle))
    : repos.value
  return list.slice(0, 100)
})

function choose(repo: Repo) {
  selected.value = repo
  branch.value = repo.defaultBranch
}

async function load(refresh = false) {
  loading.value = true
  error.value = null
  try {
    const [repoList, imageList] = await Promise.all([api.github.repos(refresh), api.images.list()])
    repos.value = repoList
    images.value = imageList
    if (!imageId.value) imageId.value = usableImages.value[0]?.id ?? ''
  } catch (e) {
    error.value = message(e)
  } finally {
    loading.value = false
  }
}

async function submit() {
  if (!selected.value || !imageId.value) return
  submitting.value = true
  error.value = null
  try {
    const session = await api.sessions.create({
      repoFullName: selected.value.fullName,
      branch: branch.value.trim() || undefined,
      imageId: imageId.value,
      title: title.value.trim() || undefined,
      autoClaude: autoClaude.value,
    })
    emit('created', session)
  } catch (e) {
    error.value = message(e)
  } finally {
    submitting.value = false
  }
}

function message(e: unknown): string {
  if (e instanceof ApiError) return e.message
  return e instanceof Error ? e.message : String(e)
}

onMounted(() => load())
</script>

<template>
  <div class="backdrop" @click.self="emit('close')">
    <section class="dialog">
      <header>
        <h2>New session</h2>
        <button type="button" class="icon" @click="emit('close')" aria-label="Close">×</button>
      </header>

      <p v-if="error" class="error">{{ error }}</p>

      <p v-if="loading" class="hint">Loading your repositories…</p>

      <template v-else>
        <p v-if="!usableImages.length" class="hint">
          No image is ready yet. Build one on the <RouterLink :to="{ name: 'images' }">Images</RouterLink>
          page first.
        </p>

        <form @submit.prevent="submit">
          <label class="field">
            <span>
              Repository
              <button type="button" class="link" @click="load(true)">refresh</button>
            </span>
            <input v-model="filter" placeholder="Filter by name" />
          </label>

          <ul class="repos">
            <li v-for="repo in matches" :key="repo.fullName">
              <button
                type="button"
                :class="{ chosen: selected?.fullName === repo.fullName }"
                @click="choose(repo)"
              >
                <span class="name">{{ repo.fullName }}</span>
                <span v-if="repo.private" class="tag">private</span>
                <span class="desc">{{ repo.description }}</span>
              </button>
            </li>
            <li v-if="!matches.length" class="hint">No repository matches.</li>
          </ul>

          <div class="row">
            <label class="field">
              <span>Branch</span>
              <input v-model="branch" :placeholder="selected?.defaultBranch || 'default branch'" />
            </label>

            <label class="field">
              <span>Image</span>
              <select v-model="imageId">
                <option v-for="image in usableImages" :key="image.id" :value="image.id">
                  {{ image.name }}
                </option>
              </select>
            </label>
          </div>

          <label class="field">
            <span>Title <em>optional</em></span>
            <input v-model="title" :placeholder="selected?.fullName || 'Repository name'" />
          </label>

          <label class="toggle">
            <input type="checkbox" v-model="autoClaude" />
            <span>
              Start Claude Code automatically
              <em>Otherwise the session opens at a shell prompt inside tmux.</em>
            </span>
          </label>

          <footer>
            <button type="button" @click="emit('close')">Cancel</button>
            <button type="submit" class="primary" :disabled="!selected || !imageId || submitting">
              <Spinner v-if="submitting" />{{ submitting ? 'Creating…' : 'Create session' }}
            </button>
          </footer>
        </form>
      </template>
    </section>
  </div>
</template>

<style scoped>
/* The backdrop and the panel itself come from style.css, shared with the other
   dialog; only this one's width is its own. */
.dialog {
  width: min(38rem, 100%);
}

.field {
  display: grid;
  gap: 0.35rem;
  flex: 1;
}

.field > span {
  display: flex;
  align-items: baseline;
  gap: 0.5rem;
  font-weight: 600;
  font-size: 0.9rem;
}

.field em {
  color: var(--text-muted);
  font-weight: 400;
  font-style: normal;
}

.row {
  display: flex;
  gap: 1rem;
}

.toggle {
  display: flex;
  align-items: baseline;
  gap: 0.5rem;
  font-size: 0.9rem;
  font-weight: 600;
}

.toggle em {
  display: block;
  color: var(--text-muted);
  font-weight: 400;
  font-style: normal;
}

/* The input rule below is written for text fields; a checkbox wants none of it. */
.toggle input {
  padding: 0;
  border: none;
  background: none;
}

input,
select {
  padding: 0.5rem 0.6rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: var(--bg);
  color: var(--text);
  font: inherit;
}

form {
  display: grid;
  gap: 1rem;
}

.repos {
  list-style: none;
  margin: 0;
  padding: 0;
  max-height: 16rem;
  overflow: auto;
  border: 1px solid var(--border);
  border-radius: 6px;
}

.repos button {
  display: flex;
  align-items: baseline;
  gap: 0.5rem;
  width: 100%;
  padding: 0.45rem 0.7rem;
  border: none;
  border-bottom: 1px solid var(--border);
  background: transparent;
  color: var(--text);
  font: inherit;
  text-align: left;
  cursor: pointer;
}

.repos button:hover {
  background: var(--surface);
}

.repos button.chosen {
  background: var(--surface);
  box-shadow: inset 3px 0 0 var(--accent);
}

.name {
  font-weight: 600;
}

.tag {
  padding: 0 0.35rem;
  border: 1px solid var(--border);
  border-radius: 999px;
  color: var(--text-muted);
  font-size: 0.75rem;
}

.desc {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--text-muted);
  font-size: 0.85rem;
}

footer {
  display: flex;
  justify-content: flex-end;
  gap: 0.6rem;
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

.primary {
  font-weight: 600;
}

.link {
  padding: 0;
  border: none;
  background: none;
  color: var(--accent);
  font: inherit;
  font-size: 0.85rem;
  font-weight: 400;
  cursor: pointer;
}

.hint {
  margin: 0;
  color: var(--text-muted);
}

.error {
  margin: 0;
  padding: 0.6rem 0.8rem;
  border: 1px solid var(--error);
  border-radius: 6px;
  color: var(--error);
}
</style>
