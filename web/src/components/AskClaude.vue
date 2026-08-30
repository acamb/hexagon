<script setup lang="ts">
// One editor's ask-Claude control: the box, its pending state, the summary of
// what changed and the undo that puts the previous text back.
//
// It is a component rather than markup in the page because advanced mode has
// two editors, so the page carries three of these: the simple Dockerfile, the
// advanced Dockerfile and the advanced compose file. Each keeps its own
// instruction, its own summary and its own undo buffer, which is exactly the
// state a component holds.
import { ref } from 'vue'
import Spinner from './Spinner.vue'
import { ApiError, api, type SourceKind } from '../api'

const props = defineProps<{
  kind: SourceKind
  // What to ask for, shown in the box: "add the Go toolchain", "add a redis".
  placeholder: string
}>()

// The file being edited. Claude Code answers with the whole of it, so the
// control writes the answer straight back through the model.
const content = defineModel<string>({ required: true })

const emit = defineEmits<{ failed: [message: string] }>()

const instruction = ref('')
const asking = ref(false)
const summary = ref('')
// What the answer replaced, and what undo puts back. Null means there is
// nothing to undo: either nothing was asked, or the undo was already used.
const previous = ref<string | null>(null)

async function ask() {
  const said = instruction.value.trim()
  if (!said || asking.value) return
  asking.value = true
  try {
    const edit = await api.images.editSource(props.kind, content.value, said)
    previous.value = content.value
    content.value = edit.content
    summary.value = edit.summary
    instruction.value = ''
  } catch (e) {
    emit('failed', message(e))
  } finally {
    asking.value = false
  }
}

function undo() {
  if (previous.value === null) return
  content.value = previous.value
  previous.value = null
  summary.value = ''
}

function message(e: unknown): string {
  if (e instanceof ApiError) return e.message
  return e instanceof Error ? e.message : String(e)
}
</script>

<template>
  <div class="ask">
    <input
      v-model="instruction"
      :disabled="asking"
      :placeholder="placeholder"
      @keydown.enter.prevent="ask"
    />
    <button type="button" :disabled="asking || !instruction.trim()" @click="ask">
      <Spinner v-if="asking" />{{ asking ? 'Asking…' : 'Ask Claude' }}
    </button>
  </div>

  <p v-if="summary" class="summary">
    {{ summary }}
    <button v-if="previous !== null" type="button" class="link" @click="undo">undo</button>
  </p>
</template>

<style scoped>
.ask {
  display: flex;
  gap: 0.5rem;
}

.ask input {
  flex: 1;
  min-width: 0;
  padding: 0.5rem 0.6rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: var(--bg);
  color: var(--text);
  font: inherit;
}

button {
  padding: 0.5rem 0.9rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: var(--bg);
  color: var(--text);
  font: inherit;
  cursor: pointer;
}

button:hover:not(:disabled) {
  border-color: var(--accent);
}

button:disabled {
  opacity: 0.5;
  cursor: default;
}

.summary {
  margin: 0;
  color: var(--text-muted);
  font-size: 0.9rem;
}

.link {
  margin-left: 0.4rem;
  padding: 0;
  border: none;
  background: none;
  color: var(--accent);
  font: inherit;
  font-size: 0.9rem;
  cursor: pointer;
}
</style>
