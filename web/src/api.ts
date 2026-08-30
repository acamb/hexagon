// Thin wrapper over the Hexagon JSON API. Every call goes to the same origin:
// in development Vite proxies /api to the Go server.

export interface Health {
  status: string
  uptime: string
  docker?: string
  error?: string
}

export interface CurrentUser {
  id: string
  login: string
  avatarUrl: string
}

// SetupStatus describes the first-time wizard. Everything but `required` is
// absent once the wizard has closed, and `writable` is absent rather than false
// when the configuration file cannot be written.
export interface SetupStatus {
  required: boolean
  configPath?: string
  writable?: boolean
  callbackUrl?: string
  clientId?: string
  allowedUsers?: string[]
  // Settings an environment variable is supplying, by their configuration file
  // key. The variable outranks the file, so writing these here has no effect
  // until it goes away.
  fromEnvironment?: string[]
}

// The settings page's whole view of the server: what the process is running on,
// what its configuration file now says, and whether the two have drifted apart.
// Most settings are captured when the server starts, so saving one is not the
// same as applying it — restartRequired is that difference.
export interface Settings {
  configPath: string
  writable: boolean
  restartRequired: boolean
  // Settings an environment variable is supplying, by their configuration file
  // key. The variable outranks the file, so writing those here has no effect
  // until it goes away.
  fromEnvironment?: string[]
  // The two secrets in the file are reported as present or absent, never sent.
  clientSecretSet: boolean
  anthropicApiKeySet: boolean
  running: SettingsValues
  saved: SettingsValues
}

export interface SettingsValues {
  // addr and secretKeySource are shown and cannot be changed from here: a wrong
  // value in either could not be corrected from this page afterwards.
  addr: string
  secretKeySource: string
  publicUrl: string
  insecureHttp: boolean
  dataDir: string
  workspaceRoot: string
  debug: boolean
  github: { clientId: string; allowedUsers: string[]; apiUrl: string }
  bitbucket: { apiUrl: string }
  claude: { credentials: string; binary: string; model: string }
  git: { userName: string; userEmail: string }
  vscode: { dir: string; version: string }
  docker: { host: string; cli: string }
  limits: { maxSessionsPerUser: number; maxConcurrentBuilds: number; publicRatePerMinute: number }
  callbackUrl: string
}

// Every field is optional: an omitted one is left alone. That is how the two
// secrets are kept without the browser ever seeing them, and
// `claude.credentials` uses all three states — omitted leaves it, null puts it
// back to the default path, and "" mounts nothing.
export interface SettingsUpdate {
  publicUrl?: string
  insecureHttp?: boolean
  dataDir?: string
  workspaceRoot?: string
  debug?: boolean
  github?: { clientId?: string; clientSecret?: string; allowedUsers?: string[]; apiUrl?: string }
  bitbucket?: { apiUrl?: string }
  claude?: { credentials?: string | null; anthropicApiKey?: string; binary?: string; model?: string }
  git?: { userName?: string; userEmail?: string }
  vscode?: { dir?: string; version?: string }
  docker?: { host?: string; cli?: string }
  limits?: { maxSessionsPerUser?: number; maxConcurrentBuilds?: number; publicRatePerMinute?: number }
}

export interface SetupRequest {
  password: string
  clientId: string
  clientSecret: string
  allowedUsers: string[]
}

export class ApiError extends Error {
  status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }

  get unauthorized(): boolean {
    return this.status === 401
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const method = init?.method ?? 'GET'
  const response = await fetch(`/api${path}`, {
    credentials: 'same-origin',
    ...init,
    headers: {
      Accept: 'application/json',
      // The server rejects mutating requests that do not look like they came
      // from this app; an HTML form cannot set this content type.
      ...(method === 'GET' || method === 'HEAD' ? {} : { 'Content-Type': 'application/json' }),
      ...init?.headers,
    },
  })

  const body = await response.json().catch(() => null)
  if (!response.ok) {
    throw new ApiError(response.status, body?.error ?? response.statusText)
  }
  return body as T
}

