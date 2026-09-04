<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { System } from '@wailsio/runtime'
import { Version } from './backend'
import AppHeader from './components/layout/AppHeader.vue'
import AppSidebar from './components/layout/AppSidebar.vue'
import { useServer } from './composables/useServer'
import ApiKeysSection from './sections/ApiKeysSection.vue'
import ClientSetupSection from './sections/ClientSetupSection.vue'
import ConnectionsSection from './sections/ConnectionsSection.vue'
import LogsSection from './sections/LogsSection.vue'
import SettingsSection from './sections/SettingsSection.vue'
import type { Section } from './types'

const activeSection = ref<Section>('connections')
const version = ref('')
const server = useServer()
const hasMacTitlebar = System.IsMac()

onMounted(async () => {
  version.value = await Version()
})
</script>

<template>
  <div class="app-shell" :class="{ 'mac-titlebar': hasMacTitlebar }">
    <AppHeader :server="server.server.value" :busy="server.busy.value" @toggle="server.toggle" />

    <div class="body-grid">
      <AppSidebar :active-section="activeSection" :version="version" @select="activeSection = $event" />

      <main class="workspace">
        <div v-if="server.error.value" class="notice fault" role="alert">
          <span>{{ server.error.value }}</span>
          <button aria-label="Dismiss error" @click="server.error.value = ''">×</button>
        </div>

        <ConnectionsSection v-if="activeSection === 'connections'" />
        <ApiKeysSection v-else-if="activeSection === 'keys'" />
        <SettingsSection v-else-if="activeSection === 'settings'" @saved="server.refresh" />
        <LogsSection v-else-if="activeSection === 'logs'" />
        <ClientSetupSection v-else />
      </main>
    </div>
  </div>
</template>
