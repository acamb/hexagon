<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import Spinner from './Spinner.vue'
import Notice from './Notice.vue'
import {
  ApiError,
  api,
  providerNames,
  type Account,
  type Image,
  type ProviderKind,
  type Repo,
  type Session,
} from '../api'

const emit = defineEmits<{ close: []; created: [session: Session] }>()

const repos = ref<Repo[]>([])
const images = ref<Image[]>([])
const accounts = ref<Account[]>([])
const loading = ref(true)
const submitting = ref(false)
const error = ref<string | null>(null)

const filter = ref('')
// Which account's repositories to show. Empty means all of them, which is the
// useful default: typing part of a name is how anyone with more than a handful
// of repositories finds one.
const only = ref<ProviderKind | ''>('')
// Accounts that could not be reached, so the list can say what is missing
// instead of quietly being short.
const failed = ref<Partial<Record<ProviderKind, string>>>({})
const selected = ref<Repo | null>(null)
const branch = ref('')
const imageId = ref('')
const title = ref('')
// On by default: running Claude Code is what a session is for. Turning it off
// leaves the tmux session at a shell prompt.
const autoClaude = ref(true)
// On by default too: an agent that cannot push is half of one. Off gives the
// session the clone and nothing to authenticate with, and unlike the switch
// above it cannot be changed later.
const propagateToken = ref(true)
// Off by default, unlike the two switches above: it costs a mount and a
// published port, and a session that never opens the editor should carry
// neither. It has to be chosen now because the container is built for it.
const vscode = ref(false)
// Container ports to publish, typed as a list: "3000, 5173". Empty by default —
// a session that publishes nothing is the usual one — and, like the switch
// above, fixed when the container is created.
const ports = ref('')
// Which host interface those ports bind. 0.0.0.0 by default because a Hexagon
// on a remote machine is the case that needs published ports at all, and a port
// on loopback there is reachable by nobody. What that costs is spelled out
// under the field rather than assumed to be understood.
const portAddress = ref('0.0.0.0')

// Whether the chosen address reaches beyond this machine, which is what the
// warning is about. Anything that is not a loopback address does.
const portsAreExposed = computed(() => {
  const value = portAddress.value.trim()
  return (
    parsedPorts.value.length > 0 &&
    value !== '' &&
    value !== '127.0.0.1' &&
    value !== 'localhost' &&
    value !== '::1'
  )
})

// The ports as the API takes them. Anything that is not a number is dropped
// here and refused by the server if it somehow gets through: this is a
// convenience, not the check.
const parsedPorts = computed(() =>
  ports.value
    .split(/[\s,]+/)
    .filter(Boolean)
    .map(Number)
    .filter((p) => Number.isInteger(p) && p > 0 && p < 65536),
)
// A session with no repository at all: an empty /workspace, and whatever the
// user does in it.
const withoutRepo = ref(false)
// Which account such a session carries, empty for none. It is the same choice
// as the checkbox above, made where there is no repository to imply an account.
// Empty by default: with nothing cloned there is no operation that needs a
// credential until its user thinks of one.
const tokenProvider = ref<ProviderKind | ''>('')

// The accounts a repository-less session can take a token from.
const connected = computed(() => accounts.value.filter((a) => a.connected))

// What the server names such a session when the title is left empty.
const imageName = computed(() => usableImages.value.find((i) => i.id === imageId.value)?.name ?? '')

// Only images that finished building can start a session.
const usableImages = computed(() => images.value.filter((i) => i.status === 'ready'))

// The providers with something to show, so the filter only appears when there
// is a choice to make.
const sources = computed(() => [...new Set(repos.value.map((r) => r.provider))])

const matches = computed(() => {
  const needle = filter.value.trim().toLowerCase()
  const list = repos.value.filter(
    (r) =>
      (!only.value || r.provider === only.value) &&
      (!needle || r.fullName.toLowerCase().includes(needle)),
  )
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
    const [listing, imageList, accountList] = await Promise.all([
      api.repos(refresh),
      api.images.list(),
      api.accounts.list(),
    ])
    repos.value = listing.repos
    failed.value = listing.failed ?? {}
    images.value = imageList
    accounts.value = accountList
    if (!imageId.value) imageId.value = usableImages.value[0]?.id ?? ''
  } catch (e) {
    error.value = message(e)
  } finally {
    loading.value = false
  }
}

// Everything a session needs: an image, and a repository unless it was asked to
// go without one.
const ready = computed(() => !!imageId.value && (withoutRepo.value || !!selected.value))

