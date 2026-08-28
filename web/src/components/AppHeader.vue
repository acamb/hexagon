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
      <strong>Hexagon</strong>
      <RouterLink :to="{ name: 'sessions' }">Sessions</RouterLink>
      <RouterLink :to="{ name: 'images' }">Images</RouterLink>
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
  align-items: baseline;
  gap: 1.25rem;
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
