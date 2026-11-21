// Package publicmigrations embeds the migrations for the public search database.
package publicmigrations

import "embed"

//go:embed *.sql
var FS embed.FS
