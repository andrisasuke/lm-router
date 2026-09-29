<script setup lang="ts">
import { ref } from 'vue'
import type { Connection, CustomProviderInput } from '../../types'

const props = defineProps<{
  connection: Connection | null
  busy: boolean
  error: string
}>()

const emit = defineEmits<{ close: []; save: [input: CustomProviderInput] }>()

const step = ref(0)
const validationError = ref('')
const form = ref<CustomProviderInput>(props.connection ? {
  name: props.connection.name,
  prefix: props.connection.prefix,
  baseUrl: props.connection.baseUrl,
  apiKey: '',
  compatType: props.connection.compatType as CustomProviderInput['compatType'],
  apiType: (props.connection.apiType || '') as CustomProviderInput['apiType'],
} : {
  name: '',
  prefix: '',
  baseUrl: '',
  apiKey: '',
  compatType: 'openai-compatible',
  apiType: 'chat',
})

function validateStep() {
  const value = form.value
  if (step.value === 0 && !value.name.trim()) return 'Connection alias is required.'
  if (step.value === 1 && !value.prefix.trim()) return 'Routing prefix is required.'
  if (step.value === 2 && !value.baseUrl.trim()) return 'Base URL is required.'
  if (step.value === 5 && !props.connection && !value.apiKey.trim()) return 'API key is required.'
  return ''
}

function next() {
  validationError.value = validateStep()
  if (!validationError.value) step.value++
}

function save() {
  validationError.value = validateStep()
  if (!validationError.value) emit('save', form.value)
}
</script>

<template>
  <div class="modal-backdrop" @click.self="$emit('close')">
    <div class="modal custom-modal" role="dialog" aria-modal="true" aria-labelledby="custom-title">
      <div class="wizard-progress"><span v-for="number in 6" :key="number" :class="{ active: number - 1 <= step }">{{ String(number).padStart(2, '0') }}</span></div>
      <div class="eyebrow">CUSTOM PROVIDER · STEP {{ step + 1 }}/6</div>
      <h2 id="custom-title">{{ connection ? 'Edit custom connection' : 'Add custom connection' }}</h2>

      <label v-if="step === 0" class="field"><span>Connection alias</span><input v-model="form.name" autofocus autocapitalize="off" autocorrect="off" spellcheck="false" placeholder="my-server" /><small>A human-readable label for this endpoint.</small></label>
      <label v-else-if="step === 1" class="field"><span>Routing prefix</span><input v-model="form.prefix" autofocus autocapitalize="off" autocorrect="off" spellcheck="false" placeholder="myapi" /><small>Models route as <code>myapi/model-name</code>.</small></label>
      <label v-else-if="step === 2" class="field"><span>Base URL</span><input v-model="form.baseUrl" autofocus autocapitalize="off" autocorrect="off" spellcheck="false" placeholder="https://api.example.com/v1" /></label>
      <div v-else-if="step === 3" class="field">
        <span>Compatibility</span>
        <div class="choice-grid">
          <button :class="{ selected: form.compatType === 'openai-compatible' }" :disabled="!!connection" @click="form.compatType = 'openai-compatible'; form.apiType ||= 'chat'">OpenAI-compatible<small>Chat or Responses API</small></button>
          <button :class="{ selected: form.compatType === 'anthropic-compatible' }" :disabled="!!connection" @click="form.compatType = 'anthropic-compatible'; form.apiType = ''">Anthropic-compatible<small>Messages API</small></button>
        </div>
        <small v-if="connection">Compatibility cannot be changed after creation.</small>
      </div>
      <div v-else-if="step === 4" class="field">
        <span>Wire API</span>
        <div v-if="form.compatType === 'openai-compatible'" class="choice-grid">
          <button :class="{ selected: form.apiType === 'chat' }" @click="form.apiType = 'chat'">Chat Completions</button>
          <button :class="{ selected: form.apiType === 'responses' }" @click="form.apiType = 'responses'">Responses</button>
        </div>
        <p v-else>Anthropic-compatible endpoints always use the Messages API.</p>
      </div>
      <label v-else class="field"><span>Upstream API key</span><input v-model="form.apiKey" type="password" autofocus autocapitalize="off" autocorrect="off" spellcheck="false" :placeholder="connection ? 'Leave blank to keep existing key' : 'Required'" /><small>This secret is stored locally and is never returned to the webview.</small></label>

      <div v-if="validationError || error" class="notice fault">{{ validationError || error }}</div>
      <div class="modal-actions">
        <button class="button" @click="step === 0 ? $emit('close') : step--">{{ step === 0 ? 'Cancel' : 'Back' }}</button>
        <button v-if="step < 5" class="button primary" @click="next">Continue</button>
        <button v-else class="button primary" :disabled="busy" @click="save">{{ connection ? 'Save connection' : 'Add connection' }}</button>
      </div>
    </div>
  </div>
</template>
