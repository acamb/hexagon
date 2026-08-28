<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import AppHeader from '../components/AppHeader.vue'
import StatusDot from '../components/StatusDot.vue'
import TerminalPane from '../components/TerminalPane.vue'
import { api, type Session } from '../api'

const route = useRoute()
const sessionId = route.params.id as string

const session = ref<Session | null>(null)
const error = ref<string | null>(null)

onMounted(async () => {
  try {
    session.value = await api.sessions.get(sessionId)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
})
</script>

<template>
  <div class="layout">
    <AppHeader />

    <div v-if="error" class="notice error">{{ error }}</div>

    <template v-else-if="session">
      <div class="meta">
        <div>
          <strong>{{ session.repoFullName }}</strong>
          <span class="branch" v-if="session.branch">{{ session.branch }}</span>
        </div>
        <StatusDot :status="session.status" />
      </div>

      <TerminalPane v-if="session.status === 'running'" :session-id="session.id" class="terminal" />
      <div v-else class="notice">
        This session is {{ session.status }}. The terminal is available while it runs.
      </div>
    </template>

    <div v-else class="notice">Loading…</div>
  </div>
</template>

<style scoped>
.layout {
  display: flex;
  flex-direction: column;
  height: 100svh;
}

.meta {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0.6rem 1.5rem;
  border-bottom: 1px solid var(--border);
}

.branch {
  margin-left: 0.6rem;
  padding: 0.1rem 0.45rem;
  border: 1px solid var(--border);
  border-radius: 999px;
  color: var(--text-muted);
  font-size: 0.85rem;
}

.terminal {
  flex: 1;
  min-height: 0;
}

.notice {
  padding: 1.5rem;
  color: var(--text-muted);
}

.notice.error {
  color: var(--error);
}
</style>
