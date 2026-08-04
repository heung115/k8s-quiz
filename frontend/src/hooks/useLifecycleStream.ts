import { useEffect, useRef } from 'react'
import {
  clearLifecycleCursor,
  loadLifecycleCursor,
  saveLifecycleCursor,
} from '../lib/lifecycleCursor'
import { clearPendingResetAfterLifecycle } from '../lib/resetOperation'
import { useSessionStore } from '../stores/session'
import type { LifecycleSubscribeFrame } from '../types'
import { WS_CLOSE_CONNECTION_REPLACED } from '../types'
import { ensureWebSocketAuth } from '../api/client'
import { useAuthStore } from '../stores/auth'

const MAX_RECONNECT_ATTEMPTS = 8
const MAX_BACKOFF_MS = 8_000

export function useLifecycleStream(userId: string | undefined, problemId: string | undefined): void {
  const identityRef = useRef({ userId, problemId })
  identityRef.current = { userId, problemId }

  useEffect(() => {
    if (!userId || !problemId) return

    let disposed = false
    let reconnectTimer: number | undefined
    let attempts = 0
    let forceFreshSnapshot = false
    let activeSocket: WebSocket | null = null
    const unsubscribe = useSessionStore.subscribe((state, previous) => {
      if (state.lifecycleCursor && state.lifecycleCursor !== previous.lifecycleCursor) {
        saveLifecycleCursor(userId, state.lifecycleCursor)
      }
    })

    const scheduleReconnect = () => {
      if (disposed || attempts >= MAX_RECONNECT_ATTEMPTS) return
      const delay = Math.min(500 * 2 ** attempts, MAX_BACKOFF_MS)
      attempts += 1
      reconnectTimer = window.setTimeout(() => { void connect() }, delay)
    }

    const connect = async () => {
      if (disposed) return
      const currentIdentity = identityRef.current
      if (currentIdentity.userId !== userId || currentIdentity.problemId !== problemId) return

      if (!await ensureWebSocketAuth()) {
        if (!disposed && identityRef.current.userId === userId &&
          identityRef.current.problemId === problemId && useAuthStore.getState().user?.id === userId) {
          scheduleReconnect()
        }
        return
      }
      if (disposed || identityRef.current.userId !== userId || identityRef.current.problemId !== problemId) return

      const state = useSessionStore.getState()
      const session = state.session
      if (session && !state.lifecycleCursor && !forceFreshSnapshot) {
        const stored = loadLifecycleCursor(userId, session.session_id, session.generation)
        if (stored) state.restoreLifecycleCursor(stored)
      }

      const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
      const initialSnapshotAuthorityEpoch = useSessionStore.getState().authorityEpoch
      const socket = new WebSocket(`${protocol}//${window.location.host}/ws/lifecycle`)
      activeSocket = socket
      let opened = false
      let resyncRequested = false
      let awaitingInitialSnapshot = true
      const isCurrentSocket = () => !disposed && activeSocket === socket &&
        identityRef.current.userId === userId && identityRef.current.problemId === problemId

      socket.onopen = () => {
        if (!isCurrentSocket()) {
          socket.close()
          return
        }
        opened = true
        useSessionStore.getState().setLifecycleConnected(true)
        const cursor = forceFreshSnapshot ? null : useSessionStore.getState().lifecycleCursor
        forceFreshSnapshot = false
        if (cursor) {
          const subscribe: LifecycleSubscribeFrame = { type: 'lifecycle_subscribe', cursor }
          socket.send(JSON.stringify(subscribe))
        }
      }

      socket.onmessage = (event) => {
        if (!isCurrentSocket()) return
        let value: unknown
        try {
          value = JSON.parse(String(event.data))
        } catch {
          value = null
        }
        const isInitialSnapshot = awaitingInitialSnapshot && !!value && typeof value === 'object' &&
          !Array.isArray(value) && (value as { type?: unknown }).type === 'lifecycle_snapshot'
        if (isInitialSnapshot) awaitingInitialSnapshot = false
        const result = useSessionStore.getState().handleLifecycleMessage(
          value,
          problemId,
          isInitialSnapshot ? initialSnapshotAuthorityEpoch : undefined,
        )
        if (!isCurrentSocket()) return
        if (result === 'resync') {
          const stale = useSessionStore.getState().lifecycleCursor
          if (stale) clearLifecycleCursor(userId, stale)
          useSessionStore.getState().prepareLifecycleResync()
          forceFreshSnapshot = true
          resyncRequested = true
          socket.close(1000, 'lifecycle_resync')
          return
        }
        if (result !== 'applied') return
        attempts = 0
        const next = useSessionStore.getState()
        if (next.session) clearPendingResetAfterLifecycle(userId, next.session)
      }

      socket.onclose = (event) => {
        if (!isCurrentSocket()) return
        activeSocket = null
        useSessionStore.getState().setLifecycleConnected(false)
        if (event.code === WS_CLOSE_CONNECTION_REPLACED) return
        if (!opened || resyncRequested || identityRef.current.userId === userId) scheduleReconnect()
      }

      socket.onerror = () => {
        if (!isCurrentSocket()) return
        // The close handler owns bounded reconnect. Lifecycle state remains
        // authoritative until a fresh snapshot says otherwise.
      }
    }

    void connect()
    return () => {
      disposed = true
      if (reconnectTimer !== undefined) window.clearTimeout(reconnectTimer)
      const socket = activeSocket
      activeSocket = null
      socket?.close(1000, 'page_unmounted')
      unsubscribe()
      useSessionStore.getState().setLifecycleConnected(false)
    }
  }, [problemId, userId])
}
