import { describe, it, expect, beforeEach } from 'vitest'
import { useSessionStore } from './session'
import type { Session, WSMessage } from '../types'

const baseSession: Session = {
  session_id: 's1',
  problem_id: 'p1',
  status: 'creating',
  timeout_at: '2030-01-01T00:00:00Z',
}

function reset(session: Session | null = { ...baseSession }) {
  useSessionStore.setState({
    session,
    stage: '',
    stageMessage: '',
    crashed: false,
    verifyResult: null,
    wsConnected: false,
    notice: null,
  })
}

const send = (msg: WSMessage) => useSessionStore.getState().handleWSMessage(msg)
const status = () => useSessionStore.getState().session?.status

describe('session store — stage → status mapping', () => {
  beforeEach(() => reset())

  it('maps container_created → booting', () => {
    send({ type: 'stage', stage: 'container_created', message: 'Creating container...' })
    expect(status()).toBe('booting')
  })

  it('maps k3s_booting → booting', () => {
    send({ type: 'stage', stage: 'k3s_booting', message: 'Waiting for k3s to boot...' })
    expect(status()).toBe('booting')
  })

  it('maps setup_running → setting_up', () => {
    send({ type: 'stage', stage: 'setup_running', message: 'Setting up problem environment...' })
    expect(status()).toBe('setting_up')
  })

  it('maps ready → ready', () => {
    send({ type: 'stage', stage: 'ready', message: 'Environment ready. Good luck!' })
    expect(status()).toBe('ready')
  })

  it('walks the full boot sequence to ready', () => {
    send({ type: 'stage', stage: 'container_created' })
    send({ type: 'stage', stage: 'k3s_booting' })
    send({ type: 'stage', stage: 'setup_running' })
    send({ type: 'stage', stage: 'ready' })
    expect(status()).toBe('ready')
  })

  it('leaves status untouched for an unmapped stage (container_crashed)', () => {
    send({ type: 'stage', stage: 'ready' })
    expect(status()).toBe('ready')
    send({ type: 'stage', stage: 'container_crashed', message: 'stopped unexpectedly' })
    expect(status()).toBe('ready') // unchanged
    expect(useSessionStore.getState().stage).toBe('container_crashed')
  })

  it('ignores stages when there is no active session', () => {
    reset(null)
    send({ type: 'stage', stage: 'ready' })
    expect(useSessionStore.getState().session).toBeNull()
  })
})

describe('session store — session_ended reasons', () => {
  beforeEach(() => reset())

  it('reset → keeps the session and sets status booting', () => {
    send({ type: 'stage', stage: 'ready' })
    send({ type: 'session_ended', reason: 'reset' })
    const s = useSessionStore.getState().session
    expect(s).not.toBeNull()
    expect(s?.status).toBe('booting')
    expect(useSessionStore.getState().crashed).toBe(false)
  })

  it('server_restart → clears the session and sets a notice', () => {
    send({ type: 'session_ended', reason: 'server_restart' })
    expect(useSessionStore.getState().session).toBeNull()
    expect(useSessionStore.getState().notice).toBeTruthy()
  })

  it('timeout (default path) → clears the session, leaves notice unset', () => {
    send({ type: 'session_ended', reason: 'timeout' })
    expect(useSessionStore.getState().session).toBeNull()
    expect(useSessionStore.getState().notice).toBeNull()
  })

  it('container_crashed → flags crashed and keeps the session', () => {
    send({ type: 'session_ended', reason: 'container_crashed' })
    expect(useSessionStore.getState().crashed).toBe(true)
    expect(useSessionStore.getState().session).not.toBeNull()
  })
})
