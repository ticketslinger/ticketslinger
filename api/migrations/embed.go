// Package migrations embeds the SQL schema migrations.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