// 'compose' is an advanced image: a Dockerfile and a compose file together. The
// Dockerfile still describes the container Claude Code runs in and builds like
// any other; the compose file describes the services beside it.
export type ImageSource = 'dockerfile' | 'registry' | 'compose'
export type ImageStatus = 'pending' | 'building' | 'ready' | 'failed'

export interface Image {
  id: string
  name: string
  sourceType: ImageSource
  dockerfile?: string
  compose?: string
  registryRef?: string
  imageRef?: string
  status: ImageStatus
  error?: string
  createdAt: string
}

export interface ImageLog {
  status: ImageStatus
  log: string
  error: string
}

// Which of an image's two files Claude Code is being asked to change. The
// values are the image source types they belong to.
export type SourceKind = 'dockerfile' | 'compose'

// What Claude Code came back with when asked to change one of them.
export interface SourceEdit {
  content: string
  summary: string
}

// What the Images page starts from, and what this server can do besides
// building: canAsk is false with no Claude Code binary, canCompose false with no
// `docker compose`. The page leaves each control out rather than offering one
// that always fails.
export interface ImageTemplate {
  dockerfile: string
  compose: string
  canAsk: boolean
  canCompose: boolean
}

export interface NewImage {
  name: string
  sourceType: ImageSource
  dockerfile?: string
  compose?: string
  registryRef?: string
}

export type SessionStatus =
  | 'creating'
  | 'cloning'
  | 'starting'
  | 'running'
  | 'stopped'
  | 'failed'
  | 'gone'

export interface Session {
  id: string
  title: string
  // Empty together for a session created without a repository: it started on
  // an empty workspace instead of a clone.
  repoFullName: string
  branch: string
  imageId: string
  imageRef: string
  // The account the session is attached to, empty when it is attached to none.
  provider: ProviderKind | ''
  // The directory mounted at /workspace, clone or not.
  repoDir: string
  status: SessionStatus
  error?: string
  autoClaude: boolean
  // Whether the container was given the credentials of the account above.
  // Decided when the session was created and never afterwards: a container
  // keeps the environment it was created with.
  propagateToken: boolean
  // Whether the container publishes code-server and has the release bind
  // mounted. Decided at creation, like propagateToken above: the mount and the
  // port binding are the container.
  vscode: boolean
  // The ports the session publishes. `host` is absent until the container is
  // running: Docker picks a new host port every time it starts, so there is
  // nothing to report before then and nothing to store afterwards.
  ports: SessionPort[]
  // Whether the session is a compose project rather than a single container,
  // which is a property of the image it came from.
  compose: boolean
  createdAt: string
}

export interface SessionPort {
  container: number
  host?: number
}

export interface NewSession {
  // With a repository, which account it comes from. Without one, which
  // account's token the session gets: empty means none.
  provider?: ProviderKind | ''
  // Omitted for a session that starts on an empty workspace.
  repoFullName?: string
  branch?: string
  imageId: string
  title?: string
  autoClaude?: boolean
  propagateToken?: boolean
  // Off by default, unlike the two above: it costs a mount and a published
  // port, and a session that never opens the editor should carry neither.
  vscode?: boolean
  // Container ports to publish on the host's loopback interface. The host side
  // is Docker's to choose. Settable only here: a container keeps the port
  // bindings it was created with.
  ports?: number[]
}

// What a session's settings can be changed to after it exists. Omitted fields
// are left alone.
export interface SessionSettings {
  autoClaude?: boolean
}

// The providers this build knows. The value is what the API sends and stores,
// so it is part of the contract rather than a label.
export type ProviderKind = 'github' | 'bitbucket'

export const providerNames: Record<ProviderKind, string> = {
  github: 'GitHub',
  bitbucket: 'Bitbucket',
}

export interface Repo {
  provider: ProviderKind
  fullName: string
  cloneUrl: string
  defaultBranch: string
  private: boolean
  description: string
  updatedAt: string
}

// Repositories from every connected account, plus the accounts that could not
// be reached: one expired token must not hide the rest of the list.
export interface RepoListing {
  repos: Repo[]
  failed?: Partial<Record<ProviderKind, string>>
}

