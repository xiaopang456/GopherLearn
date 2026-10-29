<script setup>
import { computed } from 'vue'

const props = defineProps({
  modelValue: { type: String, default: '' },
  label: { type: String, required: true },
  name: { type: String, required: true },
  type: { type: String, default: 'text' },
  error: { type: String, default: '' },
  hint: { type: String, default: '' },
  autocomplete: { type: String, default: 'off' },
  placeholder: { type: String, default: '' },
  disabled: { type: Boolean, default: false },
})

const emit = defineEmits(['update:modelValue', 'blur'])

const inputId = computed(() => `fld-${props.name}`)
const describedBy = computed(() => {
  if (props.error) return `${inputId.value}-error`
  if (props.hint) return `${inputId.value}-hint`
  return undefined
})
</script>

<template>
  <div class="field">
    <label class="field__label" :for="inputId">{{ label }}</label>
    <input
      :id="inputId"
      class="field__input"
      :name="props.name"
      :type="props.type"
      :value="props.modelValue"
      :autocomplete="props.autocomplete"
      :placeholder="props.placeholder"
      :disabled="props.disabled"
      :aria-invalid="props.error ? 'true' : undefined"
      :aria-describedby="describedBy"
      @input="emit('update:modelValue', $event.target.value)"
      @blur="emit('blur')"
    />
    <p v-if="props.error" :id="`${inputId}-error`" class="field__msg field__msg--error">
      {{ props.error }}
    </p>
    <p v-else-if="props.hint" :id="`${inputId}-hint`" class="field__msg">
      {{ props.hint }}
    </p>
  </div>
</template>

<style scoped>
.field {
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
}

.field__label {
  font-size: 0.86rem;
  font-weight: 700;
  color: var(--gl-ink-soft);
}

.field__input {
  width: 100%;
  padding: 0.62rem 0.75rem;
  border: 1px solid var(--gl-surface-line);
  border-radius: var(--gl-radius-sm);
  background: var(--gl-surface);
  font-size: 1rem;
}

.field__input:focus {
  border-color: var(--gl-accent);
}

.field__input[aria-invalid='true'] {
  border-color: var(--gl-danger);
  background: #fffafa;
}

.field__msg {
  margin: 0;
  font-size: 0.82rem;
  color: var(--gl-ink-faint);
}

.field__msg--error {
  color: var(--gl-danger);
  font-weight: 600;
}
</style>
