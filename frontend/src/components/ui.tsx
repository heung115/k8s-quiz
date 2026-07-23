import { useEffect, useState } from 'react'
import { Wrench, Search, Rocket, Box, Globe, Database, KeyRound, CalendarClock, Settings2 } from 'lucide-react'

/* ------------------------------------------------------------------ */
/* Difficulty → incident severity (SEV-1 is the worst, like real ops) */
/* ------------------------------------------------------------------ */
export const sevMeta: Record<string, { sev: string; label: string; cls: string; dot: string }> = {
  easy:   { sev: 'SEV-3', label: 'EASY',   cls: 'text-success border-success/40 bg-success-soft', dot: 'bg-success' },
  medium: { sev: 'SEV-2', label: 'MEDIUM', cls: 'text-warning border-warning/40 bg-warning-soft', dot: 'bg-warning' },
  hard:   { sev: 'SEV-1', label: 'HARD',   cls: 'text-danger border-danger/40 bg-danger-soft', dot: 'bg-danger' },
}

export const categoryMeta: Record<string, { label: string; icon: typeof Box }> = {
  pod:       { label: 'Pod',       icon: Box },
  network:   { label: 'Network',   icon: Globe },
  storage:   { label: 'Storage',   icon: Database },
  rbac:      { label: 'RBAC',      icon: KeyRound },
  scheduling:{ label: 'Scheduling',icon: CalendarClock },
  config:    { label: 'Config',    icon: Settings2 },
  deploy:    { label: 'Deploy',    icon: Rocket },
}

export const typeMeta: Record<string, { label: string; icon: typeof Wrench }> = {
  fix:    { label: 'FIX',    icon: Wrench },
  find:   { label: 'FIND',   icon: Search },
  deploy: { label: 'DEPLOY', icon: Rocket },
}

/* ------------------------------------------------------------------ */
/* Small primitives                                                    */
/* ------------------------------------------------------------------ */

export function SectionLabel({ children, className = '' }: { children: React.ReactNode; className?: string }) {
  return (
    <div className={`micro text-ink-faint flex items-center gap-2 ${className}`}>
      <span className="text-accent">//</span>
      {children}
    </div>
  )
}

export function SevTag({ difficulty, size = 'sm' }: { difficulty: string; size?: 'sm' | 'md' }) {
  const m = sevMeta[difficulty] || sevMeta.easy
  return (
    <span
      className={`inline-flex items-center gap-1.5 border font-mono font-medium ${m.cls} ${
        size === 'md' ? 'px-2.5 py-1 text-xs' : 'px-1.5 py-0.5 text-[10px]'
      }`}
      title={`difficulty: ${m.label}`}
    >
      <span className={`w-1.5 h-1.5 ${m.dot}`} aria-hidden="true" />
      {m.sev}
    </span>
  )
}

export function TypeTag({ type }: { type: string }) {
  const m = typeMeta[type] || typeMeta.fix
  const Icon = m.icon
  return (
    <span className="inline-flex items-center gap-1 font-mono text-[10px] tracking-wider text-ink-muted border border-edge px-1.5 py-0.5">
      <Icon className="w-3 h-3" aria-hidden="true" />
      {m.label}
    </span>
  )
}

export type SolveState = 'solved' | 'attempted' | 'open'

export function SolveStamp({ state }: { state: SolveState }) {
  if (state === 'solved') {
    return (
      <span className="micro text-success border border-success/40 px-1.5 py-0.5 inline-flex items-center gap-1">
        <span className="w-1.5 h-1.5 bg-success animate-pulse-dot" aria-hidden="true" />
        SOLVED
      </span>
    )
  }
  if (state === 'attempted') {
    return (
      <span className="micro text-warning border border-warning/40 px-1.5 py-0.5">ATTEMPTED</span>
    )
  }
  return <span className="micro text-ink-faint border border-edge px-1.5 py-0.5">OPEN</span>
}

/* ------------------------------------------------------------------ */
/* Terminal replay — animated hero terminal                            */
/* ------------------------------------------------------------------ */

type ReplayLine =
  | { kind: 'cmd'; text: string }
  | { kind: 'out'; text: string; tone?: 'ok' | 'err' | 'dim' | 'accent' }
  | { kind: 'gap' }

