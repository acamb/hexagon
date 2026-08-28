// Who is signed in. Kept in a module rather than a store: it is one value, read
// by the router guard and the header.
import { ref } from 'vue'
import { ApiError, api, type CurrentUser } from './api'

export const currentUser = ref<CurrentUser | null>(null)

let pending: Promise<CurrentUser | null> | null = null

// load resolves the session against the server, once, and caches the result.
// Concurrent callers share the same request.
export function load(): Promise<CurrentUser | null> {
  if (currentUser.value) return Promise.resolve(currentUser.value)
  if (!pending) {
    pending = api
      .me()
      .then((user) => {
        currentUser.value = user
        return user
      })
      .catch((e) => {
        if (e instanceof ApiError && e.unauthorized) return null
        throw e
      })
      .finally(() => {
        pending = null
      })
  }
  return pending
}

export async function logout(): Promise<void> {
  await api.logout()
  currentUser.value = null
}

export function signIn(): void {
  window.location.href = api.loginUrl
}
