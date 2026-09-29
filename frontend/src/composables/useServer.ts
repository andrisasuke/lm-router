import { onBeforeUnmount, onMounted, ref } from 'vue'
import {
  ServerStatus as FetchServerStatus,
  StartServer,
  StopServer,
} from '../backend'
import type { ServerStatus } from '../types'
import { errorText } from '../utils'

const initialStatus: ServerStatus = {
  state: 'OFF',
  configuredEndpoint: 'http://127.0.0.1:19090',
  actualEndpoint: '',
  host: '127.0.0.1',
  port: 19090,
  error: '',
}

export function useServer() {
  const server = ref<ServerStatus>({ ...initialStatus })
  const busy = ref(false)
  const error = ref('')
  let timer = 0

  async function refresh() {
    try {
      server.value = await FetchServerStatus() as unknown as ServerStatus
    } catch (cause) {
      error.value = errorText(cause)
    }
  }

  async function toggle() {
    busy.value = true
    error.value = ''
    try {
      server.value = (server.value.state === 'ON' ? await StopServer() : await StartServer()) as unknown as ServerStatus
    } catch (cause) {
      error.value = errorText(cause)
      await refresh()
    } finally {
      busy.value = false
    }
  }

  onMounted(async () => {
    await refresh()
    timer = window.setInterval(refresh, 1000)
  })

  onBeforeUnmount(() => window.clearInterval(timer))

  return { server, busy, error, refresh, toggle }
}
