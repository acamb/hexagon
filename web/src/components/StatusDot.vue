<script setup lang="ts">
defineProps<{ status: string }>()
</script>

<template>
  <span class="status" :class="`status--${status}`">
    <span class="dot" />
    {{ status }}
  </span>
</template>

<style scoped>
.status {
  display: inline-flex;
  align-items: center;
  gap: 0.4rem;
  color: var(--text-muted);
  font-size: 0.9rem;
}

.dot {
  width: 0.5rem;
  height: 0.5rem;
  border-radius: 50%;
  background: var(--text-muted);
}

.status--ready .dot,
.status--running .dot {
  background: var(--ok);
}

.status--failed .dot {
  background: var(--error);
}

.status--building .dot {
  background: var(--accent);
  animation: pulse 1.2s ease-in-out infinite;
}

@keyframes pulse {
  50% {
    opacity: 0.25;
  }
}
</style>
