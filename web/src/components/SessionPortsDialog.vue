<script setup lang="ts">
// Published ports are a property of the container, so changing them means the
// session gets a new one. That is why this dialog exists at all rather than a
// field on the session page: it is a rebuild, it takes a moment, and what it
// costs is worth saying before it happens.
import { computed, onMounted, onUnmounted, ref } from 'vue'
import Spinner from './Spinner.vue'
import Notice from './Notice.vue'
import { ApiError, api, type Session } from '../api'

const props = defineProps<{ session: Session }>()
const emit = defineEmits<{ close: []; updated: [session: Session] }>()

// Seeded from the session, so the dialog opens on what it already publishes and
// the usual edit is one number away.
const ports = ref(props.session.ports.map((p) => p.container).join(', '))
const address = ref(props.session.portAddress)
const error = ref<string | null>(null)
const saving = ref(false)

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

// Whether the chosen address reaches beyond this machine, which is what the
// warning is about. Anything that is not a loopback address does.
const exposed = computed(() => {
  const value = address.value.trim()
  return (
    parsedPorts.value.length > 0 &&
    value !== '' &&
    value !== '127.0.0.1' &&
    value !== 'localhost' &&
    value !== '::1'
  )
})

async function save() {
  saving.value = true
  error.value = null
  try {
    emit('updated', await api.sessions.setPorts(props.session.id, {
      ports: parsedPorts.value,
      portAddress: address.value.trim(),
    }))
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : e instanceof Error ? e.message : String(e)
  } finally {
    saving.value = false
  }
}

function onKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape' && !saving.value) emit('close')
}

onMounted(() => window.addEventListener('keydown', onKeydown))
onUnmounted(() => window.removeEventListener('keydown', onKeydown))
</script>

<template>
  <div class="backdrop" @click.self="!saving && emit('close')">
    <section class="dialog">
      <header>
        <h2>Published ports</h2>
        <button type="button" class="icon" :disabled="saving" @click="emit('close')" aria-label="Close">
          ×
        </button>
      </header>

      <form @submit.prevent="save">
        <div class="row">
          <label class="field">
            <span>Container ports <em>empty publishes nothing</em></span>
            <input v-model="ports" placeholder="3000, 5173" :disabled="saving" />
          </label>

          <label class="field">
            <span>On address</span>
            <input
              v-model="address"
              placeholder="0.0.0.0"
              :disabled="saving || !parsedPorts.length"
            />
          </label>
        </div>

        <em class="note">
          Hexagon picks the host port and shows the pair once the session is up. Use
          <code>127.0.0.1</code> to keep these ports on the machine running Hexagon.
        </em>

        <p class="caution">
          Saving rebuilds this session's container from its image. The workspace and everything
          under the session's home directory are kept — they live on this machine — but anything
          installed inside the old container by hand is not.
        </p>

        <p v-if="exposed" class="warning">
          On <code>{{ address.trim() }}</code> these ports are open to anyone who can reach that
          address, with nothing in front of them — no password, and not Hexagon's own sign-in.
          Whatever the session runs on them is public to that network for as long as the session
          is up.
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
  width: min(34rem, 100%);
}

form {
  display: grid;
  gap: 0.9rem;
}

.row {
  display: flex;
  gap: 1rem;
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

.field em,
.note {
  color: var(--text-muted);
  font-weight: 400;
  font-style: normal;
}

.note {
  font-size: 0.85rem;
}

input {
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

/* What saving costs, said before it is spent: it is a consequence of the
   choice, not a failure, so it reads like the exposure warning below rather
   than like an error. */
.caution,
.warning {
  margin: 0;
  padding: 0.6rem 0.8rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  color: var(--text-muted);
  font-size: 0.9rem;
}

.warning {
  border-color: var(--warning);
  color: var(--warning);
}

.error {
  margin: 0;
  padding: 0.6rem 0.8rem;
  border: 1px solid var(--error);
  border-radius: 6px;
  color: var(--error);
}
</style>
