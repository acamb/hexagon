import { createRouter, createWebHistory } from 'vue-router'
import HomeView from './views/HomeView.vue'
import LoginView from './views/LoginView.vue'
import { load } from './session'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'home', component: HomeView },
    { path: '/login', name: 'login', component: LoginView, meta: { public: true } },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

// Every route except /login needs a session. The check is a request to
// /api/auth/me: the server is the only authority on whether the cookie is still
// valid, so there is nothing to trust on the client side.
router.beforeEach(async (to) => {
  const user = await load()
  if (!user && !to.meta.public) {
    return { name: 'login', query: to.fullPath === '/' ? {} : { redirect: to.fullPath } }
  }
  if (user && to.name === 'login') {
    return { path: (to.query.redirect as string) || '/' }
  }
  return true
})
