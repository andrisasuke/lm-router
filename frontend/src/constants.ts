import type { Provider } from './types'

export const providers: Array<{ id: Provider; label: string; route: string }> = [
  { id: 'openai-codex', label: 'OpenAI Codex', route: 'gpt*' },
  { id: 'anthropic-claude', label: 'Anthropic Claude', route: 'claude*' },
  { id: 'custom', label: 'Custom', route: 'prefix/*' },
]
