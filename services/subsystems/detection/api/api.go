// Package api 是 detection 的公开接口（其他子系统唯一可导入的部分）。规格见 docs/03-architecture/subsystems/detection.md。
package api

// Name 是子系统名。
const Name = "detection"

// Error 是可在传输边界暴露的错误（稳定 code）；基础设施错误不直接暴露。Current 是冲突与 signal_updated 时信号的当前值
// （契约 SignalState 的 JSON），其余情况为空。
type Error struct {
	Status  int
	Code    string
	Detail  string
	Current []byte
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Code + "：" + e.Detail
	}
	return e.Code
}
