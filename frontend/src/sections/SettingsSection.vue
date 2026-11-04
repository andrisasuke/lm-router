<script setup lang="ts">
import { System } from '@wailsio/runtime'
import { useSettings } from '../composables/useSettings'

const emit = defineEmits<{ saved: [] }>()
const state = useSettings(() => emit('saved'))
const trayHelp = System.IsLinux()
  ? 'Show LM Router in the system tray. Changes apply after restarting on Linux.'
  : 'Show LM Router in the system tray.'
const quotaRecoveryIntervals = [
  { value: 5, label: '5s' },
  { value: 10, label: '10s' },
  { value: 15, label: '15s' },
  { value: 30, label: '30s' },
  { value: 60, label: '1m' },
  { value: 300, label: '5m' },
  { value: 900, label: '15m' },
  { value: 1200, label: '20m' },
]
</script>

<template>
  <section class="section settings-section">
    <div class="section-heading">
      <div><div class="eyebrow">ROUTER BEHAVIOR</div><h1>Settings</h1><p>Changes stay local until you explicitly save them.</p></div>
    </div>
    <div v-if="state.error.value" class="notice fault">{{ state.error.value }}</div>
    <form class="settings-form" @submit.prevent="state.save">
      <label><span>Listen host<small>Interface used by the local proxy.</small></span><input v-model="state.settings.value.host" autocapitalize="off" autocorrect="off" spellcheck="false" /></label>
      <label><span>Port<small>Use 0 to let the OS select a free port.</small></span><input v-model.number="state.settings.value.port" type="number" min="0" max="65535" /></label>
      <label><span>Default model<small>Used by connection tests and Codex setup.</small></span><input v-model="state.settings.value.defaultModel" autocapitalize="off" autocorrect="off" spellcheck="false" /></label>
      <label><span>Upstream timeout<small>Seconds before a provider request is cancelled.</small></span><input v-model.number="state.settings.value.upstreamTimeoutSeconds" type="number" min="1" /></label>
      <label><span>Log body limit<small>Maximum captured upstream body bytes.</small></span><input v-model.number="state.settings.value.logBodyLimit" type="number" min="1024" /></label>
      <label class="toggle-row"><span>Enable Codex quota recovery<small>Scan due Codex cooldowns automatically.</small></span><input v-model="state.settings.value.codexQuotaRecoveryEnabled" type="checkbox" /><i></i></label>
      <label><span>Codex recovery interval<small>Changes apply after saving settings.</small></span><select v-model.number="state.settings.value.codexQuotaRecoveryIntervalSeconds" :disabled="!state.settings.value.codexQuotaRecoveryEnabled"><option v-for="option in quotaRecoveryIntervals" :key="option.value" :value="option.value">{{ option.label }}</option></select></label>
      <label class="toggle-row"><span>Enable tray menu<small>{{ trayHelp }}</small></span><input v-model="state.settings.value.trayEnabled" type="checkbox" /><i></i></label>
      <label class="toggle-row"><span>Request logs<small>Record incoming proxy requests in the ring log.</small></span><input v-model="state.settings.value.logRequests" type="checkbox" /><i></i></label>
      <label class="toggle-row"><span>Upstream logs<small>Record provider request and response diagnostics.</small></span><input v-model="state.settings.value.logUpstream" type="checkbox" /><i></i></label>
      <div class="form-actions"><span v-if="state.saved.value" class="saved-state">Saved</span><button class="button primary" :disabled="state.busy.value">Save settings</button></div>
    </form>
  </section>
</template>
