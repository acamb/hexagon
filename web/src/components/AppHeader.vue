<script setup lang="ts">
import { useRouter } from 'vue-router'
import { currentUser, logout } from '../session'

const router = useRouter()

async function signOut() {
  await logout()
  router.push({ name: 'login' })
}
</script>

<template>
  <header class="bar">
    <nav>
      <span class="brand">
        <picture>
          <source srcset="/hexagon-mark-dark.png" media="(prefers-color-scheme: dark)" />
          <img src="/hexagon-mark.png" alt="" width="64" height="57" />
        </picture>
        <strong>Hexagon</strong>
      </span>
      <RouterLink :to="{ name: 'sessions' }">Sessions</RouterLink>
      <RouterLink :to="{ name: 'images' }">Images</RouterLink>
      <RouterLink :to="{ name: 'accounts' }">Accounts</RouterLink>
      <RouterLink :to="{ name: 'settings' }">Settings</RouterLink>
    </nav>

    <div class="account" v-if="currentUser">
      <img v-if="currentUser.avatarUrl" :src="currentUser.avatarUrl" alt="" width="24" height="24" />
      <span>{{ currentUser.login }}</span>
      <button type="button" @click="signOut">Sign out</button>
    </div>
  </header>
</template>

<style scoped>
.bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0.75rem 1.5rem;
  border-bottom: 1px solid var(--border);
  background: var(--surface);
}

nav {
  display: flex;
  align-items: center;
  gap: 1.25rem;
}

.brand {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  /* Extra room after it, so the mark and the name do not read as one more
     destination in the list beside them. */
  margin-right: 0.5rem;
}

/* The intrinsic size is on the element so the bar does not reflow when the mark
   arrives; the height here is what it is actually drawn at. */
.brand img {
  width: auto;
  height: 1.75rem;
}

nav a {
  color: var(--text-muted);
  text-decoration: none;
}

nav a:hover {
  color: var(--text);
}

nav a.router-link-active {
  color: var(--text);
  font-weight: 600;
}

.account {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  color: var(--text-muted);
}

.account img {
  border-radius: 50%;
}

.account button {
  border: 1px solid var(--border);
  border-radius: 6px;
  background: transparent;
  color: inherit;
  font: inherit;
  padding: 0.25rem 0.6rem;
  cursor: pointer;
}

.account button:hover {
  border-color: var(--accent);
  color: var(--text);
}
</style>
