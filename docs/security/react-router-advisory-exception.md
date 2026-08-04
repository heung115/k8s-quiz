# React Router advisory exception

Status: temporary, expires 2026-10-31.

`react-router-dom` is pinned to `7.18.2`. As of 2026-07-31, npm reports
[GHSA-qwww-vcr4-c8h2](https://github.com/advisories/GHSA-qwww-vcr4-c8h2), an
RSC-mode CSRF action-execution issue. This application uses only declarative
browser SPA routing (`BrowserRouter`, `Routes`, `Route`, `Link`, `Navigate`,
`Outlet`, and location/navigation/parameter hooks). It does not configure RSC,
data routers, route actions, server actions, or SSR hydration.

Downgrading to the available `6.30.4` release was tested and rejected: npm then
reported open-redirect/XSS advisories in APIs this application actually uses,
including `Link` and `useNavigate`. No currently published stable version is
free of both advisory sets.

`npm run audit:prod` is the release gate. It permits only the exact advisory,
exact package version, and exact package chain above. It fails for any other
production advisory, any critical advisory, an expired exception, a router
version change, a server/RSC entrypoint, or an unreviewed router import. The
exception must be removed or explicitly re-reviewed before its expiry.
