import { onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { ClearLogs, Logs, LogText } from '../backend'
import type { LogEntry, LogPage, LogQuery } from '../types'
import { errorText } from '../utils'
import { useConfirmDialog } from './useConfirmDialog'

const pageSize = 100
const pollIntervalMs = 1500

export interface LogRow {
  id: number
  key: string
  time: string
  source: string
  sourceClass: string
  message: string
  truncated: boolean
}

export function useLogs() {
  const rows = shallowRef<LogRow[]>([])
  const filter = ref<'all' | 'proxy' | 'openai'>('all')
  const available = ref(0)
  const hasOlder = ref(false)
  const loadingOlder = ref(false)
  const error = ref('')
  const confirmDialog = useConfirmDialog()
  let timer: number | undefined
  let active = false
  let generation = 0
  let latestID = 0

  function clearTimer() {
    if (timer === undefined) return
    window.clearTimeout(timer)
    timer = undefined
  }

  function schedulePoll(currentGeneration: number) {
    clearTimer()
    if (!active || generation !== currentGeneration) return
    timer = window.setTimeout(() => {
      timer = undefined
      void loadNewer(currentGeneration)
    }, pollIntervalMs)
  }

  function query(afterId = 0, beforeId = 0): LogQuery {
    return { filter: filter.value, afterId, beforeId, limit: pageSize }
  }

  function buildRows(entries: LogEntry[]): LogRow[] {
    return entries.map((entry) => {
      const source = entry.source.toUpperCase()
      return {
        id: entry.id,
        key: String(entry.id),
        time: new Date(entry.time).toLocaleTimeString(),
        source,
        sourceClass: `source-${entry.source}`,
        message: entry.message,
        truncated: entry.truncated,
      }
    })
  }

  function replacePage(page: LogPage) {
    rows.value = buildRows(page.entries)
    latestID = page.latestId
    available.value = page.available
    hasOlder.value = page.hasOlder
  }

  function appendPage(page: LogPage) {
    if (page.reset) {
      replacePage(page)
      return
    }
    const next = page.oldestId > 0
      ? rows.value.filter((row) => row.id >= page.oldestId)
      : []
    if (page.entries.length > 0) next.push(...buildRows(page.entries))
    rows.value = next
    latestID = page.latestId
    available.value = page.available
  }

  async function loadLatest(currentGeneration: number) {
    error.value = ''
    try {
      const page = await Logs(query()) as unknown as LogPage
      if (!active || generation !== currentGeneration) return
      error.value = ''
      replacePage(page)
    } catch (cause) {
      if (active && generation === currentGeneration) error.value = errorText(cause)
    } finally {
      schedulePoll(currentGeneration)
    }
  }

  async function loadNewer(currentGeneration: number) {
    if (!active || generation !== currentGeneration) return
    try {
      const page = await Logs(query(latestID)) as unknown as LogPage
      if (!active || generation !== currentGeneration) return
      error.value = ''
      appendPage(page)
    } catch (cause) {
      if (active && generation === currentGeneration) error.value = errorText(cause)
    } finally {
      schedulePoll(currentGeneration)
    }
  }

  async function loadOlder() {
    if (!active || loadingOlder.value || !hasOlder.value || rows.value.length === 0) return
    const currentGeneration = generation
    const beforeID = rows.value[0].id
    loadingOlder.value = true
    try {
      const page = await Logs(query(0, beforeID)) as unknown as LogPage
      if (!active || generation !== currentGeneration) return
      error.value = ''
      const existing = new Set(rows.value.map((row) => row.id))
      const older = buildRows(page.entries).filter((row) => !existing.has(row.id))
      rows.value = [...older, ...rows.value]
      available.value = page.available
      hasOlder.value = page.hasOlder
    } catch (cause) {
      if (active && generation === currentGeneration) error.value = errorText(cause)
    } finally {
      if (generation === currentGeneration) loadingOlder.value = false
    }
  }

  async function clipboardValue(row: LogRow) {
    let message = row.message
    if (row.truncated) {
      try {
        message = await LogText(row.id)
      } catch {
        // The ring may evict an old entry between rendering and copying.
      }
    }
    return `${row.time} ${row.source} ${message}`
  }

  function resetAndReload() {
    generation += 1
    clearTimer()
    latestID = 0
    rows.value = []
    available.value = 0
    hasOlder.value = false
    loadingOlder.value = false
    if (active) void loadLatest(generation)
  }

  function requestClear() {
    confirmDialog.request('Clear recent logs?', 'The in-memory 500-entry log ring will be emptied.', async () => {
      generation += 1
      clearTimer()
      await ClearLogs()
      latestID = 0
      rows.value = []
      available.value = 0
      hasOlder.value = false
      if (active) await loadLatest(generation)
    })
  }

  watch(filter, resetAndReload)
  onMounted(() => {
    active = true
    generation += 1
    void loadLatest(generation)
  })
  onBeforeUnmount(() => {
    active = false
    generation += 1
    clearTimer()
  })

  return {
    rows,
    filter,
    available,
    hasOlder,
    loadingOlder,
    error,
    confirmDialog,
    loadOlder,
    clipboardValue,
    requestClear,
  }
}
