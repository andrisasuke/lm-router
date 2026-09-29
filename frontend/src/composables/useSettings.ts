import { onMounted, ref } from 'vue'
import { GetSettings, SaveSettings } from '../backend'
import type { Settings } from '../types'
import { errorText } from '../utils'

const initialSettings: Settings = {
  host: '127.0.0.1',
  port: 19090,
  trayEnabled: true,
  logRequests: true,
  logUpstream: true,
  logBodyLimit: 65536,
  defaultModel: 'gpt-5.3-codex',
  upstreamTimeoutSeconds: 300,
}

export function useSettings(onSaved?: () => void | Promise<void>) {
  const settings = ref<Settings>({ ...initialSettings })
  const busy = ref(false)
  const saved = ref(false)
  const error = ref('')

  async function load() {
    error.value = ''
    try {
      settings.value = await GetSettings() as unknown as Settings
    } catch (cause) {
      error.value = errorText(cause)
    }
  }

  async function save() {
    busy.value = true
    saved.value = false
    error.value = ''
    try {
      settings.value = await SaveSettings(settings.value) as unknown as Settings
      saved.value = true
      await onSaved?.()
      window.setTimeout(() => { saved.value = false }, 2500)
    } catch (cause) {
      error.value = errorText(cause)
    } finally {
      busy.value = false
    }
  }

  onMounted(load)

  return { settings, busy, saved, error, save }
}
