import { defineConfig } from 'vitest/config'

// Minimal test setup: pure store/client logic tests run in jsdom (so any
// browser-global access is safe) with globals enabled. Test files import from
// 'vitest' explicitly, so the project's `tsc` type-checks them without extra
// tsconfig `types` entries, and `vite build` never bundles them (nothing
// imports *.test.ts from the app graph).
export default defineConfig({
  test: {
    globals: true,
    environment: 'jsdom',
  },
})
