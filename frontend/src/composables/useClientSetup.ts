import { computed, onMounted, ref } from 'vue'
import { ClaudeConfig, CodexConfig } from '../backend'
import { errorText } from '../utils'

export function useClientSetup() {
  const codexConfig = ref('')
  const claudeConfig = ref('')
  const tab = ref<'codex' | 'claude'>('codex')
  const error = ref('')
  const text = computed(() => tab.value === 'codex' ? codexConfig.value : claudeConfig.value)

  async function load() {
    error.value = ''
    try {
      ;[codexConfig.value, claudeConfig.value] = await Promise.all([CodexConfig(), ClaudeConfig()])
    } catch (cause) {
      error.value = errorText(cause)
    }
  }

  onMounted(load)

  return { tab, text, error }
}
