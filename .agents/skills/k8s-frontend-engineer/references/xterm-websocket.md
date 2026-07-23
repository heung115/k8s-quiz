# xterm.js ↔ WebSocket Bridge

## Lifecycle
1. Connect to `WS /ws/terminal`.
2. Immediately send `{type:"auth", token}`. Wait for `stage` messages and/or first `output`.
3. On xterm `onData`, send `{type:"input", data}`.
4. On server `output`, `term.write(data)`.
5. On `resize` (fit addon), send `{type:"resize", cols, rows}`.
6. On `session_ended` or socket close, dispose the terminal and update the session store.

## Message Handling
- `stage`: update a progress indicator (`container_created → k3s_booting → setup_running → ready`); enable input only at `ready`.
- `verify_result`: show success/failure banner + collapsible log.
- `timeout_warning`: surface remaining seconds near the timer.
- `error`: show a toast; do not crash the terminal.

## Rules
- Reconnect is not silent: if the socket drops mid-session, prompt the user to reset rather than auto-reattaching to a possibly-dead container.
- Always send a fresh `resize` after fit and after the panel becomes visible.