export interface Account {
  provider: ProviderKind
  account: string
  identity?: string
  avatarUrl?: string
  connected: boolean
  updatedAt?: string
  // False for GitHub: it is the account you signed in with.
  removable: boolean
}

export type ClaudeCredentialKind = 'api_key' | 'oauth_token'
// What a new session will actually authenticate with: the credential pasted
// here, the server's own configured key, the file a browser login wrote, or
// none of the above.
export type ClaudeSource = 'credential' | 'apiKey' | 'file' | 'none'

export interface ClaudeCredential {
  kind: ClaudeCredentialKind
  updatedAt: string
}

export interface ClaudeFile {
  path: string
  present: boolean
  updatedAt?: string
}

export interface ClaudeStatus {
  credential: ClaudeCredential | null
  file: ClaudeFile
  effective: ClaudeSource
  canLogin: boolean
  canVerify: boolean
}

export const api = {
  health: () => request<Health>('/health'),
  me: () => request<CurrentUser>('/auth/me'),
  logout: () => request<{ status: string }>('/auth/logout', { method: 'POST' }),
  loginUrl: '/api/auth/login',

  setup: {
    status: () => request<SetupStatus>('/setup'),
    save: (setup: SetupRequest) =>
      request<SetupStatus>('/setup', { method: 'POST', body: JSON.stringify(setup) }),
  },

  settings: {
    get: () => request<Settings>('/settings'),
    save: (settings: SettingsUpdate) =>
      request<Settings>('/settings', { method: 'PUT', body: JSON.stringify(settings) }),
  },

  sessions: {
    list: () => request<Session[]>('/sessions'),
    get: (id: string) => request<Session>(`/sessions/${id}`),
    create: (session: NewSession) =>
      request<Session>('/sessions', { method: 'POST', body: JSON.stringify(session) }),
    update: (id: string, settings: SessionSettings) =>
      request<Session>(`/sessions/${id}`, { method: 'PATCH', body: JSON.stringify(settings) }),
    start: (id: string) => request<Session>(`/sessions/${id}/start`, { method: 'POST' }),
    stop: (id: string) => request<Session>(`/sessions/${id}/stop`, { method: 'POST' }),
    remove: (id: string, purge: boolean) =>
      request<null>(`/sessions/${id}?purge=${purge}`, { method: 'DELETE' }),
  },

  repos: (refresh = false) => request<RepoListing>(`/repos${refresh ? '?refresh=1' : ''}`),

  accounts: {
    list: () => request<Account[]>('/accounts'),
    connect: (provider: ProviderKind, identity: string, secret: string) =>
      request<Account>(`/accounts/${provider}`, {
        method: 'PUT',
        body: JSON.stringify({ identity, secret }),
      }),
    disconnect: (provider: ProviderKind) =>
      request<null>(`/accounts/${provider}`, { method: 'DELETE' }),
  },

  claude: {
    status: () => request<ClaudeStatus>('/claude'),
    setCredential: (kind: ClaudeCredentialKind, secret: string) =>
      request<ClaudeStatus>('/claude/credential', { method: 'PUT', body: JSON.stringify({ kind, secret }) }),
    forgetCredential: () => request<null>('/claude/credential', { method: 'DELETE' }),
    stopLogin: () => request<null>('/claude/login', { method: 'DELETE' }),
    // The terminal is a WebSocket, so it is a path for TerminalPane rather than
    // a fetch.
    loginTerminal: (imageId: string) => `/api/claude/login/terminal?image=${encodeURIComponent(imageId)}`,
  },

  images: {
    list: () => request<Image[]>('/images'),
    create: (image: NewImage) =>
      request<Image>('/images', { method: 'POST', body: JSON.stringify(image) }),
    remove: (id: string) => request<null>(`/images/${id}`, { method: 'DELETE' }),
    log: (id: string) => request<ImageLog>(`/images/${id}/log`),
    template: () => request<ImageTemplate>('/images/template'),
    editSource: (kind: SourceKind, content: string, instruction: string) =>
      request<SourceEdit>('/images/source', {
        method: 'POST',
        body: JSON.stringify({ kind, content, instruction }),
      }),
  },
}
