<script setup>
// Root component: the router outlet plus the theme provider.
//
// The provider is the only place the app decides how the control layer looks;
// every view below inherits it, so a page never has to configure a theme.
import { NConfigProvider } from 'naive-ui'

import { naiveThemeOverrides } from './styles/naive-theme.js'
</script>

<template>
  <n-config-provider :theme-overrides="naiveThemeOverrides">
    <RouterView />
  </n-config-provider>
</template>
