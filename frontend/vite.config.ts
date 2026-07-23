import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://localhost:8080',
      '/ws': { target: 'ws://localhost:8080', ws: true },
    },
  },
  build: {
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (!id.includes('node_modules')) return
          if (id.includes('@xterm')) return 'xterm'
          if (id.includes('lucide-react')) return 'icons'
          if (id.includes('/react-dom/') || id.includes('/react/') || id.includes('react-router') || id.includes('zustand') || id.includes('scheduler')) return 'vendor'
        },
      },
    },
  },
})
