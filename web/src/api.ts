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
  repoFullName: string
  branch: string
  imageId: string
  imageRef: string
  repoDir: string
  status: SessionStatus
  error?: string
  autoClaude: boolean
  createdAt: string
}

export interface NewSession {
  repoFullName: string
  branch?: string
  imageId: string
  title?: string
  autoClaude?: boolean
}

// What a session's settings can be changed to after it exists. Omitted fields
// are left alone.
export interface SessionSettings {
  autoClaude?: boolean
}

export interface Repo {
  fullName: string
  cloneUrl: string
  defaultBranch: string
  private: boolean
  description: string
  updatedAt: string
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

  github: {
    repos: (refresh = false) => request<Repo[]>(`/github/repos${refresh ? '?refresh=1' : ''}`),
  },

  images: {
    list: () => request<Image[]>('/images'),
    create: (image: NewImage) =>
      request<Image>('/images', { method: 'POST', body: JSON.stringify(image) }),
    remove: (id: string) => request<null>(`/images/${id}`, { method: 'DELETE' }),
    log: (id: string) => request<ImageLog>(`/images/${id}/log`),
    template: () => request<{ dockerfile: string }>('/images/template'),
  },
}
