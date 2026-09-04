<script setup lang="ts">
import { Copy } from '@lucide/vue'
import { useClientSetup } from '../composables/useClientSetup'
import { copyText } from '../utils'

const state = useClientSetup()
</script>

<template>
  <section class="section setup-section">
    <div class="section-heading">
      <div><div class="eyebrow">SENSITIVE OUTPUT</div><h1>Client setup</h1><p>Replace the key placeholder with a full local API-key secret before use.</p></div>
      <button class="button" @click="copyText(state.text.value)"><Copy :size="16" />Copy config</button>
    </div>
    <div v-if="state.error.value" class="notice fault">{{ state.error.value }}</div>
    <div class="provider-tabs setup-tabs">
      <button :class="{ active: state.tab.value === 'codex' }" @click="state.tab.value = 'codex'">Codex CLI</button>
      <button :class="{ active: state.tab.value === 'claude' }" @click="state.tab.value = 'claude'">Claude Code</button>
    </div>
    <div class="sensitive-banner">Treat this configuration as sensitive. It identifies your local endpoint and API-key prefix.</div>
    <pre class="config-block">{{ state.text.value }}</pre>
  </section>
</template>
