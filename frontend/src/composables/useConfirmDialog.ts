import { ref } from 'vue'
import { errorText } from '../utils'

export function useConfirmDialog() {
  const open = ref(false)
  const busy = ref(false)
  const title = ref('')
  const message = ref('')
  const error = ref('')
  const action = ref<null | (() => Promise<void>)>(null)

  function request(nextTitle: string, nextMessage: string, nextAction: () => Promise<void>) {
    title.value = nextTitle
    message.value = nextMessage
    action.value = nextAction
    error.value = ''
    open.value = true
  }

  function close() {
    open.value = false
  }

  async function confirm() {
    if (!action.value) return
    busy.value = true
    error.value = ''
    try {
      await action.value()
      open.value = false
    } catch (cause) {
      error.value = errorText(cause)
    } finally {
      busy.value = false
    }
  }

  return { open, busy, title, message, error, request, close, confirm }
}
