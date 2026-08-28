<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import AppHeader from '../components/AppHeader.vue'
import { api, type Health } from '../api'

const health = ref<Health | null>(null)
const error = ref<string | null>(null)

async function refresh() {
  try {
    health.value = await api.health()
    error.value = null
  } catch (e) {
    health.value = null
    error.value = e instanceof Error ? e.message : String(e)
  }
}

let timer: number
onMounted(() => {
  refresh()
  timer = window.setInterval(refresh, 5000)
})
onUnmounted(() => window.clearInterval(timer))
</script>

<template>
  <AppHeader />

  <main class="shell">
    <h1>Sessions</h1>
    <p class="placeholder">Session management arrives with the next milestone.</p>

    <p v-if="error" class="status status--error">Backend unreachable: {{ error }}</p>
    <p v-else-if="health" class="status" :class="health.docker === 'ok' ? 'status--ok' : 'status--error'">
      Backend {{ health.status }} &middot; up {{ health.uptime }} &middot; docker {{ health.docker }}
    </p>
    <p v-else class="status">Contacting backend&hellip;</p>
  </main>
</template>

<style scoped>
.shell {
  max-width: 48rem;
  margin: 3rem auto;
  padding: 0 1.5rem;
}

h1 {
  margin: 0 0 0.25rem;
}

.placeholder {
  margin: 0 0 2rem;
  color: var(--text-muted);
}

.status {
  font-variant-numeric: tabular-nums;
}

.status--ok {
  color: var(--ok);
}

.status--error {
  color: var(--error);
}
</style>