async function submit() {
  if (!ready.value) return
  submitting.value = true
  error.value = null
  try {
    const session = await api.sessions.create(
      withoutRepo.value
        ? {
            // With no repository the provider is the token choice, so the two
            // travel together: naming an account is asking for its token.
            provider: tokenProvider.value,
            imageId: imageId.value,
            title: title.value.trim() || undefined,
            autoClaude: autoClaude.value,
            propagateToken: tokenProvider.value !== '',
            vscode: vscode.value,
            ports: parsedPorts.value,
            portAddress: portAddress.value.trim() || undefined,
          }
        : {
            provider: selected.value!.provider,
            repoFullName: selected.value!.fullName,
            branch: branch.value.trim() || undefined,
            imageId: imageId.value,
            title: title.value.trim() || undefined,
            autoClaude: autoClaude.value,
            propagateToken: propagateToken.value,
            vscode: vscode.value,
            ports: parsedPorts.value,
            portAddress: portAddress.value.trim() || undefined,
          },
    )
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

      <Notice v-if="error" kind="error" :message="error" @dismiss="error = null" />

      <p v-if="loading" class="hint">Loading your repositories…</p>

      <template v-else>
        <p v-if="!usableImages.length" class="hint">
          No image is ready yet. Build one on the <RouterLink :to="{ name: 'images' }">Images</RouterLink>
          page first.
        </p>

        <form @submit.prevent="submit">
          <div class="sources">
            <button type="button" :class="{ chosen: !withoutRepo }" @click="withoutRepo = false">
              From a repository
            </button>
            <button type="button" :class="{ chosen: withoutRepo }" @click="withoutRepo = true">
              No repository
            </button>
          </div>

          <template v-if="!withoutRepo">
            <label class="field">
              <span>
                Repository
                <button type="button" class="link" @click="load(true)">refresh</button>
              </span>
              <input v-model="filter" placeholder="Filter by name" />
            </label>

            <div v-if="sources.length > 1" class="sources">
              <button type="button" :class="{ chosen: only === '' }" @click="only = ''">All</button>
              <button
                v-for="source in sources"
                :key="source"
                type="button"
                :class="{ chosen: only === source }"
                @click="only = source"
              >
                {{ providerNames[source] }}
              </button>
            </div>

            <p v-for="(reason, source) in failed" :key="source" class="hint">
              {{ providerNames[source as ProviderKind] }} could not be reached: {{ reason }}
            </p>

            <ul class="repos">
              <li v-for="repo in matches" :key="repo.provider + '/' + repo.fullName">
                <button
                  type="button"
                  :class="{ chosen: selected?.provider === repo.provider && selected?.fullName === repo.fullName }"
                  @click="choose(repo)"
                >
                  <span class="name">{{ repo.fullName }}</span>
                  <span v-if="sources.length > 1" class="tag">{{ providerNames[repo.provider] }}</span>
                  <span v-if="repo.private" class="tag">private</span>
                  <span class="desc">{{ repo.description }}</span>
                </button>
              </li>
              <li v-if="!matches.length" class="hint">No repository matches.</li>
            </ul>
          </template>

          <p v-else class="hint">
            The session starts on an empty workspace. Choose an account to give it that account's
            token, so anything inside can clone and push with it.
          </p>

          <div class="row">
            <label v-if="!withoutRepo" class="field">
              <span>Branch</span>
              <input v-model="branch" :placeholder="selected?.defaultBranch || 'default branch'" />
            </label>

            <label v-else class="field">
              <span>Token</span>
              <select v-model="tokenProvider">
                <option value="">No token</option>
                <option
                  v-for="account in connected"
                  :key="account.provider"
                  :value="account.provider"
                >
                  {{ providerNames[account.provider] }} — {{ account.account }}
                </option>
              </select>
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
            <input
              v-model="title"
              :placeholder="withoutRepo ? imageName || 'Image name' : selected?.fullName || 'Repository name'"
            />
          </label>

          <label class="toggle">
            <input type="checkbox" v-model="autoClaude" />
            <span>
              Start Claude Code automatically
              <em>Otherwise the session opens at a shell prompt inside tmux.</em>
            </span>
          </label>

          <label v-if="!withoutRepo" class="toggle">
            <input type="checkbox" v-model="propagateToken" />
            <span>
              Pass the {{ selected ? providerNames[selected.provider] : 'account' }} token to the
              session
              <em>
                The repository is cloned either way. Without the token nothing inside the session
                can fetch or push, and this cannot be changed afterwards.
              </em>
            </span>
          </label>

          <label class="toggle">
            <input type="checkbox" v-model="vscode" />
            <span>
              VS Code in the browser
              <em>
                Adds a button on the session page that opens VS Code on the workspace. It has to
                be chosen now: the container is built for it.
              </em>
            </span>
          </label>

          <div class="row">
            <label class="field">
              <span>Published ports <em>optional</em></span>
              <input v-model="ports" placeholder="3000, 5173" />
            </label>

            <label class="field">
              <span>On address</span>
              <input v-model="portAddress" placeholder="0.0.0.0" :disabled="!parsedPorts.length" />
            </label>
          </div>
          <em class="note">
            Container ports to reach from outside the session — a dev server, a preview. Hexagon
            picks the host port and the session page shows the pair. They cannot be changed
            afterwards: the container is built with them. Use <code>127.0.0.1</code> to keep them
            on the machine running Hexagon.
          </em>
          <p v-if="portsAreExposed" class="warning">
            On <code>{{ portAddress.trim() }}</code> these ports are open to anyone who can reach
            that address, with nothing in front of them — no password, and not Hexagon's own
            sign-in. Whatever the session runs on them is public to that network for as long as
            the session is up.
          </p>

          <footer>
            <button type="button" @click="emit('close')">Cancel</button>
            <button type="submit" class="primary" :disabled="!ready || submitting">
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

.field em.note {
  font-size: 0.85rem;
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

.sources {
  display: flex;
  gap: 0.4rem;
}

.sources button {
  padding: 0.25rem 0.7rem;
  border-radius: 999px;
  font-size: 0.85rem;
}

.sources button.chosen {
  border-color: var(--accent);
  color: var(--accent);
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

/* Not .error: nothing has gone wrong. It is a consequence of a choice the user
   is in the middle of making, and it has to be as visible as one. */
.warning {
  margin: 0;
  padding: 0.6rem 0.8rem;
  border: 1px solid var(--warning);
  border-radius: 6px;
  color: var(--warning);
  font-size: 0.9rem;
}
</style>
