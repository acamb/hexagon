<script setup lang="ts">
// The configuration file, edited from the browser. Three things about a setting
// matter here and none of them is its value alone: what this process is running
// on, what the file now says, and whether the two have drifted apart. Most of
// these are read once when the server starts, so saving one is not applying it,
// and the page says which is which rather than pretending.
import { computed, onMounted, reactive, ref } from 'vue'
import AppHeader from '../components/AppHeader.vue'
import Spinner from '../components/Spinner.vue'
import { ApiError, api, type Settings, type SettingsUpdate } from '../api'

const settings = ref<Settings | null>(null)
const loaded = ref(false)
const saving = ref(false)
const error = ref<string | null>(null)
const saved = ref(false)

// How the Claude credentials file is chosen. It is the one setting where an
// empty value is a choice of its own, so it cannot be a text box: "default"
// removes the key, "none" writes an empty string, "path" writes the path.
type CredentialsMode = 'default' | 'none' | 'path'

// The form. It starts as what the file says, because that is what a save edits.
const form = reactive({
  publicUrl: '',
  insecureHttp: false,
  dataDir: '',
  workspaceRoot: '',
  debug: false,
  clientId: '',
  clientSecret: '',
  allowedUsers: '',
  githubApiUrl: '',
  bitbucketApiUrl: '',
  credentialsMode: 'default' as CredentialsMode,
  credentials: '',
  anthropicApiKey: '',
  claudeBinary: '',
  claudeModel: '',
  gitUserName: '',
  gitUserEmail: '',
  vscodeDir: '',
  vscodeVersion: '',
  dockerHost: '',
  maxSessionsPerUser: 0,
  maxConcurrentBuilds: 0,
  publicRatePerMinute: 0,
})

const shadowed = computed(() => new Set(settings.value?.fromEnvironment ?? []))

// A setting whose saved value the running process has not picked up. The three
// the gate holds are applied on save, so they never appear here.
const waiting = computed(() => {
  const s = settings.value
  if (!s) return new Set<string>()
  const differs: [string, unknown, unknown][] = [
    ['publicUrl', s.running.publicUrl, s.saved.publicUrl],
    ['insecureHttp', s.running.insecureHttp, s.saved.insecureHttp],
    ['dataDir', s.running.dataDir, s.saved.dataDir],
    ['workspaceRoot', s.running.workspaceRoot, s.saved.workspaceRoot],
    ['debug', s.running.debug, s.saved.debug],
    ['github.apiUrl', s.running.github.apiUrl, s.saved.github.apiUrl],
    ['bitbucket.apiUrl', s.running.bitbucket.apiUrl, s.saved.bitbucket.apiUrl],
    ['claude.credentials', s.running.claude.credentials, s.saved.claude.credentials],
    ['claude.binary', s.running.claude.binary, s.saved.claude.binary],
    ['claude.model', s.running.claude.model, s.saved.claude.model],
    ['git.userName', s.running.git.userName, s.saved.git.userName],
    ['git.userEmail', s.running.git.userEmail, s.saved.git.userEmail],
    ['vscode.dir', s.running.vscode.dir, s.saved.vscode.dir],
    ['vscode.version', s.running.vscode.version, s.saved.vscode.version],
    ['docker.host', s.running.docker.host, s.saved.docker.host],
    ['limits.maxSessionsPerUser', s.running.limits.maxSessionsPerUser, s.saved.limits.maxSessionsPerUser],
    ['limits.maxConcurrentBuilds', s.running.limits.maxConcurrentBuilds, s.saved.limits.maxConcurrentBuilds],
    ['limits.publicRatePerMinute', s.running.limits.publicRatePerMinute, s.saved.limits.publicRatePerMinute],
  ]
  return new Set(differs.filter(([, running, file]) => running !== file).map(([key]) => key))
})

