<script setup lang="ts">
import { Check, Copy } from '@lucide/vue'
import { onBeforeUnmount, ref } from 'vue'
import ConfirmDialog from '../components/dialogs/ConfirmDialog.vue'
import { useLogs, type LogRow } from '../composables/useLogs'
import { copyText } from '../utils'

const state = useLogs()
const copiedRows = ref(new Set<string>())
const copiedResetTimers = new Map<string, number>()

function isCopied(key: string) {
  return copiedRows.value.has(key)
}

async function copyLogEntry(row: LogRow) {
  try {
    const value = await state.clipboardValue(row)
    await copyText(value)
  } catch {
    return
  }

  const key = row.key
  copiedRows.value = new Set(copiedRows.value).add(key)
  const existingTimer = copiedResetTimers.get(key)
  if (existingTimer !== undefined) window.clearTimeout(existingTimer)
  copiedResetTimers.set(key, window.setTimeout(() => {
    const nextCopiedRows = new Set(copiedRows.value)
    nextCopiedRows.delete(key)
    copiedRows.value = nextCopiedRows
    copiedResetTimers.delete(key)
  }, 2500))
}

onBeforeUnmount(() => {
  for (const timer of copiedResetTimers.values()) window.clearTimeout(timer)
  copiedResetTimers.clear()
})
</script>

<template>
  <section class="section logs-section">
    <div class="section-heading">
      <div><div class="eyebrow">RING LOGGER · 500 ENTRIES</div><h1>Live logs</h1><p>Authorization bearer values are redacted before they enter this view.</p></div>
      <button class="button danger" @click="state.requestClear">Clear logs</button>
    </div>
    <div v-if="state.error.value || state.confirmDialog.error.value" class="notice fault">{{ state.error.value || state.confirmDialog.error.value }}</div>
    <div class="log-toolbar">
      <button v-for="filter in ['all', 'proxy', 'openai'] as const" :key="filter" :class="{ active: state.filter.value === filter }" @click="state.filter.value = filter">{{ filter }}</button>
      <span role="status" aria-live="polite">{{ state.rows.value.length }} of {{ state.available.value }} loaded</span>
    </div>
    <div class="log-console">
      <button
        v-if="state.hasOlder.value"
        type="button"
        class="log-load-older"
        :disabled="state.loadingOlder.value"
        @click="state.loadOlder"
      >
        {{ state.loadingOlder.value ? 'Loading…' : 'Load older logs' }}
      </button>
      <div v-for="row in state.rows.value" :key="row.key" v-memo="[row.key, isCopied(row.key)]" class="log-line">
        <time>{{ row.time }}</time><span :class="row.sourceClass">{{ row.source }}</span><p>{{ row.message }}</p>
        <button
          type="button"
          class="log-copy-button"
          :class="{ copied: isCopied(row.key) }"
          :aria-label="isCopied(row.key) ? 'Copied log entry' : 'Copy log entry'"
          :title="isCopied(row.key) ? 'Copied log entry' : 'Copy log entry'"
          @click="copyLogEntry(row)"
        >
          <Check v-if="isCopied(row.key)" :size="14" />
          <Copy v-else :size="14" />
        </button>
      </div>
      <div v-if="state.rows.value.length === 0" class="empty-row">No matching log entries.</div>
    </div>
  </section>

  <ConfirmDialog
    v-if="state.confirmDialog.open.value"
    :title="state.confirmDialog.title.value"
    :message="state.confirmDialog.message.value"
    :busy="state.confirmDialog.busy.value"
    @close="state.confirmDialog.close"
    @confirm="state.confirmDialog.confirm"
  />
</template>
