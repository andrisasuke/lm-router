<script setup lang="ts">
import { Check, Copy } from '@lucide/vue'
import { onBeforeUnmount, ref } from 'vue'
import type { CreatedKey } from '../../types'
import { copyText } from '../../utils'

defineProps<{
  name: string
  createdKey: CreatedKey | null
  busy: boolean
  error: string
}>()

defineEmits<{
  close: []
  create: []
  'update:name': [value: string]
}>()

const copied = ref(false)
let copiedResetTimer = 0

async function copySecret(secret: string) {
  try {
    await copyText(secret)
    copied.value = true
    window.clearTimeout(copiedResetTimer)
    copiedResetTimer = window.setTimeout(() => {
      copied.value = false
    }, 2500)
  } catch {
    copied.value = false
  }
}

onBeforeUnmount(() => window.clearTimeout(copiedResetTimer))
</script>

<template>
  <div class="modal-backdrop" @click.self="$emit('close')">
    <div class="modal api-key-modal" role="dialog" aria-modal="true" aria-labelledby="api-key-title">
      <template v-if="!createdKey">
        <div class="eyebrow">LOCAL AUTHENTICATION</div>
        <h2 id="api-key-title">Add API key</h2>
        <p>Create a key for clients that connect to this LM Router instance.</p>

        <form @submit.prevent="$emit('create')">
          <label class="field">
            <span>Key label</span>
            <input
              :value="name"
              autofocus
              autocapitalize="off"
              autocorrect="off"
              spellcheck="false"
              placeholder="local"
              @input="$emit('update:name', ($event.target as HTMLInputElement).value)"
            />
          </label>

          <div v-if="error" class="notice fault">{{ error }}</div>
          <div class="modal-actions">
            <button type="button" class="button" @click="$emit('close')">Cancel</button>
            <button class="button primary" :disabled="busy || !name.trim()">Create key</button>
          </div>
        </form>
      </template>

      <template v-else>
        <div class="eyebrow">COPY NOW · SHOWN ONCE</div>
        <h2 id="api-key-title">{{ createdKey.name }}</h2>
        <p>LM Router stores only a SHA-256 hash. This full secret cannot be shown again.</p>
        <code class="api-key-secret">{{ createdKey.secret }}</code>
        <div class="modal-actions">
          <button class="button copy-secret-button" @click="copySecret(createdKey.secret)">
            <Check v-if="copied" :size="16" />
            <Copy v-else :size="16" />
            <span aria-live="polite">{{ copied ? 'Copied' : 'Copy secret' }}</span>
          </button>
          <button class="button primary" @click="$emit('close')">Done</button>
        </div>
      </template>
    </div>
  </div>
</template>
