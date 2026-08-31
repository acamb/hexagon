<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ApiError, api, type SetupStatus } from '../api'

const router = useRouter()

// The wizard is three screens: the password, the settings, and what the server
// ended up running on. The password is checked by the save rather than by a
// step of its own — an endpoint that only answers "is this password right"
// would reveal exactly what the save already reveals — so a wrong one comes
// back here with every field still filled in.
type Step = 'password' | 'settings' | 'done'

const step = ref<Step>('password')
const status = ref<SetupStatus | null>(null)
const loading = ref(true)
const saving = ref(false)
const error = ref('')

const password = ref('')
const clientId = ref('')
const clientSecret = ref('')
const allowedUsers = ref('')

const shadowed = computed(() => new Set(status.value?.fromEnvironment ?? []))

onMounted(async () => {
  try {
    const current = await api.setup.status()
    if (!current.required) {
      // Somebody has signed in: there is nothing here any more, and the
      // settings live behind a session from now on.
      router.replace('/login')
      return
    }
    status.value = current
    clientId.value = current.clientId ?? ''
    allowedUsers.value = (current.allowedUsers ?? []).join('\n')
  } catch (e) {
    error.value = message(e)
  } finally {
    loading.value = false
  }
})

function toSettings() {
  if (!password.value.trim()) {
    error.value = 'Enter the password from the server log.'
    return
  }
  error.value = ''
  step.value = 'settings'
}

async function save() {
  saving.value = true
  error.value = ''
  try {
    status.value = await api.setup.save({
      password: password.value,
      clientId: clientId.value,
      clientSecret: clientSecret.value,
      allowedUsers: allowedUsers.value.split('\n'),
    })
    step.value = 'done'
  } catch (e) {
    error.value = message(e)
    // The password is only wrong on the way to the server, so this is the one
    // failure that belongs on the first screen.
    if (e instanceof ApiError && e.unauthorized) {
      password.value = ''
      step.value = 'password'
    }
  } finally {
    saving.value = false
  }
}

function message(e: unknown): string {
  if (e instanceof ApiError) return e.message
  return e instanceof Error ? e.message : String(e)
}
</script>

<template>
  <main class="setup">
    <h1>
      <picture>
        <source srcset="/hexagon-logo-dark.png" media="(prefers-color-scheme: dark)" />
        <img src="/hexagon-logo.png" alt="Hexagon" width="420" height="443" />
      </picture>
    </h1>
    <p class="tagline">First-time setup.</p>

    <p v-if="loading" class="muted">Loading…</p>
    <p v-if="error" class="error">{{ error }}</p>

    <template v-if="!loading && status">
      <p v-if="status.writable === false" class="warning">
        <strong>{{ status.configPath }}</strong> cannot be written by the server. Fix its
        permissions, or configure Hexagon by hand and restart it.
      </p>

      <form v-if="step === 'password'" @submit.prevent="toSettings">
        <p class="lead">
          The server printed a password when it started. It is in the log, beside
          <code>first-time setup is open</code>, and a restart replaces it.
        </p>
        <label>
          Setup password
          <input v-model="password" type="password" autocomplete="off" autofocus />
        </label>
        <button type="submit">Continue</button>
      </form>

      <form v-else-if="step === 'settings'" @submit.prevent="save">
        <p class="lead">
          Register an OAuth app at
          <a href="https://github.com/settings/developers" target="_blank" rel="noreferrer">
            github.com/settings/developers</a>, with this exact callback URL:
        </p>
        <p class="callback"><code>{{ status.callbackUrl }}</code></p>

        <label>
          Client ID
          <input v-model="clientId" type="text" autocomplete="off" autofocus />
          <span v-if="shadowed.has('clientId')" class="note">
            Set by HEXAGON_GITHUB_CLIENT_ID, which wins over what is saved here.
          </span>
        </label>

        <label>
          Client secret
          <input v-model="clientSecret" type="password" autocomplete="off" />
          <span v-if="shadowed.has('clientSecret')" class="note">
            Set by HEXAGON_GITHUB_CLIENT_SECRET, which wins over what is saved here.
          </span>
        </label>

        <label>
          Who may sign in, one per line
          <textarea v-model="allowedUsers" rows="4" spellcheck="false"></textarea>
          <span class="note">
            An account id or a login. Prefer ids: a login is released when an account is
            renamed, and can then be claimed by somebody else.
          </span>
          <span v-if="shadowed.has('allowedUsers')" class="note">
            Set by HEXAGON_ALLOWED_USERS, which wins over what is saved here.
          </span>
        </label>

        <p class="note">Saved to <code>{{ status.configPath }}</code>.</p>
        <button type="submit" :disabled="saving">{{ saving ? 'Saving…' : 'Save and continue' }}</button>
      </form>

      <div v-else class="done">
        <p class="lead">Saved. Hexagon is now signing people in with:</p>
        <dl>
          <dt>Client ID</dt>
          <dd><code>{{ status.clientId }}</code></dd>
          <dt>Allowed to sign in</dt>
          <dd><code>{{ (status.allowedUsers ?? []).join(', ') }}</code></dd>
        </dl>
        <p class="note">
          This page stays available until somebody signs in, so a wrong value can still be
          corrected here.
        </p>
        <RouterLink class="signin" to="/login">Go to sign-in</RouterLink>
      </div>
    </template>
  </main>
</template>

<style scoped>
.setup {
  max-width: 32rem;
  margin: 10vh auto 0;
  padding: 0 1.5rem 4rem;
}

h1 {
  margin: 0 0 1.5rem;
}

/* The wordmark is part of the artwork, so the heading is the logo and nothing
   else. Half the intrinsic width: the file is sized for a high-density screen. */
h1 img {
  width: 210px;
  height: auto;
}

h1 picture {
  display: block;
  text-align: center;
}

.tagline {
  margin: 0.25rem 0 2rem;
  color: var(--text-muted);
  text-align: center;
}

.lead {
  margin: 0 0 1.25rem;
}

.callback {
  margin: 0 0 1.5rem;
  padding: 0.6rem 0.75rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: var(--surface);
  overflow-wrap: anywhere;
}

label {
  display: block;
  margin-bottom: 1.25rem;
  font-weight: 600;
}

input,
textarea {
  display: block;
  width: 100%;
  margin-top: 0.35rem;
  padding: 0.6rem 0.75rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: var(--bg);
  color: var(--text);
  font: inherit;
}

textarea {
  font-family: var(--mono);
  resize: vertical;
}

.note {
  display: block;
  margin-top: 0.35rem;
  color: var(--text-muted);
  font-size: 0.85rem;
  font-weight: 400;
}

.muted {
  color: var(--text-muted);
}

.error,
.warning {
  margin-bottom: 1.5rem;
  padding: 0.75rem 1rem;
  border: 1px solid var(--error);
  border-radius: 6px;
  color: var(--error);
}

.warning {
  border-color: var(--border);
  background: var(--surface);
  color: var(--text);
}

dl {
  margin: 0 0 1.25rem;
}

dt {
  color: var(--text-muted);
  font-size: 0.85rem;
}

dd {
  margin: 0.15rem 0 0.75rem;
  overflow-wrap: anywhere;
}

button,
.signin {
  display: block;
  width: 100%;
  padding: 0.75rem 1rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: var(--surface);
  color: var(--text);
  font: inherit;
  font-weight: 600;
  text-align: center;
  text-decoration: none;
  cursor: pointer;
}

button:hover,
.signin:hover {
  border-color: var(--accent);
}

button:disabled {
  cursor: default;
  opacity: 0.6;
}
</style>
