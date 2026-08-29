<script setup lang="ts">
// Where a signed-in user connects the accounts their repositories come from.
// GitHub is already there — it is how they signed in — and the rest are
// connected here with a token. The Claude card below is a different kind of
// account — the model sessions run as, not a source of repositories — but this
// is still where a signed-in user configures things, so it lives on the same
// page rather than a new one with a single card on it.
import { computed, onMounted, ref } from 'vue'
import AppHeader from '../components/AppHeader.vue'
import Spinner from '../components/Spinner.vue'
import TerminalPane from '../components/TerminalPane.vue'
import {
  ApiError,
  api,
  providerNames,
  type Account,
  type ClaudeCredentialKind,
  type ClaudeStatus,
  type ClaudeSource,
  type Image,
  type ProviderKind,
} from '../api'

const accounts = ref<Account[]>([])
const error = ref<string | null>(null)
const loaded = ref(false)

// The form for the account being connected, if any.
const connecting = ref<ProviderKind | null>(null)
const identity = ref('')
const secret = ref('')
const busy = ref<ProviderKind | null>(null)

async function refresh() {
  try {
    accounts.value = await api.accounts.list()
    error.value = null
  } catch (e) {
    error.value = message(e)
  } finally {
    loaded.value = true
  }
}

function open(provider: ProviderKind) {
  connecting.value = provider
  identity.value = ''
  secret.value = ''
  error.value = null
}

async function connect(provider: ProviderKind) {
  busy.value = provider
  error.value = null
  try {
    await api.accounts.connect(provider, identity.value.trim(), secret.value.trim())
    connecting.value = null
    secret.value = ''
    await refresh()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = null
  }
}

async function disconnect(account: Account) {
  if (!window.confirm(`Disconnect ${providerNames[account.provider]}?`)) return
  busy.value = account.provider
  try {
    await api.accounts.disconnect(account.provider)
    await refresh()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = null
  }
}

function message(e: unknown): string {
  if (e instanceof ApiError) return e.message
  return e instanceof Error ? e.message : String(e)
}

// The Claude login: a pasted credential, or a browser login that writes the
// host's own Claude Code file.
const claudeStatus = ref<ClaudeStatus | null>(null)
const claudeError = ref<string | null>(null)
const claudeBusy = ref(false)
const claudeKind = ref<ClaudeCredentialKind>('api_key')
const claudeSecret = ref('')

const effectiveLabel: Record<ClaudeSource, string> = {
  credential: 'the credential stored here',
  apiKey: "the server's configured API key",
  file: 'the login file on this machine',
  none: 'nothing — sessions will ask to sign in',
}

async function refreshClaude() {
  try {
    claudeStatus.value = await api.claude.status()
    claudeError.value = null
  } catch (e) {
    claudeError.value = message(e)
  }
}

async function setClaudeCredential() {
  claudeBusy.value = true
  claudeError.value = null
  try {
    claudeStatus.value = await api.claude.setCredential(claudeKind.value, claudeSecret.value.trim())
    claudeSecret.value = ''
  } catch (e) {
    claudeError.value = message(e)
  } finally {
    claudeBusy.value = false
  }
}

async function forgetClaudeCredential() {
  if (!window.confirm('Forget the stored Claude credential?')) return
  claudeBusy.value = true
  claudeError.value = null
  try {
    await api.claude.forgetCredential()
    await refreshClaude()
  } catch (e) {
    claudeError.value = message(e)
  } finally {
    claudeBusy.value = false
  }
}

// The login dialog: a real terminal running `claude` in a throwaway container
// whose $HOME/.claude is this machine's own, so /login writes the file every
// session already mounts.
const loginOpen = ref(false)
const loginImageId = ref('')
const images = ref<Image[]>([])
const readyImages = computed(() => images.value.filter((img) => img.status === 'ready'))

async function openLogin() {
  claudeError.value = null
  try {
    images.value = await api.images.list()
  } catch (e) {
    claudeError.value = message(e)
    return
  }
  loginImageId.value = readyImages.value[0]?.id ?? ''
  loginOpen.value = true
}

