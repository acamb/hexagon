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
  addr: string
  // secretKeySource is shown and cannot be changed from here: a wrong key could
  // not be corrected from this page afterwards, because it would make every
  // sealed token undecryptable.
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
  addr?: string
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

// uploadRestore posts a File body with the archive's own content type and
// parses the JSON inspection back. The first api.ts helper that is not
// request<T>: a File cannot go through the Content-Type request already sets
// for every other mutating call, because it is not JSON.
async function uploadRestore(file: File | Blob): Promise<RestoreInspection> {
  const response = await fetch('/api/images/restore', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { Accept: 'application/json', 'Content-Type': 'application/gzip' },
    body: file,
  })
  const body = await response.json().catch(() => null)
  if (!response.ok) {
    throw new ApiError(response.status, body?.error ?? response.statusText)
  }
  return body as RestoreInspection
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
  // Whether asking Claude goes through a container because the server has no
  // claude binary. It works, and it is slower: the page says so rather than
  // looking merely sluggish.
  askInContainer: boolean
  canCompose: boolean
}

export interface NewImage {
  name: string
  sourceType: ImageSource
  dockerfile?: string
  compose?: string
  registryRef?: string
}

// What an existing Dockerfile or compose image's source can be rebuilt from.
// Sent whole rather than as an omittable patch: a rebuild always replaces
// both files a compose image carries, since the two describe one build.
export interface ImageSourceUpdate {
  dockerfile: string
  compose?: string
}

// A backup on its way out of Hexagon, or a restore on its way in: a file
// staged on the server, a job that is or is not finished with it, and a
// lifetime after which the janitor removes both.
export type TransferDirection = 'backup' | 'restore'
export type TransferStatus = 'pending' | 'running' | 'ready' | 'failed'

export interface Transfer {
  id: string
  direction: TransferDirection
  imageId?: string
  name: string
  status: TransferStatus
  withImage: boolean
  size?: number
  error?: string
  createdAt: string
  expiresAt: string
}

// What backing up an image asks for: withSpec is the Dockerfile and compose
// file, withImage the image export. Chosen independently, and at least one is
// required.
export interface NewBackup {
  withSpec: boolean
  withImage: boolean
}

// What uploading a backup archive answers with: the spec, already read out of
// it, and enough about its image half to offer the two restore choices.
export interface RestoreInspection {
  id: string
  name: string
  sourceType: ImageSource
  dockerfile?: string
  compose?: string
  hasImage: boolean
  imageSize?: number
  nameExists: boolean
  existingImageId?: string
}

export interface ImportRequest {
  name: string
  overwrite: boolean
}

// The host's CPU, memory and filesystems, for the Stats page. usedPercent is
// absent on the first read after the server started: hostinfo has only one
// /proc/stat sample so far, and a percentage against boot would be wrong in a
// way nobody would notice. available is false on a platform with no /proc, in
// which case everything else here is absent too.
export interface HostCPU {
  cores: number
  usedPercent?: number
  load: [number, number, number]
}

export interface HostMemory {
  total: number
  available: number
}

export interface HostFilesystem {
  path: string
  total: number
  free: number
}

export interface HostStats {
  available: boolean
  cpu?: HostCPU
  memory?: HostMemory
  filesystems?: HostFilesystem[]
}

// What images, containers, volumes and build cache cost on the daemon, in
// bytes, the way `docker system df` reports it.
export interface DockerUsage {
  images: number
  containers: number
  volumes: number
  buildCache: number
}

// One image as the daemon holds it, not a Hexagon row. inUse means a
// container, running or stopped, is based on it; registered means a Hexagon
// image row points at it, which is enough on its own to keep the prune button
// from touching it, whether or not anything is using it right now.
export interface DockerImage {
  id: string
  tags: string[]
  size: number
  created: string
  containers: number
  inUse: boolean
  registered: boolean
  dangling: boolean
}

export interface DockerStats {
  host: string
  // False when host names a daemon on a different machine from this server:
  // the host gauges and these figures then describe two different computers.
  sameMachine: boolean
  usage: DockerUsage
  images: DockerImage[]
}

export interface Stats {
  host: HostStats
  docker: DockerStats
}

