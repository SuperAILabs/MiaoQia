package api

import "context"

// PercolatorBinding 是 detection 使用的订阅反查接口。
const PercolatorBinding = "search.Percolator"

// FactUnit 是 search 反查所需的中性事实投影；search 不依赖 detection 的领域类型。
type FactUnit struct {
	FactType   string         `json:"fact_type"`
	Domain     string         `json:"domain"`
	Fields     map[string]any `json:"fields"`
	Region     string         `json:"region"`
	Categories []string       `json:"categories"`
}

// Fact 是反查输入。Units 为 nil 时是不读事实内容的粗筛（只按 cell）；非 nil 时按单位过滤。
type Fact struct {
	Cell       string
	FactID     string
	Version    int64
	ObjectType string
	InfoDomain string
	Units      []FactUnit
}

// WatchQuery 是策略编译后的最小反查条件。注册与删除由 detection 专用命令调用。
type WatchQuery struct {
	ID         string   `json:"id"`
	Tenant     string   `json:"tenant"`
	Cell       string   `json:"cell"`
	FactTypes  []string `json:"fact_types"`
	Regions    []string `json:"regions"`
	Categories []string `json:"categories"`
}

type Percolator interface {
	Register(context.Context, WatchQuery) error
	Remove(context.Context, string) error
	Percolate(context.Context, Fact) ([]string, error)
}
