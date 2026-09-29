import { Events } from '@wailsio/runtime'
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import {
  AddCustomProvider,
  AwaitOAuth,
  BeginOAuth,
  BeginReauth,
  CancelOAuth,
  DeleteConnection,
  GetSettings,
  ListConnections,
  Quota,
  RefreshConnection,
  RenameConnection,
  Reorder,
  SetEnabled,
  SubmitCallback,
  TestConnection,
  UpdateCustomProvider,
} from '../backend'
import { providers } from '../constants'
import type {
  Connection,
  ConnectionActivity,
  ConnectionQuota,
  ConnectionQuotaEvent,
  CustomProviderInput,
  OAuthSession,
  Provider,
  QuotaResult,
  Settings,
  TestResult,
} from '../types'
import { errorText } from '../utils'
import { useConfirmDialog } from './useConfirmDialog'

export function useConnections() {
  const activeProvider = ref<Provider>('openai-codex')
  const connections = ref<Connection[]>([])
  const selectedID = ref('')
  const busy = ref(false)
  const error = ref('')
  const resultTitle = ref('')
  const testResult = ref<TestResult | null>(null)
  const quotaResult = ref<QuotaResult | null>(null)
  const quotaBusyIDs = ref(new Set<string>())
  const confirmDialog = useConfirmDialog()
  const requestingByID = new Map<string, boolean>()
  const quotaByID = new Map<string, ConnectionQuota>()
  let stopActivityListener: (() => void) | undefined
  let stopQuotaListener: (() => void) | undefined

  const oauthOpen = ref(false)
  const oauthStage = ref<'prepare' | 'waiting'>('prepare')
  const oauthSession = ref<OAuthSession | null>(null)
  const oauthName = ref('')
  const oauthCallback = ref('')
  const oauthBusy = ref(false)
  const oauthMessage = ref('')
  const oauthReauthID = ref('')
  const riskOpen = ref(false)
  const riskReauthID = ref('')

  const customOpen = ref(false)
  const customConnection = ref<Connection | null>(null)
  const customBusy = ref(false)
  const customError = ref('')

  const selectedConnection = computed(() => connections.value.find((connection) => connection.id === selectedID.value) ?? null)
  const providerInfo = computed(() => providers.find((provider) => provider.id === activeProvider.value)!)
  const detailBusy = computed(() => busy.value || quotaBusyIDs.value.has(selectedID.value))

  function isConnectionStatus(value: unknown): value is Connection['status'] {
    return value === 'Active' || value === 'Cooldown' || value === 'Needs re-auth' || value === 'Disabled'
  }

  watch(selectedConnection, () => {
    testResult.value = null
    quotaResult.value = null
    resultTitle.value = ''
  })

  watch(activeProvider, async () => {
    selectedID.value = ''
    await load()
  })

  async function load() {
    error.value = ''
    try {
      const loaded = await ListConnections(activeProvider.value) as unknown as Connection[]
      connections.value = loaded.map((connection) => ({
        ...connection,
        requesting: requestingByID.get(connection.id) ?? connection.requesting,
        quota: quotaByID.get(connection.id) ?? connection.quota,
      }))
      if (!connections.value.some((connection) => connection.id === selectedID.value)) {
        selectedID.value = connections.value[0]?.id ?? ''
      }
    } catch (cause) {
      error.value = errorText(cause)
    }
  }

  function chainActive(index: number) {
    return connections.value.slice(0, index + 1).every((connection) => connection.routable)
  }

  async function move(id: string, delta: number) {
    busy.value = true
    error.value = ''
    try {
      await Reorder(id, delta)
      await load()
      await nextTick()
      document.querySelector<HTMLElement>(`[data-connection-id="${id}"]`)?.focus()
    } catch (cause) {
      error.value = errorText(cause)
    } finally {
      busy.value = false
    }
  }

  async function drop(sourceID: string, targetID: string) {
    const source = connections.value.findIndex((connection) => connection.id === sourceID)
    const target = connections.value.findIndex((connection) => connection.id === targetID)
    if (!sourceID || source < 0 || target < 0 || source === target) return
    busy.value = true
    error.value = ''
    try {
      const delta = target > source ? 1 : -1
      for (let step = 0; step < Math.abs(target - source); step++) await Reorder(sourceID, delta)
      selectedID.value = sourceID
      await load()
    } catch (cause) {
      error.value = errorText(cause)
    } finally {
      busy.value = false
    }
  }

  async function saveAlias(name: string) {
    const connection = selectedConnection.value
    if (!connection) return
    busy.value = true
    error.value = ''
    try {
      await RenameConnection(connection.id, name)
      await load()
    } catch (cause) {
      error.value = errorText(cause)
    } finally {
      busy.value = false
    }
  }

  async function toggleEnabled() {
    const connection = selectedConnection.value
    if (!connection) return
    busy.value = true
    error.value = ''
    try {
      await SetEnabled(connection.id, !connection.enabled)
      await load()
    } catch (cause) {
      error.value = errorText(cause)
    } finally {
      busy.value = false
    }
  }

  async function test() {
    const connection = selectedConnection.value
    if (!connection) return
    busy.value = true
    error.value = ''
    testResult.value = null
    quotaResult.value = null
    resultTitle.value = 'Connection test'
    try {
      const settings = await GetSettings() as unknown as Settings
      testResult.value = await TestConnection(connection.id, settings.defaultModel) as unknown as TestResult
    } catch (cause) {
      error.value = errorText(cause)
    } finally {
      busy.value = false
    }
  }

  async function showQuota() {
    const connection = selectedConnection.value
    if (!connection) return
    const cardOnly = connection.provider === 'openai-codex'
    quotaBusyIDs.value = new Set(quotaBusyIDs.value).add(connection.id)
    error.value = ''
    testResult.value = null
    quotaResult.value = null
    resultTitle.value = cardOnly ? '' : 'Quota'
    try {
      const result = await Quota(connection.id) as unknown as QuotaResult
      if (!cardOnly) quotaResult.value = result
    } catch (cause) {
      error.value = errorText(cause)
    } finally {
      const remaining = new Set(quotaBusyIDs.value)
      remaining.delete(connection.id)
      quotaBusyIDs.value = remaining
    }
  }

  async function refresh() {
    const connection = selectedConnection.value
    if (!connection) return
    busy.value = true
    error.value = ''
    try {
      await RefreshConnection(connection.id)
      await load()
    } catch (cause) {
      error.value = errorText(cause)
    } finally {
      busy.value = false
    }
  }

  function requestDelete() {
    const connection = selectedConnection.value
    if (!connection) return
    confirmDialog.request(
      'Delete connection?',
      `${connection.name} will be removed from the ${providerInfo.value.label} failover pool. This cannot be undone.`,
      async () => {
        await DeleteConnection(connection.id)
        selectedID.value = ''
        await load()
      },
    )
  }

  function requestOAuth(reauthID = '') {
    if (activeProvider.value === 'anthropic-claude') {
      riskReauthID.value = reauthID
      riskOpen.value = true
      return
    }
    prepareOAuth(reauthID)
  }

  function prepareOAuth(reauthID = '') {
    oauthReauthID.value = reauthID
    oauthName.value = reauthID ? selectedConnection.value?.name ?? '' : ''
    oauthCallback.value = ''
    oauthMessage.value = ''
    oauthSession.value = null
    oauthStage.value = 'prepare'
    oauthOpen.value = true
  }

  function acceptClaudeRisk() {
    riskOpen.value = false
    prepareOAuth(riskReauthID.value)
  }

  async function launchOAuth() {
    oauthBusy.value = true
    oauthMessage.value = ''
    try {
      oauthSession.value = (oauthReauthID.value
        ? await BeginReauth(oauthReauthID.value)
        : await BeginOAuth(activeProvider.value)) as unknown as OAuthSession
      oauthStage.value = 'waiting'
      if (oauthSession.value.loopback) {
        void awaitLoopback(oauthSession.value.id, oauthName.value)
      } else if (oauthSession.value.loopbackError) {
        oauthMessage.value = oauthSession.value.loopbackError
      }
    } catch (cause) {
      oauthMessage.value = errorText(cause)
    } finally {
      oauthBusy.value = false
    }
  }

  async function awaitLoopback(sessionID: string, name: string) {
    try {
      await AwaitOAuth(sessionID, name)
      oauthOpen.value = false
      await load()
    } catch (cause) {
      if (oauthOpen.value) oauthMessage.value = errorText(cause)
    }
  }

  async function submitOAuthCallback() {
    if (!oauthSession.value) return
    oauthBusy.value = true
    try {
      await SubmitCallback(oauthSession.value.id, oauthName.value, oauthCallback.value)
      oauthOpen.value = false
      await load()
    } catch (cause) {
      oauthMessage.value = errorText(cause)
    } finally {
      oauthBusy.value = false
    }
  }

  async function closeOAuth() {
    const sessionID = oauthSession.value?.id
    oauthOpen.value = false
    oauthSession.value = null
    if (sessionID) await CancelOAuth(sessionID)
  }

  function openCustom(connection?: Connection) {
    customConnection.value = connection ?? null
    customError.value = ''
    customOpen.value = true
  }

  async function saveCustom(input: CustomProviderInput) {
    customBusy.value = true
    customError.value = ''
    try {
      if (customConnection.value) {
        await UpdateCustomProvider(customConnection.value.id, input)
      } else {
        await AddCustomProvider(input)
      }
      customOpen.value = false
      await load()
    } catch (cause) {
      customError.value = errorText(cause)
    } finally {
      customBusy.value = false
    }
  }

  onMounted(() => {
    stopActivityListener = Events.On('connection-activity', (event) => {
      const activity = event.data as ConnectionActivity
      if (!activity || typeof activity.id !== 'string' || typeof activity.requesting !== 'boolean') return
      requestingByID.set(activity.id, activity.requesting)
      const connection = connections.value.find((item) => item.id === activity.id)
      if (connection) connection.requesting = activity.requesting
    })
    stopQuotaListener = Events.On('connection-quota', (event) => {
      const update = event.data as ConnectionQuotaEvent
      if (!update || typeof update.id !== 'string' || !update.quota || typeof update.quota.summary !== 'string') return
      quotaByID.set(update.id, update.quota)
      const connection = connections.value.find((item) => item.id === update.id)
      if (connection) {
        connection.quota = update.quota
        if (isConnectionStatus(update.status)) connection.status = update.status
        if (typeof update.cooldownUntil === 'string') connection.cooldownUntil = update.cooldownUntil
        if (typeof update.consecutiveFailures === 'number') connection.consecutiveFailures = update.consecutiveFailures
      }
    })
    void load()
  })

  onUnmounted(() => {
    stopActivityListener?.()
    stopQuotaListener?.()
  })

  return {
    activeProvider, connections, selectedID, selectedConnection, providerInfo,
    busy, detailBusy, error, resultTitle, testResult, quotaResult, confirmDialog,
    oauthOpen, oauthStage, oauthSession, oauthName, oauthCallback, oauthBusy,
    oauthMessage, oauthReauthID, riskOpen, customOpen, customConnection,
    customBusy, customError,
    chainActive, move, drop, saveAlias, toggleEnabled, test, showQuota, refresh,
    requestDelete, requestOAuth, acceptClaudeRisk, launchOAuth, submitOAuthCallback,
    closeOAuth, openCustom, saveCustom,
  }
}