export interface PruneResult {
  removed: number
  reclaimed: number
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
  // nothing to report before then and nothing to store afterwards. The
  // container side can be changed while the session is stopped — see
  // sessions.setPorts.
  ports: SessionPort[]
  // The host interface those ports are bound to, "127.0.0.1" for a session that
  // named none. Anything else means they are reachable from off this machine.
  portAddress: string
  // Whether the session is a compose project rather than a single container,
  // which is a property of the image it came from.
  compose: boolean
  // The Claude account this session's container authenticates with, empty
  // when it resolves dynamically — the user's default account, or the
  // server's own configuration. Editable while the session is stopped — see
  // sessions.setClaudeAccount.
  claudeAccountId: string
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
  // Container ports to publish. The host side is Docker's to choose. A
  // container keeps the bindings it was created with, so changing these later
  // rebuilds it, and only while the session is stopped.
  ports?: number[]
  // The host interface they bind. Omitted means loopback: the server gives the
  // closed answer to a client that does not ask, and it is the dialog that
  // proposes 0.0.0.0 with the warning beside it.
  portAddress?: string
  // Which Claude account the container authenticates with. Omitted resolves
  // to the user's default account, or, absent one, the server's own
  // configuration.
  claudeAccountId?: string
}

// What a session's settings can be changed to after it exists. Omitted fields
// are left alone.
export interface SessionSettings {
  autoClaude?: boolean
}

// What a stopped session publishes. Both fields replace what is there: the
// whole list, and the interface it binds. Honouring them means rebuilding the
// container, which is why this is not part of SessionSettings.
export interface SessionPorts {
  ports: number[]
  portAddress: string
}

// What a session's claude account can be changed to, while stopped. Honouring
// it means rebuilding the container, the same reason SessionPorts is not part
// of SessionSettings.
export interface SessionClaudeAccount {
  claudeAccountId: string
}

// The answer to the probe the export button runs before it navigates
// anywhere. files and bytes are absent together with exists false: there is
// nothing to report for a directory that is not there.
export interface WorkspaceInfo {
  exists: boolean
  files?: number
  bytes?: number
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
  // Whether a long-lived token has been pasted for git. The value never comes
  // back from the server, only whether there is one.
  gitTokenSet: boolean
}

export type ClaudeAccountKind = 'api_key' | 'oauth_token' | 'login'
// What a session naming no account will actually authenticate with: the
// user's default account, the server's own configured key, the file the
// machine-wide login wrote, or none of the above.
export type ClaudeSource = 'account' | 'apiKey' | 'file' | 'none'

export interface ClaudeFile {
  path: string
  present: boolean
  updatedAt?: string
}

export interface ClaudeStatus {
  file: ClaudeFile
  effective: ClaudeSource
  canLogin: boolean
  canVerify: boolean
  // Whether the CLI runs in a container on this server, which is what happens
  // when there is no claude binary on it.
  inContainer: boolean
  // The image Hexagon builds for itself, which is what a browser login runs in
  // when you have built none of your own. Absent with no Docker to build it.
  defaultImage?: DefaultImage
}

// One Claude account: a pasted API key or OAuth token, or a subscription
// signed in to through the browser. Never carries a secret.
export interface ClaudeAccount {
  id: string
  name: string
  kind: ClaudeAccountKind
  default: boolean
  createdAt: string
  updatedAt: string
  // Present only for a login account: whether anybody has signed in on it yet.
  // A row can exist before that happens.
  login?: { present: boolean; updatedAt?: string }
}

export interface NewClaudeAccount {
  name: string
  kind: ClaudeAccountKind
  // Absent for a login account: it takes no secret at all.
  secret?: string
}

// What an existing account can be changed to. Omitted fields are left alone;
// `default: true` is the only value default takes, since there is no way to
// unmake an account the default except by making another one it instead.
export interface ClaudeAccountUpdate {
  name?: string
  secret?: string
  default?: true
}

