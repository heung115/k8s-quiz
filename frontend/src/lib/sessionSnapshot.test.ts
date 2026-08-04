import { describe, expect, it } from 'vitest'
import { parseSessionSnapshot } from './sessionSnapshot'

const valid = {
  operation_id: 'operation-1',
  session_id: 'session-1',
  problem_id: 'problem-1',
  generation: 1,
  status: 'ready',
  timeout_at: '2030-01-01T00:00:00Z',
  cleanup_pending: false,
  terminal_reason: null,
  event_sequence: 3,
}

describe('public session snapshot parser', () => {
  it('accepts the complete durable current-session shape', () => {
    expect(parseSessionSnapshot(valid)).toEqual(valid)
    expect(parseSessionSnapshot({ ...valid, request_id: 'start:p1:key' })).toMatchObject({
      request_id: 'start:p1:key',
    })
  })

  it.each([
    ['operation_id', undefined],
    ['session_id', ''],
    ['problem_id', ''],
    ['generation', 0],
    ['status', 'unknown'],
    ['timeout_at', 'not-a-date'],
    ['cleanup_pending', undefined],
    ['terminal_reason', 42],
    ['event_sequence', 0],
  ])('rejects an invalid %s field', (field, value) => {
    expect(parseSessionSnapshot({ ...valid, [field]: value })).toBeNull()
  })
})
