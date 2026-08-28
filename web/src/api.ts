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

export type ImageSource = 'dockerfile' | 'registry'
export type ImageStatus = 'pending' | 'building' | 'ready' | 'failed'

export interface Image {
  id: string
  name: string
  sourceType: ImageSource
  dockerfile?: string
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

// What Claude Code came back with when asked to change a Dockerfile.
export interface DockerfileEdit {
  dockerfile: string
  summary: string
}

export interface NewImage {
  name: string
  sourceType: ImageSource
  dockerfile?: string
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
  createdAt: string
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

export const api = {
  health: () => request<Health>('/health'),
  me: () => request<CurrentUser>('/auth/me'),
  logout: () => request<{ status: string }>('/auth/logout', { method: 'POST' }),
  loginUrl: '/api/auth/login',

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

  images: {
    list: () => request<Image[]>('/images'),
    create: (image: NewImage) =>
      request<Image>('/images', { method: 'POST', body: JSON.stringify(image) }),
    remove: (id: string) => request<null>(`/images/${id}`, { method: 'DELETE' }),
    log: (id: string) => request<ImageLog>(`/images/${id}/log`),
    // canAsk is false when the server has no Claude Code binary to run, and the
    // page then leaves the control out instead of offering one that fails.
    template: () => request<{ dockerfile: string; canAsk: boolean }>('/images/template'),
    editDockerfile: (dockerfile: string, instruction: string) =>
      request<DockerfileEdit>('/images/dockerfile', {
        method: 'POST',
        body: JSON.stringify({ dockerfile, instruction }),
      }),
  },
}
