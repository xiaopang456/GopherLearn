<script setup>
import AppIcon from './AppIcon.vue'

/**
 * Shared frame for the Phase 1 placeholder sections. It exists so the three
 * panels read as siblings while each keeps its own copy and layout inside the
 * default slot.
 */
defineProps({
  eyebrow: { type: String, default: '' },
  title: { type: String, required: true },
  summary: { type: String, default: '' },
  icon: { type: String, default: 'knowledge' },
  status: { type: String, default: 'Phase 2 待接入' },
  /** `warn` for something still pending, `done` for something actually wired. */
  statusTone: { type: String, default: 'warn' },
})
</script>

<template>
  <div class="section">
    <header class="gl-page__head section__head">
      <span class="section__glyph">
        <AppIcon :name="icon" :size="22" />
      </span>
      <div class="section__titles">
        <p v-if="eyebrow" class="gl-eyebrow">{{ eyebrow }}</p>
        <h1>{{ title }}</h1>
        <p class="gl-muted">{{ summary }}</p>
      </div>
      <span v-if="status" class="gl-badge" :class="{ 'gl-badge--done': statusTone === 'done' }">
        {{ status }}
      </span>
    </header>
    <slot />
  </div>
</template>

<style scoped>
.section {
  max-width: 860px;
}

.section__head {
  display: flex;
  align-items: flex-start;
  gap: 0.9rem;
}

.section__glyph {
  display: flex;
  width: 42px;
  height: 42px;
  flex: none;
  align-items: center;
  justify-content: center;
  border-radius: var(--gl-radius);
  background: var(--gl-accent-soft);
  color: var(--gl-accent-ink);
}

.section__titles {
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 0.2rem;
  min-width: 0;
}
</style>
