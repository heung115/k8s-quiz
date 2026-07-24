import { create } from 'zustand'
import { Session, WSMessage } from '../types'

interface SessionState {
  session: Session | null
  stage: string
  stageMessage: string
  crashed: boolean
  verifyResult: { success: boolean; log: string } | null
  wsConnected: boolean
  setSession: (session: Session | null) => void
  setStage: (stage: string, message: string) => void
  setCrashed: (crashed: boolean) => void
  setVerifyResult: (result: { success: boolean; log: string } | null) => void
  setWsConnected: (connected: boolean) => void
  handleWSMessage: (msg: WSMessage) => void
  clear: () => void
}

// Maps backend boot stages (service.go emitStage) onto the session.status the
// UI gates on (ProblemPage verify button + StageTimeline). Stages not listed
// (e.g. container_crashed) leave status untouched so existing handling stands.
const STATUS_BY_STAGE: Record<string, string> = {
  container_created: 'booting',
  k3s_booting: 'booting',
  setup_running: 'setting_up',
  ready: 'ready',
}

export const useSessionStore = create<SessionState>((set) => ({
  session: null,
  stage: '',
  stageMessage: '',
  crashed: false,
  verifyResult: null,
  wsConnected: false,
  setSession: (session) => set({ session, crashed: false }),
  setStage: (stage, message) => set({ stage, stageMessage: message }),
  setCrashed: (crashed) => set({ crashed }),
  setVerifyResult: (result) => set({ verifyResult: result }),
  setWsConnected: (connected) => set({ wsConnected: connected }),
  handleWSMessage: (msg) => {
    switch (msg.type) {
      case 'stage': {
        const stage = msg.stage || ''
        const nextStatus = STATUS_BY_STAGE[stage]
        set((state) => ({
          stage,
          stageMessage: msg.message || '',
          session:
            nextStatus && state.session && state.session.status !== nextStatus
              ? { ...state.session, status: nextStatus }
              : state.session,
        }))
        break
      }
      case 'verify_result':
        set({ verifyResult: { success: msg.success || false, log: msg.log || '' } })
        break
      case 'session_ended':
        if (msg.reason === 'container_crashed') {
          set({
            crashed: true,
            stage: 'container_crashed',
            stageMessage: '컨테이너가 중단되었습니다. 리셋하여 다시 시작하세요.',
          })
        } else {
          set({ session: null, stage: '', stageMessage: '', crashed: false })
        }
        break
    }
  },
  clear: () => set({ session: null, stage: '', stageMessage: '', crashed: false, verifyResult: null, wsConnected: false }),
}))
