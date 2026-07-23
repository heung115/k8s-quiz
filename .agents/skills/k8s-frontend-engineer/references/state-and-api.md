# State & API Client

## Stores (Zustand)
- `auth`: current user, access/refresh tokens, login/logout/refresh actions. Tokens come from the OAuth callback URL fragment (`#access_token=...&refresh_token=...`); parse once on `/login/callback`, then clear the fragment.
- `session`: active attempt, timer/timeout state, start/reset/end actions.
- `terminal`: connection status, staged setup progress (`stage` messages), last verify result.

Keep stores thin: fetch in `src/api/`, store results, never parse WS frames inside a store.

## API Client
- One client module in `src/api/` with typed functions per endpoint.
- Attach `Authorization: Bearer <access_token>`; on `401`, attempt one refresh via `POST /api/auth/refresh`, then retry once.
- Response types must mirror the backend contract exactly (wrapped collections, `{error}` shape).

## Auth Token Handling
- Access token is short-lived (15m); refresh token (7d) is rotated on each refresh.
- Store tokens in memory + `sessionStorage`; never log them.

## Pages → Data
| Page | Needs |
| --- | --- |
| Dashboard `/` | `GET /api/problems` (filter by category/difficulty) |
| Login `/login` | OAuth redirect; `/login/callback` parses tokens |
| Problem `/problems/:id` | problem detail, WS terminal, verify/submit-choice, reset |
| Profile `/profile` | `GET /api/attempts` |
| Admin `/admin` | admin problems/users/attempts endpoints |
| Docs `/docs` | static user + problem-creation guides |
