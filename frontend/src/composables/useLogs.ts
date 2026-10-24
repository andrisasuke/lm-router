import { onActivated, onBeforeUnmount, onDeactivated, ref, watch } from 'vue'
import { ClearLogs, Logs } from '../backend'
import type { LogEntry } from '../types'
import { errorText } from '../utils'
import { useConfirmDialog } from './useConfirmDialog'

export interface LogRow {
  key: string
  time: string
  source: string
  sourceClass: string
  message: string
  clipboardValue: string
}

export function useLogs() {
  const logs = ref<LogEntry[]>([])
  const rows = ref<LogRow[]>([])
  const filter = ref<'all' | 'proxy' | 'openai'>('all')
  const error = ref('')
  const confirmDialog = useConfirmDialog()
  let timer: number | undefined
  let active = false
  let activeGeneration = 0
  let inFlight = false
  let pendingLoad = false
  let pendingResolvers: Array<() => void> = []
  let requestID = 0

  function clearTimer() {
    if (timer === undefined) return
    window.clearTimeout(timer)
    timer = undefined
  }

  function buildRows(snapshot: LogEntry[]): LogRow[] {
    return snapshot.map((entry) => {
      const time = new Date(entry.time).toLocaleTimeString()
      const source = entry.source.toUpperCase()
      return {
        key: JSON.stringify([entry.time, entry.source, entry.message]),
        time,
        source,
        sourceClass: `source-${entry.source}`,
        message: entry.message,
        clipboardValue: `${time} ${source} ${entry.message}`,
      }
    })
  }

  function sameSnapshot(previous: LogEntry[], next: LogEntry[]) {
    if (previous.length !== next.length) return false
    return previous.every((entry, index) => {
      const candidate = next[index]
      return entry.time === candidate.time && entry.source === candidate.source && entry.message === candidate.message
    })
  }

  function schedule(generation: number) {
    clearTimer()
    if (!active || generation !== activeGeneration) return
    timer = window.setTimeout(() => {
      timer = undefined
      queueLoad()
    }, 1000)
  }

  function resolvePendingLoads() {
    const resolvers = pendingResolvers
    pendingResolvers = []
    for (const resolve of resolvers) resolve()
  }

  async function load() {
    if (!active || inFlight) return
    inFlight = true
    pendingLoad = false
    const generation = activeGeneration
    const requestedFilter = filter.value
    const currentRequestID = ++requestID
    error.value = ''
    try {
      const snapshot = await Logs(requestedFilter) as unknown as LogEntry[]
      if (!active || generation !== activeGeneration || currentRequestID !== requestID || requestedFilter !== filter.value) return
      if (!sameSnapshot(logs.value, snapshot)) {
        logs.value = snapshot
        rows.value = buildRows(snapshot)
      }
    } catch (cause) {
      if (active && generation === activeGeneration && currentRequestID === requestID && requestedFilter === filter.value) {
        error.value = errorText(cause)
      }
    } finally {
      inFlight = false
      if (!active) {
        resolvePendingLoads()
        return
      }
      if (generation !== activeGeneration) {
        pendingLoad = false
        queueLoad()
        return
      }
      if (pendingLoad) {
        queueLoad()
      } else {
        schedule(generation)
        resolvePendingLoads()
      }
    }
  }

  function queueLoad(immediate = false): Promise<void> {
    if (!active) return Promise.resolve()
    if (immediate) clearTimer()
    if (inFlight) {
      pendingLoad = true
      return new Promise((resolve) => pendingResolvers.push(resolve))
    }
    return load()
  }

  function requestClear() {
    confirmDialog.request('Clear recent logs?', 'The in-memory 500-entry log ring will be emptied.', async () => {
      await ClearLogs()
      await queueLoad(true)
    })
  }

  watch(filter, () => queueLoad(true))
  onActivated(() => {
    active = true
    activeGeneration += 1
    pendingLoad = false
    queueLoad(true)
  })
  onDeactivated(() => {
    active = false
    activeGeneration += 1
    pendingLoad = false
    clearTimer()
    resolvePendingLoads()
  })
  onBeforeUnmount(() => {
    active = false
    activeGeneration += 1
    pendingLoad = false
    clearTimer()
    resolvePendingLoads()
  })

  return { logs, rows, filter, error, confirmDialog, requestClear }
}
