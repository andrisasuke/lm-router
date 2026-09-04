<script setup lang="ts">
import { Power, ServerOff } from '@lucide/vue'
import type { ServerStatus } from '../../types'
import { shortEndpoint } from '../../utils'

defineProps<{
  server: ServerStatus
  busy: boolean
}>()

defineEmits<{ toggle: [] }>()
</script>

<template>
  <header class="topbar">
    <div class="brand" aria-label="LM Router">
      <span class="brand-mark" aria-hidden="true"><i></i><i></i></span>
      <div>
        <div class="wordmark">LM ROUTER</div>
        <div class="tagline">local model switchboard</div>
      </div>
    </div>

    <div class="server-cluster" :class="`server-${server.state.toLowerCase()}`">
      <div class="server-readout">
        <span class="status-lamp" aria-hidden="true"></span>
        <div>
          <div class="server-state">{{ server.state === 'ON' ? 'Live' : server.state.toLowerCase() }}</div>
          <div class="server-endpoint">{{ shortEndpoint(server.actualEndpoint || server.configuredEndpoint) }}</div>
          <div v-if="server.actualEndpoint && server.actualEndpoint !== server.configuredEndpoint" class="server-actual">
            configured {{ shortEndpoint(server.configuredEndpoint) }}
          </div>
        </div>
      </div>
      <button class="button primary server-button" :disabled="busy || server.state === 'STARTING'" @click="$emit('toggle')">
        <ServerOff v-if="server.state === 'ON'" :size="17" :stroke-width="1.8" />
        <Power v-else :size="17" :stroke-width="1.8" />
        {{ server.state === 'ON' ? 'Stop Server' : 'Start Server' }}
      </button>
    </div>
  </header>
</template>
