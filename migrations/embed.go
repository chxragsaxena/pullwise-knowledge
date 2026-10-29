// Package migrations embeds the SQL schema so the service applies it itself at startup.
package migrations

import "embed"

// Files holds every NNNN_name.sql migration.
//
//go:embed *.sql
var Files embed.FS
