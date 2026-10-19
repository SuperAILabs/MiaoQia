// Package migrations owns customer-context database migrations.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
