// Package migrations 是 operations 数据库（ops）的迁移。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
