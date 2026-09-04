<script setup lang="ts">
import ConfirmDialog from '../components/dialogs/ConfirmDialog.vue'
import { useLogs } from '../composables/useLogs'

const state = useLogs()
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
      <span>{{ state.logs.value.length }} visible</span>
    </div>
    <div class="log-console" aria-live="polite">
      <div v-for="(entry, index) in state.logs.value" :key="`${entry.time}-${index}`" class="log-line">
        <time>{{ new Date(entry.time).toLocaleTimeString() }}</time><span :class="`source-${entry.source}`">{{ entry.source }}</span><p>{{ entry.message }}</p>
      </div>
      <div v-if="state.logs.value.length === 0" class="empty-row">No matching log entries.</div>
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
