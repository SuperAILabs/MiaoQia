// Package migrations owns channels database migrations.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
