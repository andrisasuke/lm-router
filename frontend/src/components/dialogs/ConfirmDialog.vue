<script setup lang="ts">
defineProps<{
  title: string
  message: string
  busy: boolean
}>()

defineEmits<{ close: []; confirm: [] }>()
</script>

<template>
  <div class="modal-backdrop" @click.self="$emit('close')">
    <div class="modal confirm-modal" role="alertdialog" aria-modal="true">
      <div class="eyebrow fault-text">CONFIRM DESTRUCTIVE ACTION</div>
      <h2>{{ title }}</h2>
      <p>{{ message }}</p>
      <div class="modal-actions">
        <button class="button" @click="$emit('close')">Cancel</button>
        <button class="button danger" :disabled="busy" @click="$emit('confirm')">Confirm</button>
      </div>
    </div>
  </div>
</template>
