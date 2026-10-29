<script setup>
/**
 * Every glyph here is a hand-written 24×24 stroke path from this project.
 * No icon font and no imported image, so there is nothing to attribute and
 * nothing that can carry a payload.
 */
const GLYPHS = {
  logo: ['M4 18 L12 4 L20 18', 'M7.5 18 H16.5'],
  chat: ['M4 5 H20 V16 H10 L6 20 V16 H4 Z'],
  knowledge: ['M12 3 L21 8 L12 13 L3 8 Z', 'M4.5 12.5 L12 16.5 L19.5 12.5'],
  memory: [
    'M12 20 C7 16.5 4.5 13.5 4.5 10 A7.5 7.5 0 0 1 12 2.5',
    'M12 20 C17 16.5 19.5 13.5 19.5 10 A7.5 7.5 0 0 0 12 2.5',
    'M12 20 V2.5',
  ],
  learning: ['M12 21 A9 9 0 1 1 12 3 A9 9 0 0 1 12 21 Z', 'M8 12.5 L11 15.5 L16.5 9'],
  send: ['M4 12 L20 4 L13.5 20 L11 13.5 Z'],
  stop: ['M7 7 H17 V17 H7 Z'],
  plus: ['M12 5 V19', 'M5 12 H19'],
  trash: ['M5 7 H19', 'M9 7 V4 H15 V7', 'M6.5 7 L7.5 20 H16.5 L17.5 7'],
  signout: ['M14 4 H19 V20 H14', 'M10 12 H3', 'M6.5 8.5 L3 12 L6.5 15.5'],
  menu: ['M4 7 H20', 'M4 12 H20', 'M4 17 H20'],
  close: ['M6 6 L18 18', 'M18 6 L6 18'],
  panel: ['M4 5 H20 V19 H4 Z', 'M10 5 V19'],
  monitor: ['M3 5 H21 V16 H3 Z', 'M8 20 H16', 'M12 16 V20', 'M6 12 L9 9 L12 12 L16 8'],
  alert: ['M12 4 L21 20 H3 Z', 'M12 10 V14', 'M12 17 V17.5'],
}

const props = defineProps({
  name: { type: String, required: true },
  label: { type: String, default: '' },
  size: { type: [Number, String], default: 20 },
})

function paths(name) {
  return GLYPHS[name] ?? []
}

const decorative = () => props.label === ''
</script>

<template>
  <svg
    class="icon"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.7"
    stroke-linecap="round"
    stroke-linejoin="round"
    :aria-hidden="decorative() ? 'true' : undefined"
    :aria-label="label || undefined"
    :role="label ? 'img' : undefined"
    focusable="false"
  >
    <path v-for="(d, index) in paths(props.name)" :key="index" :d="d" />
  </svg>
</template>

<style scoped>
.icon {
  display: block;
  flex: none;
}
</style>
