<script setup lang="ts">
// A message about something that just happened, which stays on the screen until
// somebody dismisses it.
//
// It is a component rather than a paragraph in each view because of what it must
// *not* do. Every view here polls something, and a refresh that cleared the
// message on success took it away a second after it arrived — long enough to
// notice a coloured box, not long enough to read it. Nothing but a click removes
// one now, and having a single component for it is what keeps that true as views
// are added.
//
// It is for what a request answered, not for what a row says: an image that
// failed to build and a session that has gone carry their reason as part of
// their state, and that text belongs to the state rather than to a notice
// somebody can close.
defineProps<{
  // What happened. The two read the same and differ only in colour, because
  // they are the same kind of thing: the answer to something the user just did.
  kind: 'error' | 'success'
  message: string
}>()
defineEmits<{ dismiss: [] }>()
</script>

<template>
  <p class="notice" :class="kind" :role="kind === 'error' ? 'alert' : 'status'">
    <!-- The slot is for what can be done about the message, beside it: the
         "undo" under an answer from Claude Code is the one that needs it. -->
    <span>{{ message }}<slot /></span>
    <button type="button" class="dismiss" aria-label="Dismiss" @click="$emit('dismiss')">×</button>
  </p>
</template>

<style scoped>
.notice {
  display: flex;
  align-items: baseline;
  gap: 0.75rem;
  margin: 0;
  padding: 0.6rem 0.8rem;
  border: 1px solid currentColor;
  border-radius: 6px;
  /* A wash of the notice's own colour rather than a second token per kind: it
     is what makes this a box to look at instead of a line of coloured text, and
     at this strength it stays quiet in both themes. */
  background: color-mix(in srgb, currentColor 8%, transparent);
  /* The message can be a build log's last line or a daemon's complaint: it
     wraps, and it keeps the line breaks it was written with. */
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

.error {
  color: var(--error);
}

/* The green the running dot already uses: the palette's, and deliberately not a
   brighter one — this reports a small success, it does not celebrate. */
.success {
  color: var(--ok);
}

.notice span {
  flex: 1;
  min-width: 0;
}

.dismiss {
  flex: none;
  padding: 0;
  border: none;
  background: none;
  color: inherit;
  font-size: 1.25rem;
  line-height: 1;
  cursor: pointer;
  opacity: 0.7;
}

.dismiss:hover {
  opacity: 1;
}
</style>
