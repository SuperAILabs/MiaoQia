// Package api 是 learning 的公开接口（其他子系统唯一可导入的部分）。规格见 docs/03-architecture/subsystems/learning.md。
package api

// Name 是子系统名。
const Name = "learning"

// Error 是可在传输边界暴露的错误（稳定 code）；基础设施错误不直接暴露。
type Error struct {
	Status int
	Code   string
	Detail string
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Code + "：" + e.Detail
	}
	return e.Code
}
