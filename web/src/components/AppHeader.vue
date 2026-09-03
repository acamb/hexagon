<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { currentUser, logout } from '../session'

const router = useRouter()
// The links collapse behind this on a narrow viewport; irrelevant, and left
// false, above the breakpoint where the dropdown CSS never applies.
const menuOpen = ref(false)

function closeMenu() {
  menuOpen.value = false
}

async function signOut() {
  closeMenu()
  await logout()
  router.push({ name: 'login' })
}
</script>

<template>
  <header class="bar">
    <span class="brand">
      <picture>
        <source srcset="/hexagon-mark-dark.png" media="(prefers-color-scheme: dark)" />
        <img src="/hexagon-mark.png" alt="" width="64" height="57" />
      </picture>
      <strong>Hexagon</strong>
    </span>

    <button
      type="button"
      class="toggle"
      :aria-expanded="menuOpen"
      aria-label="Toggle menu"
      @click="menuOpen = !menuOpen"
    >
      <span class="bars" />
    </button>

    <div class="account" v-if="currentUser">
      <img v-if="currentUser.avatarUrl" :src="currentUser.avatarUrl" alt="" width="24" height="24" />
      <span>{{ currentUser.login }}</span>
      <button type="button" @click="signOut">Sign out</button>
    </div>

    <!-- Click-outside-to-close for the dropdown below. Not a dialog: no focus
         trap, since it holds nothing but links back into the same page. -->
    <div v-if="menuOpen" class="scrim" @click="closeMenu" />

    <nav :class="{ open: menuOpen }">
      <RouterLink :to="{ name: 'sessions' }" @click="closeMenu">Sessions</RouterLink>
      <RouterLink :to="{ name: 'images' }" @click="closeMenu">Images</RouterLink>
      <RouterLink :to="{ name: 'accounts' }" @click="closeMenu">Accounts</RouterLink>
      <RouterLink :to="{ name: 'settings' }" @click="closeMenu">Settings</RouterLink>

      <!-- Duplicates .account: below the breakpoint that hides .account's own
           username and button down to just the avatar, this is where they
           reappear. -->
      <div class="account-mobile" v-if="currentUser">
        <span>{{ currentUser.login }}</span>
        <button type="button" @click="signOut">Sign out</button>
      </div>
    </nav>
  </header>
</template>

<style scoped>
.bar {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0.75rem 1.5rem;
  border-bottom: 1px solid var(--border);
  background: var(--surface);
}

nav {
  display: flex;
  align-items: center;
  gap: 1.25rem;
}

.toggle {
  display: none;
  padding: 0.4rem;
  border: 1px solid var(--border);
  border-radius: 6px;
  background: transparent;
  cursor: pointer;
}

/* Drawn rather than an icon font or an SVG import, for the same three bars
   every other mobile nav uses: .bars is the middle one, the other two are
   positioned off it. */
.bars {
  position: relative;
  display: block;
  width: 1.1rem;
  height: 2px;
  background: var(--text);
  border-radius: 1px;
}

.bars::before,
.bars::after {
  content: '';
  position: absolute;
  left: 0;
  width: 1.1rem;
  height: 2px;
  background: var(--text);
  border-radius: 1px;
}

.bars::before {
  top: -5px;
}

.bars::after {
  top: 5px;
}

.scrim {
  position: fixed;
  inset: 0;
  z-index: 15;
}

.account-mobile {
  display: none;
}

.brand {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  /* Extra room after it, so the mark and the name do not read as one more
     destination in the list beside them. */
  margin-right: 0.5rem;
}

/* The intrinsic size is on the element so the bar does not reflow when the mark
   arrives; the height here is what it is actually drawn at. */
.brand img {
  width: auto;
  height: 1.75rem;
}

nav a {
  color: var(--text-muted);
  text-decoration: none;
}

nav a:hover {
  color: var(--text);
}

nav a.router-link-active {
  color: var(--text);
  font-weight: 600;
}

.account {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  color: var(--text-muted);
}

.account img {
  border-radius: 50%;
}

.account button {
  border: 1px solid var(--border);
  border-radius: 6px;
  background: transparent;
  color: inherit;
  font: inherit;
  padding: 0.25rem 0.6rem;
  cursor: pointer;
}

.account button:hover {
  border-color: var(--accent);
  color: var(--text);
}

@media (max-width: 640px) {
  .toggle {
    display: inline-flex;
  }

  /* The bar keeps only the avatar as an identity glyph; the name and Sign out
     move into the dropdown below, as .account-mobile. */
  .account span,
  .account button {
    display: none;
  }

  nav {
    display: none;
  }

  nav.open {
    display: flex;
    flex-direction: column;
    align-items: stretch;
    position: absolute;
    top: 100%;
    left: 0;
    right: 0;
    z-index: 20;
    gap: 0.75rem;
    padding: 0.75rem 1.5rem;
    background: var(--surface);
    border-bottom: 1px solid var(--border);
  }

  .account-mobile {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.75rem;
    margin-top: 0.5rem;
    padding-top: 0.75rem;
    border-top: 1px solid var(--border);
    color: var(--text-muted);
  }

  .account-mobile button {
    border: 1px solid var(--border);
    border-radius: 6px;
    background: transparent;
    color: inherit;
    font: inherit;
    padding: 0.25rem 0.6rem;
    cursor: pointer;
  }

  .account-mobile button:hover {
    border-color: var(--accent);
    color: var(--text);
  }
}
</style>
