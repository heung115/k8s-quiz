# WebSocket Protocol

Endpoint: `WS /ws/terminal`. Auth via the first message (not the URL).

## Client → Server
| type | fields | meaning |
| --- | --- | --- |
| `auth` | `token` | JWT presented immediately after connect |
| `input` | `data` | terminal keystrokes / command bytes |
| `resize` | `cols`, `rows` | terminal resize |

## Server → Client
| type | fields | meaning |
| --- | --- | --- |
| `output` | `data` | terminal output bytes |
| `stage` | `stage`, `message` | setup progress: `container_created` → `k3s_booting` → `setup_running` → `ready` |
| `verify_result` | `success`, `message`, `log` | grading outcome |
| `timeout_warning` | `remaining_seconds` | approaching per-problem timeout |
| `session_ended` | `reason` | `timeout` / `reset` / `server_restart` |
| `error` | `message` | protocol or runtime error |

## Flow Notes
- On `auth`, validate the JWT, resolve the user's active attempt, and bridge
  `input`/`output` to `Manager.ExecInteractive` for that container.
- Reject any non-`auth` first message; close on invalid token.
- `stage` messages are pushed during container setup so the UI can show progress
  before the terminal is interactive.
- The hub tracks one connection per user; a new connection supersedes the old.
