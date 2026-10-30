import { createRouter, createWebHistory } from 'vue-router'

import { setUnauthorizedHandler } from '../api/http.js'
import { authStore as defaultAuthStore } from '../auth/store.js'
import LoginView from '../views/LoginView.vue'
import V2RegisterView from '../views/V2RegisterView.vue'
import ShellView from '../views/ShellView.vue'
import ChatView from '../views/ChatView.vue'
import V2DocumentsView from '../views/V2DocumentsView.vue'
import V2SourceView from '../views/V2SourceView.vue'
import V2MemoryView from '../views/V2MemoryView.vue'
import V2LearningView from '../views/V2LearningView.vue'
import NotFoundView from '../views/NotFoundView.vue'

export const LOGIN_ROUTE = { name: 'login', path: '/login' }

/**
 * `requiresAuth` is the single source of truth for the guard; the nav rail and
 * the sign-out flow both read it instead of hard-coding paths.
 *
 * The older knowledge / memory / learning pages are gone: they belonged to the
 * baseline entry, and this entry does not serve the interfaces they needed.
 */
export const appRoutes = [
  { path: '/register/v2', name: 'v2-register', component: V2RegisterView, meta: { guestOnly: true } },
  { path: '/login', name: 'login', component: LoginView, meta: { guestOnly: true } },
  {
    path: '/',
    component: ShellView,
    meta: { requiresAuth: true },
    children: [
      { path: '', redirect: { name: 'chat' } },
      { path: 'chat', name: 'chat', component: ChatView },
      {
        path: 'chat/:sessionId',
        name: 'session',
        component: ChatView,
      },
      { path: 'knowledge/v2-documents', name: 'v2-documents', component: V2DocumentsView },
      { path: 'knowledge/v2-documents/:documentId/source', name: 'v2-source', component: V2SourceView },
      { path: 'memory/v2', name: 'v2-memory', component: V2MemoryView },
      { path: 'learning/v2', name: 'v2-learning', component: V2LearningView },
      {
        // The unknown-path screen lives inside the shell, so it carries the
        // same navigation as every other page — but it stays reachable without
        // a session: an unknown address is not a secret.
        path: ':pathMatch(.*)*',
        name: 'not-found',
        component: NotFoundView,
        meta: { requiresAuth: false },
      },
    ],
  },
]

/**
 * Unauthenticated visits to the app go to the sign-in screen and come back to
 * the originally requested page afterwards, except for the login/register
 * routes themselves which would otherwise loop.
 */
export function installAuthGuard(router, authStore = defaultAuthStore) {
  router.beforeEach((to) => {
    // `to.meta` merges the shell's `requiresAuth` onto every child route.
    if (to.meta.requiresAuth && !authStore.isAuthenticated()) {
      return { name: 'login', query: { redirect: to.fullPath } }
    }
    if (to.meta.guestOnly && authStore.isAuthenticated()) {
      return { name: 'chat' }
    }
    return true
  })
  return router
}

/**
 * A bearer the server no longer accepts ends the session wherever it was used —
 * a knowledge call, a message stream, a history read — not only where the token
 * happened to be validated at boot. Navigation lives here, so the reaction to a
 * rejected credential lives here too.
 */
export function installUnauthorizedHandler(router, authStore = defaultAuthStore) {
  setUnauthorizedHandler(() => {
    if (!authStore.isAuthenticated()) return
    const current = router.currentRoute.value
    const redirect = current.meta?.requiresAuth ? current.fullPath : undefined
    authStore.signOut()
    if (!redirect) return
    if (current.name === 'login') return
    router.replace({ name: 'login', query: { redirect } })
  })
  return router
}

export function createAppRouter({
  authStore = defaultAuthStore,
  history = createWebHistory(),
} = {}) {
  const router = createRouter({ history, routes: appRoutes })
  installAuthGuard(router, authStore)
  installUnauthorizedHandler(router, authStore)
  router.afterEach((to) => {
    const title = to.meta?.title
    if (typeof document !== 'undefined' && title) document.title = title
  })
  return router
}
