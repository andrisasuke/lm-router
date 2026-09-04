<script setup lang="ts">
import type { OAuthSession } from '../../types'

defineProps<{
  providerLabel: string
  reauth: boolean
  stage: 'prepare' | 'waiting'
  session: OAuthSession | null
  name: string
  callback: string
  busy: boolean
  message: string
}>()

defineEmits<{
  close: []
  launch: []
  submit: []
  'update:name': [value: string]
  'update:callback': [value: string]
}>()
</script>

<template>
  <div class="modal-backdrop" @click.self="$emit('close')">
    <div class="modal oauth-modal" role="dialog" aria-modal="true" aria-labelledby="oauth-title">
      <div class="eyebrow">OAUTH CONNECTION</div>
      <h2 id="oauth-title">{{ reauth ? 'Re-authenticate connection' : `Add ${providerLabel}` }}</h2>
      <template v-if="stage === 'prepare'">
        <label v-if="!reauth" class="field">
          <span>Connection alias</span>
          <input :value="name" placeholder="main" autofocus autocapitalize="off" autocorrect="off" spellcheck="false" @input="$emit('update:name', ($event.target as HTMLInputElement).value)" />
        </label>
        <p>Your system browser will open the provider authorization page. Codex returns automatically when port 1455 is available; Claude supplies a code to paste.</p>
        <div class="modal-actions">
          <button class="button" @click="$emit('close')">Cancel</button>
          <button class="button primary" :disabled="busy" @click="$emit('launch')">Open authorization</button>
        </div>
      </template>
      <template v-else>
        <div class="oauth-state">
          <span class="spinner"></span>
          <div>
            <strong>Waiting for authorization</strong>
            <small v-if="session?.loopback">The callback will return to this app automatically.</small>
            <small v-else>Paste the callback URL or code#state below.</small>
          </div>
        </div>
        <a v-if="session" class="auth-url" :href="session.authUrl" target="_blank" rel="noreferrer">Authorization URL opened in browser</a>
        <div v-if="message" class="notice warning">{{ message }}</div>
        <label class="field">
          <span>Manual callback fallback</span>
          <textarea
            :value="callback"
            rows="3"
            autocapitalize="off"
            autocorrect="off"
            spellcheck="false"
            placeholder="http://localhost:1455/auth/callback?code=…&state=… or code#state"
            @input="$emit('update:callback', ($event.target as HTMLTextAreaElement).value)"
          ></textarea>
        </label>
        <div class="modal-actions">
          <button class="button" @click="$emit('close')">Close</button>
          <button class="button primary" :disabled="busy || !callback.trim()" @click="$emit('submit')">Submit callback</button>
        </div>
      </template>
    </div>
  </div>
</template>