const SCRIPT: ReplayLine[] = [
  { kind: 'cmd', text: 'kubectl get pods' },
  { kind: 'out', text: 'NAME                      READY   STATUS                       RESTARTS   AGE' , tone: 'dim' },
  { kind: 'out', text: 'web-app-5b687cff65-x2k9   0/1     CreateContainerConfigError   0          4m', tone: 'err' },
  { kind: 'gap' },
  { kind: 'cmd', text: 'kubectl describe pod web-app-5b687cff65-x2k9 | grep -A2 Events' },
  { kind: 'out', text: 'Warning  Failed  kubelet  Error: configmap "app-config" not found', tone: 'err' },
  { kind: 'gap' },
  { kind: 'cmd', text: 'kubectl patch cm app-config --type merge -p \'{"data":{"MISSING_KEY":"8080"}}\'' },
  { kind: 'out', text: 'configmap/app-config patched', tone: 'accent' },
  { kind: 'gap' },
  { kind: 'cmd', text: 'kubectl get pods' },
  { kind: 'out', text: 'web-app-5b687cff65-x2k9   1/1     Running   0          5m', tone: 'ok' },
  { kind: 'gap' },
  { kind: 'out', text: '✓ verification passed — scenario cleared in 04:12', tone: 'ok' },
]

const toneCls: Record<string, string> = {
  ok: 'text-success',
  err: 'text-danger',
  dim: 'text-ink-faint',
  accent: 'text-info',
}

export function TerminalReplay() {
  const [lines, setLines] = useState<{ text: string; tone?: string; cmd?: boolean; partial?: string }[]>([])
  const [done, setDone] = useState(false)

  useEffect(() => {
    const reduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches
    if (reduced) {
      setLines(SCRIPT.filter((l) => l.kind !== 'gap').map((l) =>
        l.kind === 'cmd' ? { text: l.text, cmd: true } : { text: l.text, tone: l.tone }
      ))
      setDone(true)
      return
    }

    let cancelled = false
    const timers: ReturnType<typeof setTimeout>[] = []
    const sleep = (ms: number) => new Promise<void>((res) => timers.push(setTimeout(res, ms)))

    async function run() {
      while (!cancelled) {
        setLines([])
        setDone(false)
        for (const line of SCRIPT) {
          if (cancelled) return
          if (line.kind === 'gap') {
            await sleep(650)
            continue
          }
          if (line.kind === 'cmd') {
            // typewriter for commands
            for (let i = 1; i <= line.text.length; i++) {
              if (cancelled) return
              setLines((prev) => {
                const base = prev.filter((l) => !l.partial)
                return [...base, { text: line.text, cmd: true, partial: line.text.slice(0, i) }]
              })
              await sleep(14 + Math.random() * 26)
            }
            setLines((prev) => prev.map((l) => (l.partial ? { ...l, partial: undefined } : l)))
            await sleep(300)
          } else {
            setLines((prev) => [...prev, { text: line.text, tone: line.tone }])
            await sleep(90)
          }
        }
        setDone(true)
        await sleep(4200) // hold, then loop
      }
    }
    run()
    return () => {
      cancelled = true
      timers.forEach(clearTimeout)
    }
  }, [])

  return (
    <div className="relative border border-edge bg-terminal shadow-[0_24px_80px_-24px_rgba(50,108,229,0.25)]">
      {/* title bar */}
      <div className="flex items-center gap-2 px-4 py-2.5 border-b border-edge-soft bg-surface">
        <span className="w-2.5 h-2.5 rounded-full bg-danger/70" aria-hidden="true" />
        <span className="w-2.5 h-2.5 rounded-full bg-warning/70" aria-hidden="true" />
        <span className="w-2.5 h-2.5 rounded-full bg-success/70" aria-hidden="true" />
        <span className="ml-3 micro text-ink-faint">dev-admin@k8s-quiz — /bin/sh</span>
        <span className="ml-auto micro text-success inline-flex items-center gap-1.5">
          <span className="w-1.5 h-1.5 bg-success animate-pulse-dot" aria-hidden="true" />
          LIVE
        </span>
      </div>
      {/* scanline sweep */}
      <div className="absolute inset-0 overflow-hidden pointer-events-none" aria-hidden="true">
        <div className="absolute inset-x-0 h-24 bg-gradient-to-b from-transparent via-accent/[0.04] to-transparent animate-scan" />
      </div>
      {/* body */}
      <div className="p-4 font-mono text-[12.5px] leading-relaxed min-h-[320px] max-h-[320px] overflow-hidden">
        {lines.map((l, i) =>
          l.cmd ? (
            <div key={i} className="text-ink">
              <span className="text-accent-hover select-none">$ </span>
              {l.partial ?? l.text}
              {l.partial !== undefined && <span className="text-accent-hover animate-blink">▍</span>}
            </div>
          ) : (
            <div key={i} className={toneCls[l.tone || ''] || 'text-ink-muted'}>
              {l.text}
            </div>
          )
        )}
        {done && (
          <div className="text-ink mt-1">
            <span className="text-accent-hover select-none">$ </span>
            <span className="text-accent-hover animate-blink">▍</span>
          </div>
        )}
      </div>
    </div>
  )
}
