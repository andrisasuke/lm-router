import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ClearLogs, Logs } from '../backend'
import type { LogEntry } from '../types'
import { errorText } from '../utils'
import { useConfirmDialog } from './useConfirmDialog'

export function useLogs() {
  const logs = ref<LogEntry[]>([])
  const filter = ref<'all' | 'proxy' | 'openai'>('all')
  const error = ref('')
  const confirmDialog = useConfirmDialog()
  let timer = 0

  async function load() {
    error.value = ''
    try {
      logs.value = await Logs(filter.value) as unknown as LogEntry[]
    } catch (cause) {
      error.value = errorText(cause)
    }
  }

  function requestClear() {
    confirmDialog.request('Clear recent logs?', 'The in-memory 500-entry log ring will be emptied.', async () => {
      await ClearLogs()
      await load()
    })
  }

  watch(filter, load)
  onMounted(async () => {
    await load()
    timer = window.setInterval(load, 1000)
  })
  onBeforeUnmount(() => window.clearInterval(timer))

  return { logs, filter, error, confirmDialog, requestClear }
}
