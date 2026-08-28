import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'

// In development the Vue dev server proxies the API to the Go process, so the
// browser only ever talks to one origin and cookies behave as they do in
// production, where Go serves the built assets itself.
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: false,
        ws: true, // the session terminal upgrades to a WebSocket
      },
    },
  },
})