export interface DefaultImage {
  ready: boolean
  building: boolean
  error?: string
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
    // Only while the session is stopped: the server rebuilds its container to
    // publish anything else, and refuses with 409 while it runs.
    setPorts: (id: string, ports: SessionPorts) =>
      request<Session>(`/sessions/${id}/ports`, { method: 'PUT', body: JSON.stringify(ports) }),
    // Only while the session is stopped, for the same reason as setPorts.
    setClaudeAccount: (id: string, account: SessionClaudeAccount) =>
      request<Session>(`/sessions/${id}/claude-account`, { method: 'PUT', body: JSON.stringify(account) }),
    start: (id: string) => request<Session>(`/sessions/${id}/start`, { method: 'POST' }),
    stop: (id: string) => request<Session>(`/sessions/${id}/stop`, { method: 'POST' }),
    remove: (id: string, purge: boolean) =>
      request<null>(`/sessions/${id}?purge=${purge}`, { method: 'DELETE' }),
    // Checked before workspaceUrl is ever navigated to: the button must tell
    // the user there is nothing to export before it tries, not after.
    workspaceInfo: (id: string) => request<WorkspaceInfo>(`/sessions/${id}/workspace/info`),
    // A plain navigation, in the shape of api.claude.loginTerminal and
    // api.transfers.downloadUrl: `<a download>` rather than a fetch into
    // memory, and the archive is never staged anywhere to fetch back from.
    workspaceUrl: (id: string) => `/api/sessions/${id}/workspace`,
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
    setGitToken: (provider: ProviderKind, secret: string) =>
      request<Account>(`/accounts/${provider}/git-token`, {
        method: 'PUT',
        body: JSON.stringify({ secret }),
      }),
    clearGitToken: (provider: ProviderKind) =>
      request<null>(`/accounts/${provider}/git-token`, { method: 'DELETE' }),
  },

  claude: {
    status: () => request<ClaudeStatus>('/claude'),
    stopLogin: () => request<null>('/claude/login', { method: 'DELETE' }),
    // The terminal is a WebSocket, so it is a path for TerminalPane rather than
    // a fetch.
    // An empty id asks for Hexagon's own image, which is what a browser with no
    // image of its own to offer sends.
    loginTerminal: (imageId: string) =>
      imageId
        ? `/api/claude/login/terminal?image=${encodeURIComponent(imageId)}`
        : '/api/claude/login/terminal',

    accounts: {
      list: () => request<ClaudeAccount[]>('/claude/accounts'),
      create: (account: NewClaudeAccount) =>
        request<ClaudeAccount>('/claude/accounts', { method: 'POST', body: JSON.stringify(account) }),
      update: (id: string, update: ClaudeAccountUpdate) =>
        request<ClaudeAccount>(`/claude/accounts/${id}`, { method: 'PATCH', body: JSON.stringify(update) }),
      remove: (id: string) => request<null>(`/claude/accounts/${id}`, { method: 'DELETE' }),
      stopLogin: (id: string) => request<null>(`/claude/accounts/${id}/login`, { method: 'DELETE' }),
      loginTerminal: (id: string, imageId: string) =>
        imageId
          ? `/api/claude/accounts/${id}/login/terminal?image=${encodeURIComponent(imageId)}`
          : `/api/claude/accounts/${id}/login/terminal`,
    },
  },

  stats: {
    get: () => request<Stats>('/stats'),
    pruneImages: () => request<PruneResult>('/stats/prune/images', { method: 'POST' }),
    pruneContainers: () => request<PruneResult>('/stats/prune/containers', { method: 'POST' }),
  },

  images: {
    list: () => request<Image[]>('/images'),
    create: (image: NewImage) =>
      request<Image>('/images', { method: 'POST', body: JSON.stringify(image) }),
    remove: (id: string) => request<null>(`/images/${id}`, { method: 'DELETE' }),
    log: (id: string) => request<ImageLog>(`/images/${id}/log`),
    rebuild: (id: string, update: ImageSourceUpdate) =>
      request<Image>(`/images/${id}/rebuild`, { method: 'POST', body: JSON.stringify(update) }),
    template: () => request<ImageTemplate>('/images/template'),
    editSource: (kind: SourceKind, content: string, instruction: string) =>
      request<SourceEdit>('/images/source', {
        method: 'POST',
        body: JSON.stringify({ kind, content, instruction }),
      }),
    backup: (id: string, options: NewBackup) =>
      request<Transfer>(`/images/${id}/backup`, { method: 'POST', body: JSON.stringify(options) }),
    // Stages the archive and answers with what it found, without importing
    // anything yet.
    restore: (file: File | Blob) => uploadRestore(file),
    // Only the full restore path posts here: a spec-only restore just fills in
    // the create form with what restore() already returned.
    import: (transferId: string, req: ImportRequest) =>
      request<Image>(`/images/restore/${transferId}/import`, { method: 'POST', body: JSON.stringify(req) }),
  },

  transfers: {
    list: () => request<Transfer[]>('/transfers'),
    remove: (id: string) => request<null>(`/transfers/${id}`, { method: 'DELETE' }),
    // A plain navigation, in the shape of api.claude.loginTerminal: `<a
    // download>` rather than a fetch into memory, which for a multi-gigabyte
    // archive is the whole point.
    downloadUrl: (id: string) => `/api/transfers/${id}/file`,
  },
}
