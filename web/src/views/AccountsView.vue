<script setup lang="ts">
// Where a signed-in user connects the accounts their repositories come from.
// GitHub is already there — it is how they signed in — and the rest are
// connected here with a token.
import { onMounted, ref } from 'vue'
import AppHeader from '../components/AppHeader.vue'
import Spinner from '../components/Spinner.vue'
import { ApiError, api, providerNames, type Account, type ProviderKind } from '../api'

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

onMounted(refresh)
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

input {
  padding: 0.5rem 0.6rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: var(--bg);
  color: var(--text);
  font: inherit;
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
