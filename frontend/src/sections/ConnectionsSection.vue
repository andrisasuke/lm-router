<script setup lang="ts">
import ConnectionDetail from '../components/connections/ConnectionDetail.vue'
import ConnectionList from '../components/connections/ConnectionList.vue'
import ClaudeRiskDialog from '../components/dialogs/ClaudeRiskDialog.vue'
import ConfirmDialog from '../components/dialogs/ConfirmDialog.vue'
import CustomProviderDialog from '../components/dialogs/CustomProviderDialog.vue'
import OAuthDialog from '../components/dialogs/OAuthDialog.vue'
import { useConnections } from '../composables/useConnections'
import { providers } from '../constants'

const state = useConnections()
</script>

<template>
  <section class="section connections-section">
    <div class="section-heading">
      <div>
        <div class="eyebrow">CONNECTIONS · {{ state.providerInfo.value.label.toUpperCase() }}</div>
        <h1>Failover priority</h1>
        <p>Routes <code>{{ state.providerInfo.value.route }}</code>. Shift+↑/↓ or drag a channel to reorder it.</p>
      </div>
      <button v-if="state.activeProvider.value === 'custom'" class="button add-connection-button" @click="state.openCustom()">Add custom</button>
      <button v-else class="button add-connection-button" @click="state.requestOAuth()">Add connection</button>
    </div>

    <div class="provider-tabs" role="tablist" aria-label="Provider type">
      <button
        v-for="provider in providers"
        :key="provider.id"
        role="tab"
        :aria-selected="state.activeProvider.value === provider.id"
        :class="{ active: state.activeProvider.value === provider.id }"
        @click="state.activeProvider.value = provider.id"
      >{{ provider.label }}</button>
    </div>

    <div v-if="state.error.value || state.confirmDialog.error.value" class="notice fault">{{ state.error.value || state.confirmDialog.error.value }}</div>

    <div class="connections-layout">
      <ConnectionList
        :connections="state.connections.value"
        :selected-i-d="state.selectedID.value"
        :busy="state.busy.value"
        :chain-active="state.chainActive"
        @select="state.selectedID.value = $event"
        @move="state.move"
        @drop="state.drop"
      />

      <ConnectionDetail
        v-if="state.selectedConnection.value"
        :connection="state.selectedConnection.value"
        :index="state.connections.value.findIndex(connection => connection.id === state.selectedConnection.value?.id)"
        :busy="state.busy.value"
        :result-title="state.resultTitle.value"
        :test-result="state.testResult.value"
        :quota-result="state.quotaResult.value"
        @save-alias="state.saveAlias"
        @edit-custom="state.openCustom(state.selectedConnection.value ?? undefined)"
        @test="state.test"
        @quota="state.showQuota"
        @refresh="state.refresh"
        @reauth="state.requestOAuth(state.selectedConnection.value?.id)"
        @toggle="state.toggleEnabled"
        @delete="state.requestDelete"
      />
    </div>
  </section>

  <ClaudeRiskDialog v-if="state.riskOpen.value" @close="state.riskOpen.value = false" @accept="state.acceptClaudeRisk" />

  <OAuthDialog
    v-if="state.oauthOpen.value"
    :provider-label="state.providerInfo.value.label"
    :reauth="!!state.oauthReauthID.value"
    :stage="state.oauthStage.value"
    :session="state.oauthSession.value"
    :name="state.oauthName.value"
    :callback="state.oauthCallback.value"
    :busy="state.oauthBusy.value"
    :message="state.oauthMessage.value"
    @update:name="state.oauthName.value = $event"
    @update:callback="state.oauthCallback.value = $event"
    @close="state.closeOAuth"
    @launch="state.launchOAuth"
    @submit="state.submitOAuthCallback"
  />

  <CustomProviderDialog
    v-if="state.customOpen.value"
    :connection="state.customConnection.value"
    :busy="state.customBusy.value"
    :error="state.customError.value"
    @close="state.customOpen.value = false"
    @save="state.saveCustom"
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
