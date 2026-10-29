import { reactive, readonly } from 'vue'
import {
  fetchMe,
  login as loginRequest,
  readAuthPayload,
  register as registerRequest,
} from '../api/auth.js'
import { setTokenProvider } from '../api/http.js'

/**
 * Where the bearer token lives.
 *
 * localStorage is used so a reload does not force a re-login; the trade-off is
 * that any script injection could read it. The client therefore never renders
 * model output as HTML (see `MessageContent.vue`), and nothing else about the
 * token is derived: no "remember me" long-lived copy, and logout wipes both
 * memory and storage.
 */
export const AUTH_STORAGE_KEY = 'gopherlearn.auth.v1'

function memoryStorage() {
  let value = ''
  return {
    getItem: () => value,
    setItem: (next) => {
      value = String(next)
    },
    removeItem: () => {
      value = ''
    },
  }
}

function defaultStorage() {
  try {
    if (typeof localStorage !== 'undefined' && localStorage) return localStorage
  } catch {
    /* private mode or a non-browser host */
  }
  return memoryStorage()
}

export function createAuthStore({ storage = defaultStorage(), now = Date.now } = {}) {
  const state = reactive({
    token: '',
    user: { id: '', email: '', displayName: '' },
    status: 'anonymous', // anonymous | authenticated
  })

  let storageAvailable = true

  function persist() {
    if (!storageAvailable) return
    try {
      if (state.token) {
        storage.setItem(
          AUTH_STORAGE_KEY,
          JSON.stringify({ token: state.token, user: state.user, at: now() }),
        )
      } else {
        storage.removeItem(AUTH_STORAGE_KEY)
      }
    } catch {
      storageAvailable = false
    }
  }

  function adopt(token, user) {
    state.token = token || ''
    state.user = user || { id: '', email: '', displayName: '' }
    state.status = state.token ? 'authenticated' : 'anonymous'
    persist()
  }

  /** Load a stored session at boot. Returns whether the client is signed in. */
  function restore() {
    let raw = null
    try {
      raw = storage.getItem(AUTH_STORAGE_KEY)
    } catch {
      storageAvailable = false
      return false
    }
    if (!raw) return false

    try {
      const parsed = JSON.parse(raw)
      if (parsed && typeof parsed.token === 'string' && parsed.token !== '') {
        adopt(parsed.token, parsed.user || state.user)
        return true
      }
    } catch {
      /* corrupt entry: treat as signed out */
    }
    clearStorage()
    return false
  }

  function clearStorage() {
    try {
      storage.removeItem(AUTH_STORAGE_KEY)
    } catch {
      /* nothing else to do */
    }
  }

  function isAuthenticated() {
    return state.token !== ''
  }

  async function signIn(path, payload) {
    const response = await path(payload)
    const { token, user } = readAuthPayload(response)
    if (!token) {
      // Without a token the shell cannot authenticate anything, so surface it
      // as an ordinary server error rather than entering a broken state.
      throw new Error('服务未返回有效的登录凭证。')
    }
    adopt(token, user)
    return user
  }

  /**
   * The contract gives `register` a public profile and `login` the bearer
   * token, so signing up is two calls. If the second one fails the account
   * genuinely exists, which is worth saying out loud rather than reporting as a
   * failed registration.
   */
  async function signUpThenSignIn(account) {
    await registerRequest(account)
    try {
      return await signIn(loginRequest, {
        email: account.email,
        password: account.password,
      })
    } catch (error) {
      if (error?.code === 'network_error' || error?.status >= 500) throw error
      const wrapped = new Error('账号已创建，请改用登录页登入。')
      wrapped.code = 'login_after_register'
      throw wrapped
    }
  }

  /**
   * Sign-out is a lifecycle event, not just a token wipe. Business stores that
   * cache per-account data subscribe here so *every* exit path — the manual
   * button, a 401 from any endpoint, a token the server rejected at boot —
   * cleans up identically. Wiring it into each call site instead is how one of
   * them gets missed.
   *
   * The auth layer deliberately knows nothing about what its listeners hold.
   */
  const signOutListeners = new Set()

  function subscribeSignOut(listener) {
    if (typeof listener !== 'function') return () => {}
    signOutListeners.add(listener)
    return () => signOutListeners.delete(listener)
  }

  function notifySignOut() {
    for (const listener of [...signOutListeners]) {
      try {
        listener()
      } catch {
        // One store failing to reset must not leave the session token in place
        // or stop the others from cleaning up.
      }
    }
  }

  function signOut() {
    state.token = ''
    state.user = { id: '', email: '', displayName: '' }
    state.status = 'anonymous'
    clearStorage()
    notifySignOut()
  }

  return {
    state: readonly(state),
    isAuthenticated,
    getToken: () => state.token,
    restore,
    adopt,
    signOut,
    subscribeSignOut,
    async login(credential) {
      return signIn(loginRequest, credential)
    },
    async register(account) {
      return signUpThenSignIn(account)
    },
    // Explicit native-token seam: adopt only the actual /me profile after the
    // caller confirms this page/request is still current. No login/registration.
    async confirmNativeToken(token, { signal, isCurrent = () => true } = {}) {
      if (typeof token !== 'string' || !token) throw new Error('注册未返回有效 token；登录状态未确认。')
      const profile = await fetchMe(token, signal, false)
      const { user } = readAuthPayload({ token, user: profile })
      if (!profile || !user.id || !user.email) throw new Error('/me 未返回有效用户资料；登录状态未确认。')
      if (signal?.aborted || !isCurrent()) return null
      adopt(token, user)
      return user
    },
    /** Confirm the stored token still works; drop it if the server says no. */
    async refreshProfile() {
      if (!state.token) return null
      try {
        const profile = await fetchMe(state.token)
        const { user } = readAuthPayload({ token: state.token, ...(profile || {}) })
        if (user.email || user.displayName) adopt(state.token, user)
        return user
      } catch (error) {
        if (error?.status === 401 || error?.code === 'unauthorized') signOut()
        throw error
      }
    },
  }
}

/** The singleton the app boots with. */
export const authStore = createAuthStore()

// Wire the token into the HTTP layer from here, the one place that owns it.
setTokenProvider(() => authStore.getToken())
