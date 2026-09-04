<script setup lang="ts">
import {
  Ban,
  FlaskConical,
  Gauge,
  LockKeyhole,
  Pencil,
  Power,
  RefreshCw,
  SlidersHorizontal,
  Trash2,
} from '@lucide/vue'
import { nextTick, ref, watch } from 'vue'
import type { Connection, QuotaResult, TestResult } from '../../types'
import { formatDate } from '../../utils'

const props = defineProps<{
  connection: Connection
  index: number
  busy: boolean
  resultTitle: string
  testResult: TestResult | null
  quotaResult: QuotaResult | null
}>()

const emit = defineEmits<{
  'save-alias': [name: string]
  'edit-custom': []
  test: []
  quota: []
  refresh: []
  reauth: []
  toggle: []
  delete: []
}>()

const editAlias = ref(false)
const alias = ref(props.connection.name)
const resultPanel = ref<HTMLElement | null>(null)

watch(() => props.connection.id, () => {
  alias.value = props.connection.name
  editAlias.value = false
})

watch(
  () => [props.testResult, props.quotaResult] as const,
  async ([testResult, quotaResult]) => {
    if (!testResult && !quotaResult) return
    await nextTick()
    resultPanel.value?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  },
)

function saveAlias() {
  emit('save-alias', alias.value)
  editAlias.value = false
}
</script>

<template>
  <aside class="detail-panel">
    <div class="detail-heading">
      <div>
        <div class="eyebrow">CHANNEL {{ String(index + 1).padStart(2, '0') }} <span class="eyebrow-dot">•</span></div>
        <h2>{{ connection.name }}</h2>
      </div>
      <span class="status-pill" :class="`pill-${connection.status.toLowerCase().replaceAll(' ', '-')}`">{{ connection.status }}</span>
    </div>

    <dl class="detail-data">
      <div><dt>Account ID</dt><dd>{{ connection.id }}</dd></div>
      <div><dt>Priority</dt><dd>{{ connection.priority }}</dd></div>
      <div v-if="connection.prefix"><dt>Model prefix</dt><dd>{{ connection.prefix }}/</dd></div>
      <div v-if="connection.baseUrl"><dt>Base URL</dt><dd>{{ connection.baseUrl }}</dd></div>
      <div v-if="connection.apiType"><dt>Wire API</dt><dd>{{ connection.apiType }}</dd></div>
      <div><dt>Failures</dt><dd>{{ connection.consecutiveFailures }}</dd></div>
      <div v-if="connection.status === 'Cooldown' && connection.cooldownUntil"><dt>Cooldown</dt><dd>{{ formatDate(connection.cooldownUntil) }}</dd></div>
    </dl>

    <form v-if="editAlias" class="inline-form" @submit.prevent="saveAlias">
      <label for="alias">Connection alias</label>
      <div><input id="alias" v-model="alias" autofocus autocapitalize="off" autocorrect="off" spellcheck="false" /><button class="button small primary" :disabled="busy">Save</button></div>
    </form>

    <div class="action-grid">
      <button class="button" @click="editAlias = !editAlias"><Pencil :size="16" />Edit alias</button>
      <button v-if="connection.provider === 'custom'" class="button" @click="$emit('edit-custom')"><SlidersHorizontal :size="16" />Edit endpoint</button>
      <button class="button" :disabled="busy" @click="$emit('test')"><FlaskConical :size="16" />Test</button>
      <button v-if="connection.canQuota" class="button" :disabled="busy" @click="$emit('quota')"><Gauge :size="17" />Quota</button>
      <button v-if="connection.canRefresh" class="button" :disabled="busy" @click="$emit('refresh')"><RefreshCw :size="17" />Refresh token</button>
      <button v-if="connection.canReauth" class="button" @click="$emit('reauth')"><LockKeyhole :size="16" />Re-authenticate</button>
      <button class="button" :class="{ warning: connection.enabled }" :disabled="busy" @click="$emit('toggle')">
        <Ban v-if="connection.enabled" :size="17" /><Power v-else :size="17" />
        {{ connection.enabled ? 'Disable' : 'Enable' }}
      </button>
      <button class="button danger" @click="$emit('delete')"><Trash2 :size="16" />Delete</button>
    </div>

    <div v-if="resultTitle" ref="resultPanel" class="result-panel">
      <div class="eyebrow">{{ resultTitle.toUpperCase() }}</div>
      <template v-if="testResult">
        <div class="result-status" :class="testResult.ok ? 'ok' : 'bad'">HTTP {{ testResult.status }} · {{ testResult.ok ? 'CONNECTED' : 'FAILED' }}</div>
        <pre>{{ testResult.output }}</pre>
      </template>
      <template v-if="quotaResult">
        <p v-if="quotaResult.message">{{ quotaResult.message }}</p>
        <div v-for="window in quotaResult.windows" :key="window.name" class="quota-row">
          <div><span>{{ window.name }}</span><strong>{{ Math.round(window.utilization) }}%</strong></div>
          <div class="meter"><span :style="{ width: `${Math.min(100, window.utilization)}%` }"></span></div>
          <small v-if="window.resetsAt">Resets {{ formatDate(window.resetsAt) }}</small>
          <small v-else-if="window.summary">{{ window.summary }}</small>
        </div>
      </template>
    </div>
  </aside>
</template>
