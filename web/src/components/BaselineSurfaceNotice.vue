<script setup>
import ServerNotice from './ServerNotice.vue'

/**
 * The knowledge-collection, legacy memory and legacy quiz pages read the
 * historical baseline entry's routes. The integrated application does not
 * serve them, so these pages say which entry they belong to instead of
 * showing a list that looks empty.
 *
 * This is a statement about the page, not a guess about the running backend:
 * the routes these pages use are the baseline's, whoever is serving them.
 *
 * The link is a plain anchor rather than a router link: these pages are
 * mounted without a router in the test suite, and a full navigation to the
 * page that covers the same ground is honest about that.
 */
defineProps({
  // The page in this application that covers the same ground.
  replacement: { type: Object, default: null },
})
</script>

<template>
  <div class="baseline-notice" role="status" data-test="baseline-surface-notice">
    <ServerNotice
      variant="info"
      message="这一页属于历史基线入口（cmd/gopherlearn）的接口。整合入口不提供这组接口。"
    />
    <p v-if="replacement" class="baseline-notice__hint">
      对应功能请用
      <a :href="replacement.href">{{ replacement.label }}</a>
      页面。
    </p>
  </div>
</template>

<style scoped>
.baseline-notice {
  margin-bottom: 0.8rem;
}

.baseline-notice__hint {
  margin: 0.4rem 0 0;
  font-size: 0.92rem;
  color: var(--gl-ink-soft);
}
</style>
