// Package migrations 是 governance 数据库（gov）的迁移。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
