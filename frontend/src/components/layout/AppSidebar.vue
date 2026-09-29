<script setup lang="ts">
import { Globe2, KeyRound, MonitorSmartphone, ScrollText, Settings } from '@lucide/vue'
import type { Component } from 'vue'
import type { Section } from '../../types'

defineProps<{
  activeSection: Section
  version: string
}>()

defineEmits<{ select: [section: Section] }>()

const sections: Array<{ id: Section; label: string; hint: string; icon: Component }> = [
  { id: 'connections', label: 'Connections', hint: 'Provider pools', icon: Globe2 },
  { id: 'keys', label: 'API Keys', hint: 'Local access', icon: KeyRound },
  { id: 'settings', label: 'Settings', hint: 'Router behavior', icon: Settings },
  { id: 'logs', label: 'Logs', hint: 'Recent activity', icon: ScrollText },
  { id: 'setup', label: 'Client Setup', hint: 'Codex + Claude', icon: MonitorSmartphone },
]
</script>

<template>
  <nav class="section-rail" aria-label="Main sections">
    <button
      v-for="item in sections"
      :key="item.id"
      class="section-link"
      :class="{ active: activeSection === item.id }"
      @click="$emit('select', item.id)"
    >
      <component :is="item.icon" class="section-icon" :size="18" :stroke-width="1.6" />
      <span class="section-copy"><strong>{{ item.label }}</strong><small>{{ item.hint }}</small></span>
    </button>
    <div class="rail-footer">
      <span class="eyebrow">BUILD</span>
      <span>{{ version }}</span>
    </div>
  </nav>
</template>
