<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { signIn } from '../session'

const route = useRoute()

const messages: Record<string, string> = {
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
</script>

<template>
  <main class="login">
    <h1>Hexagon</h1>
    <p class="tagline">Claude Code sessions, one container each.</p>

    <p v-if="error" class="error">{{ error }}</p>

    <button type="button" class="signin" @click="signIn">Sign in with GitHub</button>
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
  margin: 0;
  font-size: 2.5rem;
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
</style>
