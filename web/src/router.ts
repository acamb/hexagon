import { createRouter, createWebHistory } from 'vue-router'
import AccountsView from './views/AccountsView.vue'
import ImagesView from './views/ImagesView.vue'
import LoginView from './views/LoginView.vue'
import SessionView from './views/SessionView.vue'
import SessionsView from './views/SessionsView.vue'
import SettingsView from './views/SettingsView.vue'
import SetupView from './views/SetupView.vue'
import { load } from './session'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'sessions', component: SessionsView },
    { path: '/images', name: 'images', component: ImagesView },
    { path: '/accounts', name: 'accounts', component: AccountsView },
    { path: '/settings', name: 'settings', component: SettingsView },
    { path: '/sessions/:id', name: 'session', component: SessionView },
    { path: '/login', name: 'login', component: LoginView, meta: { public: true } },
    { path: '/setup', name: 'setup', component: SetupView, meta: { public: true } },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

// Every route except /login and /setup needs a session. The check is a request
// to /api/auth/me: the server is the only authority on whether the cookie is
// still valid, so there is nothing to trust on the client side.
//
// Whether the first-time wizard is still open is a second question, and it is
// asked where it is cheap: the sign-in page asks it when it is shown, and the
// wizard itself sends anyone away when the answer is no. A signed-in visitor
// never pays for either.
router.beforeEach(async (to) => {
  const user = await load()
  if (!user && !to.meta.public) {
    return { name: 'login', query: to.fullPath === '/' ? {} : { redirect: to.fullPath } }
  }
  if (user && (to.name === 'login' || to.name === 'setup')) {
    return { path: (to.query.redirect as string) || '/' }
  }
  return true
})
