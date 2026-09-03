<script setup lang="ts">
// Where a signed-in user connects the accounts their repositories come from.
// GitHub is already there — it is how they signed in — and the rest are
// connected here with a token. The Claude card below is a different kind of
// account — the model sessions run as, not a source of repositories — but this
// is still where a signed-in user configures things, so it lives on the same
// page rather than a new one with a single card on it.
import { computed, onMounted, onUnmounted, ref } from 'vue'
import AppHeader from '../components/AppHeader.vue'
import Notice from '../components/Notice.vue'
import Spinner from '../components/Spinner.vue'
import TerminalPane from '../components/TerminalPane.vue'
import {
  ApiError,
  api,
  providerNames,
  type Account,
  type ClaudeAccount,
  type ClaudeAccountKind,
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

// The personal access token form, open for at most one provider at a time.
// Only GitHub offers it: its credential is the OAuth token from signing in, and
// that is the only one that expires while the user is still using it. What
// Bitbucket is connected with is already a token somebody chose.
const editingGitToken = ref<ProviderKind | null>(null)
const gitToken = ref('')

async function refresh() {
  try {
    accounts.value = await api.accounts.list()
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

function openGitToken(provider: ProviderKind) {
  editingGitToken.value = editingGitToken.value === provider ? null : provider
  gitToken.value = ''
  error.value = null
}

async function setGitToken(provider: ProviderKind) {
  busy.value = provider
  error.value = null
  try {
    await api.accounts.setGitToken(provider, gitToken.value.trim())
    editingGitToken.value = null
    gitToken.value = ''
    await refresh()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = null
  }
}

async function clearGitToken(account: Account) {
  const warning =
    'Remove the personal access token? Sessions created after this go back to the token from ' +
    'signing in, which expires.'
  if (!window.confirm(warning)) return
  busy.value = account.provider
  error.value = null
  try {
    await api.accounts.clearGitToken(account.provider)
    editingGitToken.value = null
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

// The Claude accounts sessions can run as: any number of pasted keys or
// tokens, plus a subscription signed in to through the browser, one of them
// marked default for a session that names none.
const claudeStatus = ref<ClaudeStatus | null>(null)
const claudeAccounts = ref<ClaudeAccount[]>([])
const claudeError = ref<string | null>(null)
// The id of the account a request is in flight for, 'new' for the add form, or
// null when nothing is busy.
const claudeBusy = ref<string | null>(null)

const effectiveLabel: Record<ClaudeSource, string> = {
  account: 'your default Claude account',
  apiKey: "the server's configured API key",
  file: 'the login file on this machine',
  none: 'nothing — sessions will ask to sign in',
}

const kindLabel: Record<ClaudeAccountKind, string> = {
  api_key: 'API key',
  oauth_token: 'Long-lived token',
  login: 'Subscription',
}

async function refreshClaude() {
  try {
    const [status, accounts] = await Promise.all([api.claude.status(), api.claude.accounts.list()])
    claudeStatus.value = status
    claudeAccounts.value = accounts
  } catch (e) {
    claudeError.value = message(e)
  }
}

// The form for adding a new account.
const addingAccount = ref(false)
const newName = ref('')
const newKind = ref<ClaudeAccountKind>('api_key')
const newSecret = ref('')

function openAddAccount() {
  addingAccount.value = true
  newName.value = ''
  newKind.value = 'api_key'
  newSecret.value = ''
  claudeError.value = null
}

async function createAccount() {
  claudeBusy.value = 'new'
  claudeError.value = null
  try {
    await api.claude.accounts.create({
      name: newName.value.trim(),
      kind: newKind.value,
      secret: newKind.value === 'login' ? undefined : newSecret.value.trim(),
    })
    addingAccount.value = false
    await refreshClaude()
  } catch (e) {
    claudeError.value = message(e)
  } finally {
    claudeBusy.value = null
  }
}

// Renaming one account in place.
const editingName = ref<string | null>(null)
const nameDraft = ref('')

function startRename(account: ClaudeAccount) {
  editingName.value = account.id
  nameDraft.value = account.name
  claudeError.value = null
}

async function renameAccount(account: ClaudeAccount) {
  claudeBusy.value = account.id
  claudeError.value = null
  try {
    await api.claude.accounts.update(account.id, { name: nameDraft.value.trim() })
    editingName.value = null
    await refreshClaude()
  } catch (e) {
    claudeError.value = message(e)
  } finally {
    claudeBusy.value = null
  }
}

async function makeDefault(account: ClaudeAccount) {
  claudeBusy.value = account.id
  claudeError.value = null
  try {
    await api.claude.accounts.update(account.id, { default: true })
    await refreshClaude()
  } catch (e) {
    claudeError.value = message(e)
  } finally {
    claudeBusy.value = null
  }
}

// Replacing a pasted account's secret in place.
const editingSecret = ref<string | null>(null)
const secretDraft = ref('')

async function replaceSecret(account: ClaudeAccount) {
  claudeBusy.value = account.id
  claudeError.value = null
  try {
    await api.claude.accounts.update(account.id, { secret: secretDraft.value.trim() })
    editingSecret.value = null
    secretDraft.value = ''
    await refreshClaude()
  } catch (e) {
    claudeError.value = message(e)
  } finally {
    claudeBusy.value = null
  }
}

async function removeAccount(account: ClaudeAccount) {
  if (!window.confirm(`Delete the Claude account "${account.name}"?`)) return
  claudeBusy.value = account.id
  claudeError.value = null
  try {
    await api.claude.accounts.remove(account.id)
    await refreshClaude()
  } catch (e) {
    claudeError.value = message(e)
  } finally {
    claudeBusy.value = null
  }
}

// The login dialog: a real terminal running `claude` in a throwaway container.
// null means the machine-wide login, whose $HOME/.claude is this machine's
// own; an account id means that account's own directory, so two logins never
// overwrite each other's credential.
const loginOpen = ref(false)
const loginTarget = ref<string | null>(null)
// The image the login container is built from. The empty string is not "none
// chosen": it is Hexagon's own image, which is what a machine where nothing has
// been built yet has to use — and the default, since the login needs a container
// with Claude Code in it before there is any reason to have built one.
const loginImageId = ref('')
const images = ref<Image[]>([])
const readyImages = computed(() => images.value.filter((img) => img.status === 'ready'))

// The default image is built the first time something asks for it, so a dialog
// opened on a fresh server waits for it. Polling the status is what starts that
// build as well as what follows it.
const defaultImage = computed(() => claudeStatus.value?.defaultImage)
const waitingForDefault = computed(
  () => loginImageId.value === '' && !defaultImage.value?.ready,
)
let defaultImageTimer: number | undefined

async function openLogin(accountId: string | null) {
  claudeError.value = null
  try {
    images.value = await api.images.list()
  } catch (e) {
    claudeError.value = message(e)
    return
  }
  loginImageId.value = readyImages.value[0]?.id ?? ''
  loginTarget.value = accountId
  loginOpen.value = true
  followDefaultImage()
}

// While the dialog waits on Hexagon's own image, ask again every few seconds:
// the answer changes on its own, when the build finishes.
function followDefaultImage() {
  window.clearTimeout(defaultImageTimer)
  if (!loginOpen.value || !waitingForDefault.value) return
  defaultImageTimer = window.setTimeout(async () => {
    await refreshClaude()
    followDefaultImage()
  }, 4000)
}

async function closeLogin() {
  loginOpen.value = false
  window.clearTimeout(defaultImageTimer)
  try {
    if (loginTarget.value) await api.claude.accounts.stopLogin(loginTarget.value)
    else await api.claude.stopLogin()
  } catch (e) {
    claudeError.value = message(e)
  }
  loginTarget.value = null
  await refreshClaude()
}

function formatDate(iso: string): string {
  return new Date(iso).toLocaleString()
}

onMounted(() => {
  refresh()
  refreshClaude()
})

onUnmounted(() => window.clearTimeout(defaultImageTimer))
</script>

<template>
  <AppHeader />

  <main class="shell">
    <h1>Accounts</h1>
    <p class="intro">
      Where your repositories come from. Sessions clone with the account's own credentials, and
      the container gets them so Claude Code can push.
    </p>

    <Notice v-if="error" kind="error" :message="error" class="alert" @dismiss="error = null" />

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
            <div v-if="account.connected && account.provider === 'github'" class="muted">
              <p v-if="!account.gitTokenSet" class="notice error" role="alert">
                <span>git is using the sign-in token, which requires a logout/login when expires</span>
              </p>
            </div>
          </div>

          <div class="actions">
            <template v-if="account.connected && account.provider === 'github'">
              <button type="button" @click="openGitToken(account.provider)">
                {{ account.gitTokenSet ? 'Replace token' : 'Set token' }}
              </button>
              <button
                v-if="account.gitTokenSet"
                type="button"
                class="danger"
                :disabled="busy === account.provider"
                @click="clearGitToken(account)"
              >
                <Spinner v-if="busy === account.provider" />Remove token
              </button>
            </template>
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

        <form
          v-if="editingGitToken === account.provider"
          class="connect"
          @submit.prevent="setGitToken(account.provider)"
        >
          <label class="field">
            <span>Personal access token</span>
            <input v-model="gitToken" required type="password" autocomplete="off" />
          </label>

          <p class="hint">
            The token sessions fetch and push with. Create one under GitHub Settings → Developer
            settings → Personal access tokens, with the <code>repo</code> scope for a classic
            token, or read and write access to Contents for a fine-grained one. It is used for git
            only — your repositories are still listed with the account you signed in with, so its
            scopes can be as narrow as you like. Sessions that already exist keep the token their
            container was built with.
          </p>

          <div class="buttons">
            <button type="button" @click="editingGitToken = null">Cancel</button>
            <button type="submit" class="primary" :disabled="busy === account.provider">
              <Spinner v-if="busy === account.provider" />Save
            </button>
          </div>
        </form>
      </li>
    </ul>

    <p v-if="!accounts.length && loaded" class="hint">No providers are configured.</p>

    <h1 class="claude-heading">Claude</h1>
    <p class="intro">
      The accounts sessions can run Claude Code as. Paste a key or token, or add one that signs
      in with a subscription in its own browser terminal. The one marked default is what a
      session gets when it names none.
    </p>

    <Notice v-if="claudeError" kind="error" :message="claudeError" class="alert" @dismiss="claudeError = null" />

    <div v-if="claudeStatus" class="card">
      <p class="effective">
        A session naming no account authenticates with
        <strong>{{ effectiveLabel[claudeStatus.effective] }}</strong>.
      </p>

      <ul class="list claude-list">
        <li v-for="account in claudeAccounts" :key="account.id">
          <div class="row">
            <div class="identity">
              <input
                v-if="editingName === account.id"
                v-model="nameDraft"
                class="name-input"
                @keydown.esc="editingName = null"
              />
              <strong v-else>{{ account.name }}</strong>
              <span class="muted">
                {{ kindLabel[account.kind] }}<span v-if="account.default"> · default</span>
              </span>
              <span v-if="account.kind === 'login'" class="who">
                {{
                  account.login?.present
                    ? `Signed in ${formatDate(account.login.updatedAt!)}`
                    : 'Not signed in yet'
                }}
              </span>
            </div>

            <div class="actions">
              <template v-if="editingName === account.id">
                <button type="button" @click="editingName = null">Cancel</button>
                <button
                  type="button"
                  class="primary"
                  :disabled="claudeBusy === account.id"
                  @click="renameAccount(account)"
                >
                  <Spinner v-if="claudeBusy === account.id" />Save
                </button>
              </template>
              <template v-else>
                <button type="button" @click="startRename(account)">Rename</button>
                <button
                  v-if="!account.default"
                  type="button"
                  :disabled="claudeBusy === account.id"
                  @click="makeDefault(account)"
                >
                  Make default
                </button>
                <button
                  v-if="account.kind === 'login'"
                  type="button"
                  @click="openLogin(account.id)"
                >
                  {{ account.login?.present ? 'Log in again' : 'Log in' }}
                </button>
                <button
                  v-else
                  type="button"
                  @click="editingSecret = editingSecret === account.id ? null : account.id"
                >
                  Replace secret
                </button>
                <button
                  type="button"
                  class="danger"
                  :disabled="claudeBusy === account.id"
                  @click="removeAccount(account)"
                >
                  <Spinner v-if="claudeBusy === account.id" />Delete
                </button>
              </template>
            </div>
          </div>

          <form v-if="editingSecret === account.id" class="connect" @submit.prevent="replaceSecret(account)">
            <label class="field">
              <span>{{ account.kind === 'api_key' ? 'API key' : 'Token' }}</span>
              <input v-model="secretDraft" required type="password" autocomplete="off" />
            </label>
            <div class="buttons">
              <button type="button" @click="editingSecret = null">Cancel</button>
              <button type="submit" class="primary" :disabled="claudeBusy === account.id">
                <Spinner v-if="claudeBusy === account.id" />Save
              </button>
            </div>
          </form>
        </li>
      </ul>
      <p v-if="!claudeAccounts.length" class="hint">No Claude accounts configured yet.</p>

      <form v-if="addingAccount" class="connect" @submit.prevent="createAccount">
        <label class="field">
          <span>Name</span>
          <input v-model="newName" required placeholder="Personal" autocomplete="off" />
        </label>

        <label class="field">
          <span>Kind</span>
          <select v-model="newKind">
            <option value="api_key">API key</option>
            <option value="oauth_token">Long-lived token</option>
            <option value="login">Subscription (browser login)</option>
          </select>
        </label>

        <label v-if="newKind !== 'login'" class="field">
          <span>{{ newKind === 'api_key' ? 'API key' : 'Token' }}</span>
          <input v-model="newSecret" required type="password" autocomplete="off" />
        </label>

        <p class="hint">
          An API key comes from the Anthropic Console. A long-lived token comes from running
          <code>claude setup-token</code> on a machine with an active subscription. A subscription
          account takes no secret here — press <strong>Log in</strong> on it afterwards.
          <span v-if="newKind !== 'login' && !claudeStatus.canVerify">
            This server has no way to run Claude Code, so the credential is stored without being
            checked first.
          </span>
          <span v-else-if="newKind !== 'login' && claudeStatus.inContainer">
            Claude Code is not installed on this server, so checking the credential runs it in a
            container: it works, it just takes longer.
          </span>
        </p>

        <div class="buttons">
          <button type="button" @click="addingAccount = false">Cancel</button>
          <button type="submit" class="primary" :disabled="claudeBusy === 'new'">
            <Spinner v-if="claudeBusy === 'new'" />Add account
          </button>
        </div>
      </form>
      <button v-else type="button" @click="openAddAccount">Add a Claude account</button>

      <div class="login">
        <button v-if="claudeStatus.canLogin" type="button" @click="openLogin(null)">
          Log in on this machine
        </button>
        <p v-else class="hint">
          No credentials path is configured on the server, so the machine-wide login has nowhere
          to write.
        </p>
        <p v-if="claudeStatus.canLogin" class="hint">
          This is the login of the machine itself — what a session naming no account falls back
          to. Most of the time an account above, made the default, is the better place to sign in.
        </p>
      </div>
    </div>

    <div v-if="loginOpen" class="overlay" @click.self="closeLogin">
      <div class="dialog">
        <h2>Log in to Claude Code</h2>

        <label class="field">
          <span>Image</span>
          <select v-model="loginImageId" @change="followDefaultImage">
            <option value="">Hexagon's own image</option>
            <option v-for="img in readyImages" :key="img.id" :value="img.id">{{ img.name }}</option>
          </select>
        </label>
        <p v-if="!readyImages.length" class="hint">
          You have built no image yet, so this runs in the one Hexagon keeps for itself — the
          same reference image the Images page starts from.
        </p>

        <p v-if="waitingForDefault && defaultImage?.error" class="error">
          That image could not be built: {{ defaultImage.error }}
        </p>
        <p v-else-if="waitingForDefault" class="hint">
          <Spinner />Building that image. It happens once, and takes a few minutes.
        </p>

        <TerminalPane
          v-else
          :key="loginImageId + (loginTarget ?? '')"
          :url="loginTarget ? api.claude.accounts.loginTerminal(loginTarget, loginImageId) : api.claude.loginTerminal(loginImageId)"
          class="terminal"
        />

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
  flex-wrap: wrap;
  row-gap: 0.5rem;
  gap: 1rem;
}

.identity {
  display: grid;
  gap: 0.15rem;
  min-width: 0;
}

.who {
  font-size: 0.9rem;
  color: var(--text-muted);
}

.name-input {
  padding: 0.2rem 0.4rem;
  border: 1px solid var(--border);
  border-radius: 4px;
  background: var(--bg);
  color: var(--text);
  font: inherit;
  font-weight: 600;
}

.muted {
  color: var(--text-muted);
  font-size: 0.9rem;
}

.actions {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  row-gap: 0.4rem;
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

/* Scoped styles reach a child component's root element, which is how this page
   spaces a notice its own layout stacks with margins. */
.alert {
  margin: 0 0 1rem;
}

.error {
  margin: 0 0 1rem;
  padding: 0.6rem 0.8rem;
  border: 1px solid var(--error);
  border-radius: 6px;
  color: var(--error);
}
</style>
