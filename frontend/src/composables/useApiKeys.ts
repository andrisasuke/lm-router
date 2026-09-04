import { onMounted, ref } from 'vue'
import { CreateKey, DeleteKey, ListKeys } from '../backend'
import type { APIKey, CreatedKey } from '../types'
import { errorText } from '../utils'
import { useConfirmDialog } from './useConfirmDialog'

export function useApiKeys() {
  const keys = ref<APIKey[]>([])
  const keyName = ref('local')
  const createdKey = ref<CreatedKey | null>(null)
  const createOpen = ref(false)
  const createError = ref('')
  const busy = ref(false)
  const error = ref('')
  const confirmDialog = useConfirmDialog()

  async function load() {
    error.value = ''
    try {
      keys.value = await ListKeys() as unknown as APIKey[]
    } catch (cause) {
      error.value = errorText(cause)
    }
  }

  async function create() {
    busy.value = true
    createError.value = ''
    createdKey.value = null
    try {
      createdKey.value = await CreateKey(keyName.value.trim()) as unknown as CreatedKey
      await load()
    } catch (cause) {
      createError.value = errorText(cause)
    } finally {
      busy.value = false
    }
  }

  function openCreate() {
    keyName.value = 'local'
    createdKey.value = null
    createError.value = ''
    createOpen.value = true
  }

  function closeCreate() {
    if (busy.value) return
    createOpen.value = false
    createdKey.value = null
    createError.value = ''
  }

  function requestDelete(key: APIKey) {
    confirmDialog.request(
      'Revoke API key?',
      `${key.name} (${key.prefix}…) will immediately stop authenticating local clients.`,
      async () => { await DeleteKey(key.id); await load() },
    )
  }

  onMounted(load)

  return {
    keys,
    keyName,
    createdKey,
    createOpen,
    createError,
    busy,
    error,
    confirmDialog,
    create,
    openCreate,
    closeCreate,
    requestDelete,
  }
}