function fill(current: Settings) {
  settings.value = current
  const s = current.saved
  form.publicUrl = s.publicUrl
  form.insecureHttp = s.insecureHttp
  form.dataDir = s.dataDir
  form.workspaceRoot = s.workspaceRoot
  form.debug = s.debug
  form.clientId = s.github.clientId
  // Never filled from the server: it is never sent one.
  form.clientSecret = ''
  form.allowedUsers = s.github.allowedUsers.join('\n')
  form.githubApiUrl = s.github.apiUrl
  form.bitbucketApiUrl = s.bitbucket.apiUrl
  form.credentialsMode = s.claude.credentials === '' ? 'none' : 'path'
  form.credentials = s.claude.credentials
  form.anthropicApiKey = ''
  form.claudeBinary = s.claude.binary
  form.claudeModel = s.claude.model
  form.gitUserName = s.git.userName
  form.gitUserEmail = s.git.userEmail
  form.vscodeDir = s.vscode.dir
  form.vscodeVersion = s.vscode.version
  form.dockerHost = s.docker.host
  form.maxSessionsPerUser = s.limits.maxSessionsPerUser
  form.maxConcurrentBuilds = s.limits.maxConcurrentBuilds
  form.publicRatePerMinute = s.limits.publicRatePerMinute
}

onMounted(async () => {
  try {
    fill(await api.settings.get())
  } catch (e) {
    error.value = message(e)
  } finally {
    loaded.value = true
  }
})

function update(): SettingsUpdate {
  const body: SettingsUpdate = {
    publicUrl: form.publicUrl,
    insecureHttp: form.insecureHttp,
    dataDir: form.dataDir,
    workspaceRoot: form.workspaceRoot,
    debug: form.debug,
    github: {
      clientId: form.clientId,
      allowedUsers: form.allowedUsers.split('\n'),
      apiUrl: form.githubApiUrl,
    },
    bitbucket: { apiUrl: form.bitbucketApiUrl },
    claude: {
      credentials:
        form.credentialsMode === 'default' ? null : form.credentialsMode === 'none' ? '' : form.credentials,
      binary: form.claudeBinary,
      model: form.claudeModel,
    },
    git: { userName: form.gitUserName, userEmail: form.gitUserEmail },
    vscode: { dir: form.vscodeDir, version: form.vscodeVersion },
    docker: { host: form.dockerHost },
    limits: {
      maxSessionsPerUser: form.maxSessionsPerUser,
      maxConcurrentBuilds: form.maxConcurrentBuilds,
      publicRatePerMinute: form.publicRatePerMinute,
    },
  }
  // The secrets go only when there is something to say: an empty box means
  // "keep the stored one", and sending it empty would mean clearing it.
  if (form.clientSecret.trim()) body.github!.clientSecret = form.clientSecret
  if (form.anthropicApiKey.trim()) body.claude!.anthropicApiKey = form.anthropicApiKey
  return body
}

