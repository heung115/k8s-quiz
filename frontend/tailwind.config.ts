import type { Config } from 'tailwindcss'

export default {
  content: ['./index.html', './src/**/*.{js,ts,jsx,tsx}'],
  theme: {
    extend: {
      colors: {
        // deep ops-console canvas, layered surfaces
        canvas: '#0a0e14',
        surface: {
          DEFAULT: '#0f141c',
          2: '#151b26',
          3: '#1c2432',
        },
        edge: {
          DEFAULT: '#26334a',
          soft: '#1a2334',
        },
        ink: {
          DEFAULT: '#e6edf6',
          muted: '#8b98ab',
          faint: '#5a6a82',
        },
        // Kubernetes brand blue — deliberate, not default tailwind blue
        accent: {
          DEFAULT: '#326ce5',
          hover: '#4d84f0',
          soft: 'rgba(50, 108, 229, 0.14)',
          dim: 'rgba(50, 108, 229, 0.07)',
        },
        success: {
          DEFAULT: '#2dd4a7',
          soft: 'rgba(45, 212, 167, 0.12)',
        },
        warning: {
          DEFAULT: '#f5b83d',
          soft: 'rgba(245, 184, 61, 0.12)',
        },
        danger: {
          DEFAULT: '#f4586b',
          soft: 'rgba(244, 88, 107, 0.12)',
        },
        info: {
          DEFAULT: '#38bdf8',
          soft: 'rgba(56, 189, 248, 0.12)',
        },
        terminal: '#05080d',
      },
      fontFamily: {
        display: ['"Chakra Petch"', '"IBM Plex Sans"', 'sans-serif'],
        sans: ['"IBM Plex Sans"', 'system-ui', '-apple-system', 'sans-serif'],
        mono: ['"IBM Plex Mono"', 'Menlo', 'Monaco', 'monospace'],
      },
      keyframes: {
        'fade-up': {
          '0%': { opacity: '0', transform: 'translateY(14px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' },
        },
        'blink': {
          '0%, 100%': { opacity: '1' },
          '50%': { opacity: '0' },
        },
        'pulse-dot': {
          '0%, 100%': { opacity: '1', transform: 'scale(1)' },
          '50%': { opacity: '0.5', transform: 'scale(0.8)' },
        },
        'scan': {
          '0%': { transform: 'translateY(-100%)' },
          '100%': { transform: 'translateY(100%)' },
        },
      },
      animation: {
        'fade-up': 'fade-up 0.5s ease-out both',
        'blink': 'blink 1.1s step-end infinite',
        'pulse-dot': 'pulse-dot 1.6s ease-in-out infinite',
        'scan': 'scan 7s linear infinite',
      },
    },
  },
  plugins: [],
} satisfies Config
