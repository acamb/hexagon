<script setup lang="ts">
// The "this is running" indicator for a button that has started something and
// is waiting for the server. It sizes and colours itself from the text it sits
// next to, so it works in a button, a label or a line of prose without being
// told anything.
</script>

<template>
  <span class="spinner" aria-hidden="true" />
</template>

<style scoped>
.spinner {
  display: inline-block;
  width: 0.85em;
  height: 0.85em;
  margin-right: 0.45rem;
  border: 2px solid currentColor;
  /* The gap is what makes the rotation visible at all. */
  border-top-color: transparent;
  border-radius: 50%;
  /* Optically centred on the text next to it, which sits on the baseline. */
  vertical-align: -0.1em;
  animation: spin 0.7s linear infinite;
}

@keyframes spin {
  to {
    transform: rotate(360deg);
  }
}

/* A still spinner would say nothing at all, so this slows the motion down
   rather than removing it. */
@media (prefers-reduced-motion: reduce) {
  .spinner {
    animation-duration: 2.5s;
  }
}
</style>
