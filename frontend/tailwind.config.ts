import type { Config } from 'tailwindcss'

// Every color is a CSS variable so the whole UI re-themes by swapping the
// variable set in index.css (:root = light, html.theme-dark = dark). Components
// keep using semantic tokens (bg-canvas, text-ink, border-edge, bg-accent/30 …)
// and never hardcode a palette value. Channel-only vars use the <alpha-value>
// placeholder so Tailwind opacity modifiers keep working.
const ch = (name: string) => `rgb(var(${name}) / <alpha-value>)`
const raw = (name: string) => `var(${name})`

export default {
  content: ['./index.html', './src/**/*.{js,ts,jsx,tsx}'],
  theme: {
    extend: {
      colors: {
        canvas: ch('--canvas'),
        surface: {
          DEFAULT: ch('--surface'),
          2: ch('--surface-2'),
          3: ch('--surface-3'),
        },
        edge: {
          DEFAULT: ch('--edge'),
          soft: ch('--edge-soft'),
        },
        ink: {
          DEFAULT: ch('--ink'),
          muted: ch('--ink-muted'),
          faint: ch('--ink-faint'),
        },
        accent: {
          DEFAULT: ch('--accent'),
          hover: ch('--accent-hover'),
          soft: raw('--accent-soft'),
          dim: raw('--accent-dim'),
        },
        success: {
          DEFAULT: ch('--success'),
          soft: raw('--success-soft'),
        },
        warning: {
          DEFAULT: ch('--warning'),
          soft: raw('--warning-soft'),
        },
        danger: {
          DEFAULT: ch('--danger'),
          soft: raw('--danger-soft'),
        },
        info: {
          DEFAULT: ch('--info'),
          soft: raw('--info-soft'),
        },
        terminal: ch('--terminal'),
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
        'blink': { '0%, 100%': { opacity: '1' }, '50%': { opacity: '0' } },
        'pulse-dot': {
          '0%, 100%': { opacity: '1', transform: 'scale(1)' },
          '50%': { opacity: '0.5', transform: 'scale(0.8)' },
        },
        'scan': { '0%': { transform: 'translateY(-100%)' }, '100%': { transform: 'translateY(100%)' } },
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
