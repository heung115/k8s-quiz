import { spawnSync } from 'node:child_process'
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'

const exception = {
  advisoryURL: 'https://github.com/advisories/GHSA-qwww-vcr4-c8h2',
  packages: new Set(['react-router', 'react-router-dom']),
  version: '7.18.2',
  expires: '2026-10-31T00:00:00Z',
}

const allowedRouterImports = new Set([
  'BrowserRouter',
  'Link',
  'Navigate',
  'Outlet',
  'Route',
  'Routes',
  'useLocation',
  'useNavigate',
  'useParams',
  'useSearchParams',
])

function fail(message) {
  console.error(`production dependency audit failed: ${message}`)
  process.exit(1)
}

function walk(directory) {
  const files = []
  for (const entry of readdirSync(directory)) {
    const path = join(directory, entry)
    const stat = statSync(path)
    if (stat.isDirectory()) files.push(...walk(path))
    else if (/\.(?:ts|tsx|js|jsx)$/.test(entry) && !/\.(?:test|spec)\.(?:ts|tsx|js|jsx)$/.test(entry)) files.push(path)
  }
  return files
}

function assertDeclarativeRouterOnly() {
  for (const path of walk('src')) {
    const source = readFileSync(path, 'utf8')
    if (/from\s+['"]react-router['"]|from\s+['"]react-router-dom\/server['"]/.test(source)) {
      fail(`${relative('.', path)} imports a server/RSC router entrypoint`)
    }
    for (const match of source.matchAll(/import\s*{([^}]+)}\s*from\s*['"]react-router-dom['"]/g)) {
      for (const specifier of match[1].split(',')) {
        const imported = specifier.trim().split(/\s+as\s+/)[0]
        if (imported && !allowedRouterImports.has(imported)) {
          fail(`${relative('.', path)} imports unreviewed router API ${imported}`)
        }
      }
    }
  }
}

function assertPinnedRouterVersion() {
  const manifest = JSON.parse(readFileSync('package.json', 'utf8'))
  const lock = JSON.parse(readFileSync('package-lock.json', 'utf8'))
  if (manifest.dependencies?.['react-router-dom'] !== exception.version) {
    fail(`react-router-dom must remain exactly ${exception.version} while the exception is active`)
  }
  for (const name of exception.packages) {
    const installed = lock.packages?.[`node_modules/${name}`]?.version
    if (installed !== exception.version) {
      fail(`${name} lock version is ${installed || 'missing'}, want ${exception.version}`)
    }
  }
}

if (Date.now() >= Date.parse(exception.expires)) {
  fail(`reachability exception expired at ${exception.expires}`)
}
assertPinnedRouterVersion()
assertDeclarativeRouterOnly()

const audit = spawnSync('npm', ['audit', '--omit=dev', '--json'], {
  encoding: 'utf8',
  maxBuffer: 16 * 1024 * 1024,
})
if (audit.error) fail(`could not execute npm audit: ${audit.error.message}`)

let report
try {
  report = JSON.parse(audit.stdout)
} catch (error) {
  fail(`npm audit returned invalid JSON: ${error.message}`)
}

const vulnerabilities = Object.entries(report.vulnerabilities || {})
if (vulnerabilities.length === 0) {
  console.log('production dependency audit passed with no advisories')
  process.exit(0)
}

for (const [name, vulnerability] of vulnerabilities) {
  if (!exception.packages.has(name)) fail(`${name} has an unapproved advisory`)
  for (const via of vulnerability.via || []) {
    if (typeof via === 'string') {
      if (!exception.packages.has(via)) fail(`${name} depends on unapproved vulnerable package ${via}`)
      continue
    }
    if (via.url !== exception.advisoryURL) {
      fail(`${name} has unapproved advisory ${via.url || via.source || 'unknown'}`)
    }
  }
}

const critical = report.metadata?.vulnerabilities?.critical || 0
if (critical > 0) fail(`npm audit reported ${critical} critical vulnerabilities`)

console.log(
  `production dependency audit passed with one reachability exception: ${exception.advisoryURL} ` +
  `(declarative SPA only; expires ${exception.expires})`,
)
