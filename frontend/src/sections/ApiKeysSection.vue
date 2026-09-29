<script setup lang="ts">
import { Plus, Trash2 } from '@lucide/vue'
import ApiKeyDialog from '../components/dialogs/ApiKeyDialog.vue'
import ConfirmDialog from '../components/dialogs/ConfirmDialog.vue'
import { useApiKeys } from '../composables/useApiKeys'
import { formatDate } from '../utils'

const state = useApiKeys()
</script>

<template>
  <section class="section">
    <div class="section-heading">
      <div><div class="eyebrow">LOCAL AUTHENTICATION</div><h1>API keys</h1><p>Keys authenticate clients to this router, never to upstream providers.</p></div>
      <button class="button add-connection-button" @click="state.openCreate"><Plus :size="17" />Add key</button>
    </div>

    <div v-if="state.error.value || state.confirmDialog.error.value" class="notice fault">{{ state.error.value || state.confirmDialog.error.value }}</div>

    <div class="table-list">
      <div v-for="key in state.keys.value" :key="key.id" class="table-row">
        <div><strong>{{ key.name }}</strong><small>{{ key.id }}</small></div>
        <code>{{ key.prefix }}…</code>
        <time>{{ formatDate(key.createdAt) }}</time>
        <button class="button small danger" @click="state.requestDelete(key)"><Trash2 :size="15" />Revoke</button>
      </div>
      <div v-if="state.keys.value.length === 0" class="empty-row">No local API keys yet.</div>
    </div>
  </section>

  <ApiKeyDialog
    v-if="state.createOpen.value"
    :name="state.keyName.value"
    :created-key="state.createdKey.value"
    :busy="state.busy.value"
    :error="state.createError.value"
    @update:name="state.keyName.value = $event"
    @close="state.closeCreate"
    @create="state.create"
  />

  <ConfirmDialog
    v-if="state.confirmDialog.open.value"
    :title="state.confirmDialog.title.value"
    :message="state.confirmDialog.message.value"
    :busy="state.confirmDialog.busy.value"
    @close="state.confirmDialog.close"
    @confirm="state.confirmDialog.confirm"
  />
</template>