async function closeLogin() {
  loginOpen.value = false
  try {
    await api.claude.stopLogin()
  } catch (e) {
    claudeError.value = message(e)
  }
  await refreshClaude()
}

function formatDate(iso: string): string {
  return new Date(iso).toLocaleString()
}

onMounted(() => {
  refresh()
  refreshClaude()
})
</script>

<template>
  <AppHeader />

  <main class="shell">
    <h1>Accounts</h1>
    <p class="intro">
      Where your repositories come from. Sessions clone with the account's own credentials, and
      the container gets them so Claude Code can push.
    </p>

    <p v-if="error" class="error">{{ error }}</p>

    <ul class="list">
      <li v-for="account in accounts" :key="account.provider">
        <div class="row">
          <div class="identity">
            <strong>{{ providerNames[account.provider] }}</strong>
            <span v-if="account.connected" class="who">
              {{ account.account }}
              <span v-if="account.identity && account.identity !== account.account" class="muted">
                · {{ account.identity }}
              </span>
            </span>
            <span v-else class="muted">Not connected</span>
          </div>

          <div class="actions">
            <span v-if="!account.removable" class="muted">signed in with</span>
            <button
              v-else-if="account.connected"
              type="button"
              class="danger"
              :disabled="busy === account.provider"
              @click="disconnect(account)"
            >
              <Spinner v-if="busy === account.provider" />Disconnect
            </button>
            <button
              v-else-if="connecting !== account.provider"
              type="button"
              @click="open(account.provider)"
            >
              Connect
            </button>
          </div>
        </div>

        <form
          v-if="connecting === account.provider"
          class="connect"
          @submit.prevent="connect(account.provider)"
        >
          <label class="field">
            <span>Atlassian account email</span>
            <input v-model="identity" required placeholder="you@example.com" autocomplete="off" />
          </label>

          <label class="field">
            <span>API token</span>
            <input v-model="secret" required type="password" autocomplete="off" />
          </label>

          <p class="hint">
            Create one under Atlassian account settings → Security → API tokens, with
            <code>read:workspace:bitbucket</code> and <code>read:repository:bitbucket</code>, plus
            <code>write:repository:bitbucket</code> if Claude Code should push. Both reads are
            needed: repositories can only be listed one workspace at a time.
            <code>read:user:bitbucket</code> is optional — with it the account shows its username,
            without it the email. App passwords no longer work: Atlassian removed them in July 2026.
          </p>

          <div class="buttons">
            <button type="button" @click="connecting = null">Cancel</button>
            <button type="submit" class="primary" :disabled="busy === account.provider">
              <Spinner v-if="busy === account.provider" />Connect
            </button>
          </div>
        </form>
      </li>
    </ul>

    <p v-if="!accounts.length && loaded" class="hint">No providers are configured.</p>

    <h1 class="claude-heading">Claude</h1>
    <p class="intro">
      The account sessions run Claude Code as. Paste a key or token, or sign in with a
      subscription in a terminal that writes this machine's own Claude Code login.
    </p>

    <p v-if="claudeError" class="error">{{ claudeError }}</p>

    <div v-if="claudeStatus" class="card">
      <p class="effective">
        Sessions currently authenticate with <strong>{{ effectiveLabel[claudeStatus.effective] }}</strong>.
      </p>
      <p v-if="claudeStatus.effective === 'credential' && claudeStatus.file.present" class="hint">
        A login file is also present on this machine, but the stored credential takes precedence —
        Claude Code prefers it to the file, and there is no way to change that from here.
      </p>

      <div v-if="claudeStatus.credential" class="row">
        <div class="identity">
          <strong>{{ claudeStatus.credential.kind === 'api_key' ? 'API key' : 'Long-lived token' }}</strong>
          <span class="muted">updated {{ formatDate(claudeStatus.credential.updatedAt) }}</span>
        </div>
        <button type="button" class="danger" :disabled="claudeBusy" @click="forgetClaudeCredential">
          <Spinner v-if="claudeBusy" />Forget
        </button>
      </div>

      <form v-else class="connect" @submit.prevent="setClaudeCredential">
        <label class="field">
          <span>Kind</span>
          <select v-model="claudeKind">
            <option value="api_key">API key</option>
            <option value="oauth_token">Long-lived token</option>
          </select>
        </label>

        <label class="field">
          <span>{{ claudeKind === 'api_key' ? 'API key' : 'Token' }}</span>
          <input v-model="claudeSecret" required type="password" autocomplete="off" />
        </label>

        <p class="hint">
          An API key comes from the Anthropic Console. A long-lived token comes from running
          <code>claude setup-token</code> on a machine with an active subscription.
          <span v-if="!claudeStatus.canVerify">
            This server has no Claude Code binary, so the credential is stored without being
            checked first.
          </span>
        </p>

        <div class="buttons">
          <button type="submit" class="primary" :disabled="claudeBusy">
            <Spinner v-if="claudeBusy" />Save
          </button>
        </div>
      </form>

      <div class="login">
        <button v-if="claudeStatus.canLogin" type="button" @click="openLogin">
          Log in with a subscription
        </button>
        <p v-else class="hint">
          No credentials path is configured on the server, so a browser login has nowhere to
          write.
        </p>
        <p v-if="claudeStatus.canLogin" class="hint">
          Signing in here writes this machine's own Claude Code login. A session already running
          keeps whatever it started with; a session picks this up the next time it starts.
        </p>
      </div>
    </div>

    <div v-if="loginOpen" class="overlay" @click.self="closeLogin">
      <div class="dialog">
        <h2>Log in to Claude Code</h2>

        <label class="field">
          <span>Image</span>
          <select v-model="loginImageId">
            <option v-for="img in readyImages" :key="img.id" :value="img.id">{{ img.name }}</option>
          </select>
        </label>
        <p v-if="!readyImages.length" class="hint">No image is ready to run the login in.</p>

        <TerminalPane v-if="loginImageId" :key="loginImageId" :url="api.claude.loginTerminal(loginImageId)" class="terminal" />

        <div class="buttons">
          <button type="button" class="primary" @click="closeLogin">Done</button>
        </div>
      </div>
    </div>
  </main>
