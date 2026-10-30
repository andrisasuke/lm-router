<script setup lang="ts">
import { GripVertical } from '@lucide/vue'
import { onMounted, onUnmounted, ref } from 'vue'
import type { Connection, ConnectionQuota, ConnectionQuotaPart } from '../../types'
import { formatQuotaReset } from '../../utils'

defineProps<{
  connections: Connection[]
  selectedID: string
  busy: boolean
  chainActive: (index: number) => boolean
}>()

const emit = defineEmits<{
  select: [id: string]
  move: [id: string, delta: number]
  drop: [sourceID: string, targetID: string]
}>()

const draggedID = ref('')
const quotaClock = ref(Date.now())
let quotaClockTimer: number | undefined

const emptyQuota: ConnectionQuota = {
  state: 'unknown',
  summary: '',
  fetchedAt: '',
  windows: [],
}

onMounted(() => {
  quotaClockTimer = window.setInterval(() => {
    quotaClock.value = Date.now()
  }, 60_000)
})

onUnmounted(() => {
  if (quotaClockTimer !== undefined) window.clearInterval(quotaClockTimer)
})

function dropOn(targetID: string) {
  emit('drop', draggedID.value, targetID)
  draggedID.value = ''
}

function quotaFor(connection: Connection): ConnectionQuota {
  const quota = connection.quota
  return quota !== null && typeof quota === 'object' && !Array.isArray(quota) ? quota : emptyQuota
}

function quotaWindows(connection: Connection): ConnectionQuotaPart[] {
  const windows = quotaFor(connection).windows
  return Array.isArray(windows)
    ? windows.filter((window): window is ConnectionQuotaPart => window !== null && typeof window === 'object' && !Array.isArray(window))
    : []
}
</script>

<template>
  <div class="connection-list" aria-label="Connections in priority order">
    <div v-if="connections.length === 0" class="empty-state">
      <span class="empty-glyph">○</span>
      <h2>No connections</h2>
      <p>Add a connection to start this provider pool.</p>
    </div>
    <button
      v-for="(connection, index) in connections"
      :key="connection.id"
      :data-connection-id="connection.id"
      class="connection-card"
      :class="[
        `status-${connection.status.toLowerCase().replaceAll(' ', '-').replace('re-auth', 'reauth')}`,
        { selected: selectedID === connection.id, requesting: connection.requesting, 'has-card-quota': connection.provider === 'openai-codex', 'chain-active': chainActive(index), 'chain-first': index === 0, 'chain-last': index === connections.length - 1 },
      ]"
      draggable="true"
      :disabled="busy"
      @click="$emit('select', connection.id)"
      @dragstart="draggedID = connection.id"
      @dragover.prevent
      @drop.prevent="dropOn(connection.id)"
      @keydown.shift.up.prevent="$emit('move', connection.id, -1)"
      @keydown.shift.down.prevent="$emit('move', connection.id, 1)"
    >
      <span class="priority">{{ String(index + 1).padStart(2, '0') }}</span>
      <span class="channel-lamp" aria-hidden="true"></span>
      <span class="channel-copy">
        <strong :title="connection.name">{{ connection.name }}</strong>
        <small v-if="connection.provider === 'custom'">{{ connection.prefix }}/ · {{ connection.compatType }}</small>
        <small v-else>{{ connection.provider.replaceAll('-', '+') }}</small>
      </span>
      <span
        v-if="connection.provider === 'openai-codex'"
        class="channel-quota"
        :class="`quota-${quotaFor(connection).state}`"
        :title="quotaFor(connection).fetchedAt ? `Updated ${quotaFor(connection).fetchedAt}` : ''"
      >
        <template v-if="quotaWindows(connection).length">
          <span v-for="window in quotaWindows(connection)" :key="window.label" class="quota-window">
            <span :class="{ 'quota-critical': window.critical }">{{ window.label }}: {{ Math.round(window.utilization) }}%</span>
            <span class="quota-reset"> - {{ formatQuotaReset(window.resetsAt, quotaClock) }}</span>
          </span>
          <span v-if="quotaFor(connection).state === 'error'" class="quota-stale">{{ quotaFor(connection).summary }}</span>
        </template>
        <template v-else>{{ quotaFor(connection).summary || 'Loading quota...' }}</template>
      </span>
      <span class="channel-status">{{ connection.status }}</span>
      <GripVertical class="drag-handle" :size="17" :stroke-width="2" aria-hidden="true" />
    </button>
  </div>
</template>
