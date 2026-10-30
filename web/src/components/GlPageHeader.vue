<script setup>
/**
 * The page header every section page opens with: a title, one sentence of
 * orientation, and the actions for this page on the right.
 *
 * It replaces the old frame, which also carried an "eyebrow" and a badge
 * naming the underlying contract. Those described the implementation, not
 * the page, and had nowhere sensible to put them in a product.
 */
defineProps({
  title: { type: String, required: true },
  description: { type: String, default: '' },
})
</script>

<template>
  <header class="gl-page__head gl-header">
    <div class="gl-header__text">
      <h1>{{ title }}</h1>
      <p v-if="description" class="gl-muted gl-header__description">
        {{ description }}
      </p>
    </div>
    <div v-if="$slots.actions" class="gl-header__actions">
      <slot name="actions" />
    </div>
  </header>
</template>

<style scoped>
.gl-header {
  align-items: flex-start;
}

.gl-header__text {
  display: flex;
  flex: 1 1 auto;
  flex-direction: column;
  gap: var(--gl-space-2);
  min-width: 0;
}

.gl-header__description {
  max-width: 62ch;
  font-size: var(--gl-text-base);
  line-height: var(--gl-leading-normal);
}

.gl-header__actions {
  display: flex;
  flex: none;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--gl-space-2);
}

@media (max-width: 700px) {
  .gl-header__actions {
    width: 100%;
  }
}
</style>
