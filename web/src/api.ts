// Thin wrapper over the Hexagon JSON API. Every call goes to the same origin:
// in development Vite proxies /api to the Go server.

export interface Health {
  status: string
  uptime: string
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

export const api = {
  health: () => request<Health>('/health'),
  me: () => request<CurrentUser>('/auth/me'),
  logout: () => request<{ status: string }>('/auth/logout', { method: 'POST' }),
  loginUrl: '/api/auth/login',
}
