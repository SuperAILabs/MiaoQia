// Package migrations owns pursuit database migrations.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