async function save() {
  saving.value = true
  error.value = null
  saved.value = false
  try {
    fill(await api.settings.save(update()))
    saved.value = true
  } catch (e) {
    error.value = message(e)
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
  <AppHeader />

  <main>
    <h1>Settings</h1>
    <p class="intro">
      Hexagon's configuration file, edited from here. Most of it is read once when the server
      starts, so a change is saved now and in force at the next start; the GitHub sign-in
      settings are the exception and take effect immediately.
    </p>

    <p v-if="error" class="error">{{ error }}</p>
    <p v-if="!loaded" class="hint"><Spinner />Loading…</p>

    <template v-if="settings">
      <p v-if="!settings.writable" class="error">
        <strong>{{ settings.configPath }}</strong> cannot be written by the server. Fix its
        permissions, or edit it by hand and restart.
      </p>
      <p v-else-if="saved && settings.restartRequired" class="notice">
        Saved. Some of it is waiting for a restart — the settings marked below are still
        running on their old values.
      </p>
      <p v-else-if="saved" class="notice">Saved, and in force now.</p>
      <p v-else-if="settings.restartRequired" class="notice">
        The configuration file has settings this server has not picked up. They are marked
        below, and a restart applies them.
      </p>

      <form @submit.prevent="save">
        <section class="card">
          <h2>Server</h2>

          <label class="field">
            <span>Listen address</span>
            <input :value="settings.running.addr" disabled />
            <span class="hint">
              <code>HEXAGON_ADDR</code>. Read-only: a wrong address is a port nobody can
              reach, and this page is behind it. Change it in the file and restart.
            </span>
          </label>

          <label class="field">
            <span>Public URL</span>
            <input v-model="form.publicUrl" spellcheck="false" />
            <span class="hint">
              <code>HEXAGON_PUBLIC_URL</code>. The origin browsers use. GitHub must be
              configured with the matching callback URL:
              <code>{{ settings.saved.callbackUrl }}</code>
            </span>
            <span v-if="shadowed.has('publicUrl')" class="hint env">
              Set by <code>HEXAGON_PUBLIC_URL</code>, which wins over what is saved here.
            </span>
            <span v-if="waiting.has('publicUrl')" class="hint waiting">
              Saved. Still serving <code>{{ settings.running.publicUrl }}</code> until a restart.
            </span>
          </label>

          <label class="check">
            <input type="checkbox" v-model="form.insecureHttp" />
            <span>Serve a non-loopback address without https</span>
          </label>
          <p class="hint">
            <code>HEXAGON_INSECURE_HTTP</code>. Without it the server refuses to start on an
            address the network can reach unless the public URL is https.
          </p>

          <label class="check">
            <input type="checkbox" v-model="form.debug" />
            <span>Debug logging</span>
          </label>
          <p class="hint"><code>HEXAGON_DEBUG</code>, one line per request.</p>
        </section>

        <section class="card">
          <h2>Storage</h2>

          <label class="field">
            <span>Data directory</span>
            <input v-model="form.dataDir" spellcheck="false" />
            <span class="hint">
              <code>HEXAGON_DATA_DIR</code>. Holds the database and the secret key.
              <strong>Changing it points the next start at a different database and a different
              secret key</strong>, so the sessions, the images and the stored provider tokens of
              this one will not be there.
            </span>
            <span v-if="waiting.has('dataDir')" class="hint waiting">
              Saved. Still using <code>{{ settings.running.dataDir }}</code> until a restart.
            </span>
          </label>

          <label class="field">
            <span>Workspace root</span>
            <input v-model="form.workspaceRoot" spellcheck="false" />
            <span class="hint"><code>HEXAGON_WORKSPACE_ROOT</code>, one directory per session.</span>
            <span v-if="waiting.has('workspaceRoot')" class="hint waiting">Waiting for a restart.</span>
          </label>

          <label class="field">
            <span>Secret key</span>
            <input :value="settings.running.secretKeySource" disabled />
            <span class="hint">
              <code>HEXAGON_SECRET_KEY</code>. Read-only, and shown as where it comes from rather
              than what it is: it seals every stored GitHub and Bitbucket token, so changing it
              would make all of them undecryptable.
            </span>
          </label>
        </section>

        <section class="card">
          <h2>GitHub sign-in</h2>
          <p class="hint applies-now">These three are applied as soon as they are saved.</p>

          <label class="field">
            <span>Client ID</span>
            <input v-model="form.clientId" autocomplete="off" spellcheck="false" />
            <span v-if="shadowed.has('github.clientId')" class="hint env">
              Set by <code>HEXAGON_GITHUB_CLIENT_ID</code>, which wins over what is saved here.
            </span>
          </label>

          <label class="field">
            <span>Client secret</span>
            <input
              v-model="form.clientSecret"
              type="password"
              autocomplete="off"
              :placeholder="settings.clientSecretSet ? 'Stored — leave blank to keep it' : 'Not set'"
            />
            <span v-if="shadowed.has('github.clientSecret')" class="hint env">
              Set by <code>HEXAGON_GITHUB_CLIENT_SECRET</code>, which wins over what is saved here.
            </span>
          </label>

          <label class="field">
            <span>Who may sign in, one per line</span>
            <textarea v-model="form.allowedUsers" rows="4" spellcheck="false"></textarea>
            <span class="hint">
              An account id or a login. Prefer ids: a login is released when an account is
              renamed, and can then be claimed by somebody else. This is also the list of people
              who can change these settings, so it cannot be saved without you in it.
            </span>
            <span v-if="shadowed.has('github.allowedUsers')" class="hint env">
              Set by <code>HEXAGON_ALLOWED_USERS</code>, which wins over what is saved here.
            </span>
          </label>

          <label class="field">
            <span>API URL</span>
            <input v-model="form.githubApiUrl" spellcheck="false" placeholder="https://api.github.com" />
            <span class="hint"><code>HEXAGON_GITHUB_API_URL</code>, for GitHub Enterprise.</span>
            <span v-if="waiting.has('github.apiUrl')" class="hint waiting">Waiting for a restart.</span>
          </label>
        </section>

        <section class="card">
          <h2>Bitbucket</h2>
          <label class="field">
            <span>API URL</span>
            <input v-model="form.bitbucketApiUrl" spellcheck="false" placeholder="https://api.bitbucket.org/2.0" />
            <span class="hint"><code>HEXAGON_BITBUCKET_API_URL</code>.</span>
            <span v-if="waiting.has('bitbucket.apiUrl')" class="hint waiting">Waiting for a restart.</span>
          </label>
        </section>

        <section class="card">
          <h2>Claude</h2>

          <label class="field">
            <span>Credentials mounted into sessions</span>
            <select v-model="form.credentialsMode">
              <option value="default">The default file, ~/.claude/.credentials.json</option>
              <option value="path">A specific file</option>
              <option value="none">No file at all</option>
            </select>
            <span class="hint"><code>HEXAGON_CLAUDE_CREDENTIALS</code>.</span>
          </label>

          <label v-if="form.credentialsMode === 'path'" class="field">
            <span>Path</span>
            <input v-model="form.credentials" spellcheck="false" />
          </label>
          <p v-if="waiting.has('claude.credentials')" class="hint waiting">
            Saved. Sessions started before a restart still use
            <code>{{ settings.running.claude.credentials || 'no file' }}</code>.
          </p>

          <label class="field">
            <span>Anthropic API key</span>
            <input
              v-model="form.anthropicApiKey"
              type="password"
              autocomplete="off"
              :placeholder="settings.anthropicApiKeySet ? 'Stored — leave blank to keep it' : 'Not set'"
            />
            <span class="hint">
              <code>ANTHROPIC_API_KEY</code>, injected into session containers instead of the
              credentials mount.
            </span>
          </label>

          <label class="field">
            <span>Claude binary</span>
            <input v-model="form.claudeBinary" spellcheck="false" placeholder="claude on PATH" />
            <span class="hint"><code>HEXAGON_CLAUDE_BINARY</code>, used to edit a Dockerfile from the Images page.</span>
            <span v-if="waiting.has('claude.binary')" class="hint waiting">Waiting for a restart.</span>
          </label>

          <label class="field">
            <span>Model</span>
            <input v-model="form.claudeModel" spellcheck="false" placeholder="the CLI's choice" />
            <span class="hint"><code>HEXAGON_CLAUDE_MODEL</code>.</span>
            <span v-if="waiting.has('claude.model')" class="hint waiting">Waiting for a restart.</span>
          </label>
        </section>

        <section class="card">
          <h2>Git identity</h2>
          <label class="field">
            <span>Name</span>
            <input v-model="form.gitUserName" spellcheck="false" />
            <span class="hint"><code>HEXAGON_GIT_USER_NAME</code>, for clones and container commits.</span>
            <span v-if="waiting.has('git.userName')" class="hint waiting">Waiting for a restart.</span>
          </label>
          <label class="field">
            <span>Email</span>
            <input v-model="form.gitUserEmail" spellcheck="false" />
            <span class="hint"><code>HEXAGON_GIT_USER_EMAIL</code>.</span>
            <span v-if="waiting.has('git.userEmail')" class="hint waiting">Waiting for a restart.</span>
          </label>
        </section>

        <section class="card">
          <h2>VS Code</h2>
          <label class="field">
            <span>Release directory</span>
            <input v-model="form.vscodeDir" spellcheck="false" />
            <span class="hint"><code>HEXAGON_VSCODE_DIR</code>, downloaded once for the machine.</span>
            <span v-if="waiting.has('vscode.dir')" class="hint waiting">Waiting for a restart.</span>
          </label>
          <label class="field">
            <span>Version</span>
            <input v-model="form.vscodeVersion" spellcheck="false" />
            <span class="hint">
              <code>HEXAGON_VSCODE_VERSION</code>. Fetched only when the directory is empty;
              upgrading means emptying it.
            </span>
            <span v-if="waiting.has('vscode.version')" class="hint waiting">Waiting for a restart.</span>
          </label>
        </section>

        <section class="card">
          <h2>Docker</h2>
          <label class="field">
            <span>Host</span>
            <input v-model="form.dockerHost" spellcheck="false" placeholder="the SDK default" />
            <span class="hint"><code>DOCKER_HOST</code>, the Docker Engine endpoint.</span>
            <span v-if="waiting.has('docker.host')" class="hint waiting">Waiting for a restart.</span>
          </label>
        </section>

        <section class="card">
          <h2>Limits</h2>
          <label class="field">
            <span>Sessions per account</span>
            <input v-model.number="form.maxSessionsPerUser" type="number" min="1" />
            <span class="hint"><code>HEXAGON_MAX_SESSIONS_PER_USER</code>. Creating another answers 429.</span>
            <span v-if="waiting.has('limits.maxSessionsPerUser')" class="hint waiting">Waiting for a restart.</span>
          </label>
          <label class="field">
            <span>Image builds in flight</span>
            <input v-model.number="form.maxConcurrentBuilds" type="number" min="1" />
            <span class="hint"><code>HEXAGON_MAX_CONCURRENT_BUILDS</code>.</span>
            <span v-if="waiting.has('limits.maxConcurrentBuilds')" class="hint waiting">Waiting for a restart.</span>
          </label>
          <label class="field">
            <span>Requests a minute, per address, without a session</span>
            <input v-model.number="form.publicRatePerMinute" type="number" min="1" />
            <span class="hint"><code>HEXAGON_PUBLIC_RATE_PER_MINUTE</code>.</span>
            <span v-if="waiting.has('limits.publicRatePerMinute')" class="hint waiting">Waiting for a restart.</span>
          </label>
        </section>

        <div class="actions">
          <p class="hint">Saved to <code>{{ settings.configPath }}</code>.</p>
          <button type="submit" class="primary" :disabled="saving || !settings.writable">
            <Spinner v-if="saving" />Save
          </button>
        </div>
      </form>
    </template>
  </main>
</template>

<style scoped>
main {
  max-width: 44rem;
  margin: 0 auto;
  padding: 1.5rem;
}

h1 {
  margin: 0 0 0.5rem;
}

.intro {
  margin: 0 0 1.5rem;
  color: var(--text-muted);
}

.card {
  margin-bottom: 1.25rem;
  padding: 1rem 1.25rem;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--surface);
}

.card h2 {
  margin: 0 0 1rem;
  font-size: 1rem;
}

.field {
  display: block;
  margin-bottom: 1.1rem;
}

.field > span:first-child {
  display: block;
  margin-bottom: 0.3rem;
  font-weight: 600;
}

input,
select,
textarea {
  display: block;
  width: 100%;
  padding: 0.5rem 0.65rem;
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

input:disabled {
  color: var(--text-muted);
  cursor: not-allowed;
}

.check {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  margin-bottom: 0.35rem;
  font-weight: 600;
}

.check input {
  width: auto;
}

.hint {
  display: block;
  margin: 0.3rem 0 1rem;
  color: var(--text-muted);
  font-size: 0.85rem;
  font-weight: 400;
}

.field .hint {
  margin-bottom: 0;
}

.hint.env,
.hint.waiting,
.hint.applies-now {
  margin-top: 0.3rem;
  color: var(--text);
}

.notice,
.error {
  margin: 0 0 1.25rem;
  padding: 0.7rem 0.9rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: var(--surface);
}

.error {
  border-color: var(--error);
  color: var(--error);
}

.actions {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 1rem;
}

.actions .hint {
  margin: 0;
  overflow-wrap: anywhere;
}

button {
  display: flex;
  align-items: center;
  gap: 0.4rem;
  padding: 0.5rem 1.1rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: var(--bg);
  color: var(--text);
  font: inherit;
  font-weight: 600;
  cursor: pointer;
}

button:hover:not(:disabled) {
  border-color: var(--accent);
}

button:disabled {
  cursor: default;
  opacity: 0.6;
}
</style>
