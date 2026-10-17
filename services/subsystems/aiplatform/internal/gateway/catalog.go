// Package gateway 实现 ai-platform 的模型网关骨架：模型目录与路由、主备切换、预算上限、输出校验与计量。
// 供应商适配器只有故障桩；真实供应商在 M1 首次调用前按 C3/C7a 评测选定后接入（ADR-0015）。
package gateway

import (
	"errors"
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/superailabs/miaoqia/services/subsystems/aiplatform/api"
)

// Endpoint 是模型目录中的一个可调用端点。价格按每千 token 的微元计；Region 是登记的处理地域。
type Endpoint struct {
	ID                   string           `yaml:"id"`
	Provider             string           `yaml:"provider"` // 适配器名，例如 openai_compatible、stub
	Model                string           `yaml:"model"`
	Version              string           `yaml:"version"`
	Capabilities         []api.Capability `yaml:"capabilities"`
	Filed                bool             `yaml:"filed"` // 已完成模型备案
	Region               string           `yaml:"region"`
	Tier                 int              `yaml:"tier"` // 0 主选，1 起为备选，同级内按 ID 排序
	InputPricePer1K      int64            `yaml:"input_price_per_1k"`
	OutputPricePer1K     int64            `yaml:"output_price_per_1k"`
	MaxInputTokens       int              `yaml:"max_input_tokens"`
	Timeout              time.Duration    `yaml:"timeout"`
	NoTrainingOnCustomer bool             `yaml:"no_training_on_customer"` // 合同与配置禁止用客户数据训练
}

// Catalog 是模型目录与路由约束。AllowedRegions 为空时只允许境内（cn）处理；境外地域须显式列入，且只在完成跨境评估后配置。
type Catalog struct {
	AllowedRegions []string   `yaml:"allowed_regions"`
	Endpoints      []Endpoint `yaml:"endpoints"`
}

// ParseCatalog 解析并校验 YAML 模型目录。
func ParseCatalog(raw []byte) (*Catalog, error) {
	var c Catalog
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("模型目录: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate 拒绝不完整或不合规的端点登记，避免路由时才发现。
func (c *Catalog) Validate() error {
	if len(c.AllowedRegions) == 0 {
		c.AllowedRegions = []string{"cn"}
	}
	seen := map[string]bool{}
	var errs []error
	for _, e := range c.Endpoints {
		switch {
		case e.ID == "" || e.Provider == "" || e.Model == "" || e.Version == "":
			errs = append(errs, fmt.Errorf("端点 %q 缺少 id/provider/model/version", e.ID))
		case seen[e.ID]:
			errs = append(errs, fmt.Errorf("端点 %q 重复", e.ID))
		case len(e.Capabilities) == 0:
			errs = append(errs, fmt.Errorf("端点 %q 未声明能力", e.ID))
		case e.Region == "":
			errs = append(errs, fmt.Errorf("端点 %q 未登记处理地域", e.ID))
		case !e.NoTrainingOnCustomer:
			errs = append(errs, fmt.Errorf("端点 %q 未确认禁止用客户数据训练", e.ID))
		case e.InputPricePer1K < 0 || e.OutputPricePer1K < 0 || e.Tier < 0 || e.MaxInputTokens < 0 || e.Timeout < 0:
			errs = append(errs, fmt.Errorf("端点 %q 的价格、级别、上下文或时限为负", e.ID))
		}
		seen[e.ID] = true
	}
	return errors.Join(errs...)
}

func (e Endpoint) has(c api.Capability) bool {
	for _, x := range e.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

func (e Endpoint) ref() api.ModelRef {
	return api.ModelRef{Endpoint: e.ID, Provider: e.Provider, Model: e.Model, Version: e.Version}
}

// cost 按每千 token 价格向上取整到微元。
func (e Endpoint) cost(in, out int) int64 {
	return ceilDiv(int64(in)*e.InputPricePer1K, 1000) + ceilDiv(int64(out)*e.OutputPricePer1K, 1000)
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }
