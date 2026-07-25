// Package migrations embeds the SQL migration files so the server can apply
// them at boot via the runtime migration runner (INFRA-4) — no filesystem
// mount or separate migrate CLI step required.
package migrations

import "embed"

// FS contains every *.sql migration file at the filesystem root.
//
//go:embed *.sql
var FS embed.FS
