<script setup lang="ts">
// Which Claude account a session's container authenticates with is fixed when
// the container is built, exactly like its published ports — so changing it
// means the session gets a new one, which is why this is a dialog rather than
// a field on the session page.
import { onMounted, onUnmounted, ref } from 'vue'
import Spinner from './Spinner.vue'
import Notice from './Notice.vue'
import { ApiError, api, type ClaudeAccount, type Session } from '../api'

const props = defineProps<{ session: Session }>()
const emit = defineEmits<{ close: []; updated: [session: Session] }>()

const accounts = ref<ClaudeAccount[]>([])
// Seeded from the session: empty means "resolve automatically", which is also
// what a session that has never named one already does.
const claudeAccountId = ref(props.session.claudeAccountId)
const loading = ref(true)
const error = ref<string | null>(null)
const saving = ref(false)

async function load() {
  loading.value = true
  try {
    accounts.value = await api.claude.accounts.list()
  } catch (e) {
    error.value = message(e)
  } finally {
    loading.value = false
  }
}

async function save() {
  saving.value = true
  error.value = null
  try {
    emit(
      'updated',
      await api.sessions.setClaudeAccount(props.session.id, { claudeAccountId: claudeAccountId.value }),
    )
  } catch (e) {
    error.value = message(e)
  } finally {
    saving.value = false
  }
}

function message(e: unknown): string {
  return e instanceof ApiError ? e.message : e instanceof Error ? e.message : String(e)
}

function onKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape' && !saving.value) emit('close')
}

onMounted(() => {
  load()
  window.addEventListener('keydown', onKeydown)
})
onUnmounted(() => window.removeEventListener('keydown', onKeydown))
</script>

<template>
  <div class="backdrop" @click.self="!saving && emit('close')">
    <section class="dialog">
      <header>
        <h2>Claude account</h2>
        <button type="button" class="icon" :disabled="saving" @click="emit('close')" aria-label="Close">
          ×
        </button>
      </header>

      <p v-if="loading" class="hint">Loading your Claude accounts…</p>

      <form v-else @submit.prevent="save">
        <label class="field">
          <span>Account</span>
          <select v-model="claudeAccountId" :disabled="saving">
            <option value="">Resolve automatically — your default account</option>
            <option v-for="account in accounts" :key="account.id" :value="account.id">
              {{ account.name }}{{ account.default ? ' (default)' : '' }}
            </option>
          </select>
        </label>
        <p v-if="!accounts.length" class="hint">
          No Claude accounts are configured yet — add one on the
          <RouterLink :to="{ name: 'accounts' }">Accounts</RouterLink> page.
        </p>

        <p class="caution">
          Saving rebuilds this session's container from its image. The workspace and everything
          under the session's home directory are kept — they live on this machine — but anything
          installed inside the old container by hand is not.
        </p>
        <p class="caution">
          The rebuilt container carries whatever this account holds right now, so one that has
          been signed out of since fails the rebuild instead of quietly producing a container that
          cannot authenticate.
        </p>

        <Notice v-if="error" kind="error" :message="error" @dismiss="error = null" />

        <footer>
          <button type="button" :disabled="saving" @click="emit('close')">Cancel</button>
          <button type="submit" class="primary" :disabled="saving">
            <Spinner v-if="saving" />{{ saving ? 'Rebuilding…' : 'Save and rebuild' }}
          </button>
        </footer>
      </form>
    </section>
  </div>
</template>

<style scoped>
/* The backdrop and the panel itself come from style.css, shared with the other
   dialogs; only this one's width is its own. */
.dialog {
  width: min(30rem, 100%);
}

form {
  display: grid;
  gap: 0.9rem;
}

.field {
  display: grid;
  gap: 0.35rem;
}

.field > span {
  font-weight: 600;
  font-size: 0.9rem;
}

select {
  padding: 0.5rem 0.6rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: var(--bg);
  color: var(--text);
  font: inherit;
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

.hint {
  margin: 0;
  color: var(--text-muted);
  font-size: 0.9rem;
}

/* What saving costs, said before it is spent: it is a consequence of the
   choice, not a failure. */
.caution {
  margin: 0;
  padding: 0.6rem 0.8rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  color: var(--text-muted);
  font-size: 0.9rem;
}

.error {
  margin: 0;
  padding: 0.6rem 0.8rem;
  border: 1px solid var(--error);
  border-radius: 6px;
  color: var(--error);
}
</style>
