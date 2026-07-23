import type { Config } from 'tailwindcss'

export default {
  content: ['./index.html', './src/**/*.{js,ts,jsx,tsx}'],
  theme: {
    extend: {
      colors: {
        canvas: '#0b0d12',
        surface: {
          DEFAULT: '#12151c',
          2: '#1a1e27',
          3: '#232833',
        },
        edge: '#2a303c',
        ink: {
          DEFAULT: '#e7eaf0',
          muted: '#98a1b3',
          faint: '#5f6b7e',
        },
        accent: {
          DEFAULT: '#3b82f6',
          hover: '#60a5fa',
          soft: 'rgba(59, 130, 246, 0.12)',
        },
        success: {
          DEFAULT: '#34d399',
          soft: 'rgba(52, 211, 153, 0.12)',
        },
        warning: {
          DEFAULT: '#fbbf24',
          soft: 'rgba(251, 191, 36, 0.12)',
        },
        danger: {
          DEFAULT: '#f87171',
          soft: 'rgba(248, 113, 113, 0.12)',
        },
      },
      fontFamily: {
        mono: ['Menlo', 'Monaco', '"JetBrains Mono"', '"Courier New"', 'monospace'],
      },
    },
  },
  plugins: [],
} satisfies Config