</template>

<style scoped>
.shell {
  max-width: 46rem;
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
  justify-content: space-between;
  gap: 1rem;
}

.identity {
  display: grid;
  gap: 0.15rem;
  min-width: 0;
}

.who {
  font-size: 0.9rem;
}

.muted {
  color: var(--text-muted);
  font-size: 0.9rem;
}

.actions {
  display: flex;
  align-items: center;
  gap: 0.5rem;
}

.connect {
  display: grid;
  gap: 0.75rem;
  margin-top: 1rem;
  padding-top: 1rem;
  border-top: 1px solid var(--border);
}

.field {
  display: grid;
  gap: 0.35rem;
}

.field > span {
  font-weight: 600;
  font-size: 0.9rem;
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

.claude-heading {
  margin-top: 2.5rem;
}

.card {
  display: grid;
  gap: 1rem;
  padding: 0.9rem 1rem;
  border: 1px solid var(--border);
  border-radius: 8px;
}

.effective {
  margin: 0;
}

.login {
  display: grid;
  gap: 0.5rem;
  padding-top: 1rem;
  border-top: 1px solid var(--border);
}

.overlay {
  position: fixed;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  background: rgba(0, 0, 0, 0.5);
  z-index: 10;
}

.dialog {
  display: grid;
  gap: 0.75rem;
  width: min(40rem, 92vw);
  max-height: 85vh;
  padding: 1.25rem;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--surface);
}

.dialog h2 {
  margin: 0;
}

.dialog .terminal {
  height: 22rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  overflow: hidden;
}

.buttons {
  display: flex;
  justify-content: flex-end;
  gap: 0.6rem;
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
  opacity: 0.5;
  cursor: default;
}

.primary {
  font-weight: 600;
}

.danger:hover {
  border-color: var(--error);
  color: var(--error);
}

.hint {
  margin: 0;
  color: var(--text-muted);
  font-size: 0.9rem;
}

.error {
  margin: 0 0 1rem;
  padding: 0.6rem 0.8rem;
  border: 1px solid var(--error);
  border-radius: 6px;
  color: var(--error);
}
</style>
