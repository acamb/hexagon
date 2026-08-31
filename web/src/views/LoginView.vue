<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { signIn } from '../session'

const route = useRoute()
const router = useRouter()

const messages: Record<string, string> = {
  not_configured: 'This server has not been set up yet.',
  invalid_state: 'The sign-in attempt expired. Please try again.',
  missing_code: 'GitHub did not return an authorization code.',
  exchange_failed: 'GitHub rejected the sign-in. Please try again.',
  github_failed: 'Could not read your GitHub account.',
  not_allowed: 'That GitHub account is not allowed to use this Hexagon instance.',
  server_error: 'Something went wrong while signing you in.',
}

const error = computed(() => {
  const code = route.query.error
  if (typeof code !== 'string') return null
  return messages[code] ?? 'Sign-in failed.'
})

// Offer the first-time wizard while it is open. Asking here rather than in the
// router guard keeps the extra request off every navigation: only somebody who
// is not signed in ever reaches this page.
const setupRequired = ref(false)

onMounted(async () => {
  try {
    setupRequired.value = (await api.setup.status()).required
  } catch {
    // The wizard is an offer, not a requirement. A server that cannot answer
    // is one this page cannot help with either.
    return
  }
  if (setupRequired.value && route.query.error === 'not_configured') {
    router.replace('/setup')
  }
})
</script>

<template>
  <main class="login">
    <h1>
      <picture>
        <source srcset="/hexagon-logo-dark.png" media="(prefers-color-scheme: dark)" />
        <img src="/hexagon-logo.png" alt="Hexagon" width="420" height="443" />
      </picture>
    </h1>
    <p class="tagline">Claude Code sessions, one container each.</p>

    <p v-if="error" class="error">{{ error }}</p>

    <button type="button" class="signin" @click="signIn">Sign in with GitHub</button>

    <p v-if="setupRequired" class="setup">
      <RouterLink to="/setup">First-time setup</RouterLink>
    </p>
  </main>
</template>

<style scoped>
.login {
  max-width: 24rem;
  margin: 15vh auto 0;
  padding: 0 1.5rem;
  text-align: center;
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

.tagline {
  margin: 0.25rem 0 2rem;
  color: var(--text-muted);
}

.error {
  margin-bottom: 1.5rem;
  padding: 0.75rem 1rem;
  border: 1px solid var(--error);
  border-radius: 6px;
  color: var(--error);
  text-align: left;
}

.signin {
  width: 100%;
  padding: 0.75rem 1rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: var(--surface);
  color: var(--text);
  font: inherit;
  font-weight: 600;
  cursor: pointer;
}

.signin:hover {
  border-color: var(--accent);
}

.setup {
  margin-top: 1.25rem;
  font-size: 0.9rem;
}
</style>
