<script setup lang="ts">
defineEmits<{ close: []; accept: [] }>()
</script>

<template>
  <div class="modal-backdrop" @click.self="$emit('close')">
    <div class="modal risk-modal" role="dialog" aria-modal="true" aria-labelledby="risk-title">
      <div class="eyebrow fault-text">ACCOUNT RISK</div>
      <h2 id="risk-title">Claude subscription OAuth</h2>
      <p>Claude subscriptions and the Anthropic API are separate products. Routing subscription OAuth through a third-party router is not an officially licensed flow and may put the account at risk.</p>
      <p>Automatic fallback can be treated as combining or bypassing capacity limits. Continue only if you understand and accept that risk.</p>
      <div class="modal-actions">
        <button class="button" @click="$emit('close')">Cancel</button>
        <button class="button danger" @click="$emit('accept')">I understand, continue</button>
      </div>
    </div>
  </div>
</template>
