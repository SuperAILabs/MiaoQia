// Package gateway 实现 ai-platform 的模型网关骨架：模型目录与路由、主备切换、预算上限、输出校验与计量。
// 供应商适配器只有故障桩；真实供应商在 M1 首次调用前按 C3/C7a 评测选定后接入（ADR-0015）。
package gateway

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/superailabs/miaoqia/services/subsystems/aiplatform/api"
)

// Endpoint 是模型目录中的一个可调用端点。Region 是登记的处理地域。
// 价格按 Billing 声明的单位登记：token 计费按每千 token 的微元，按请求计费按每次请求的微元；未登记价格的端点不能通过校验，不按免费处理。
type Endpoint struct {
	ID               string           `yaml:"id"`
	Provider         string           `yaml:"provider"` // 适配器名，例如 openai_compatible、stub
	Model            string           `yaml:"model"`
	Version          string           `yaml:"version"`
	Capabilities     []api.Capability `yaml:"capabilities"`
	Filed            bool             `yaml:"filed"` // 已完成模型备案
	Region           string           `yaml:"region"`
	Tier             int              `yaml:"tier"` // 0 主选，1 起为备选，同级内按 ID 排序
	Billing          api.BillingUnit  `yaml:"billing"`
	InputPricePer1K  int64            `yaml:"input_price_per_1k"`
	OutputPricePer1K int64            `yaml:"output_price_per_1k"`
	RequestPrice     int64            `yaml:"request_price"`
	DecisionTasks    []DecisionTask   `yaml:"decision_tasks"` // 声明 classify/score 能力的端点经任务评测准入的任务版本
	MaxInputTokens   int              `yaml:"max_input_tokens"`
	// RequestOverheadTokens 是适配器在请求内容之外附加的 token 上界（模板、角色与格式标记），计入输入估算。
	RequestOverheadTokens int           `yaml:"request_overhead_tokens"`
	Timeout               time.Duration `yaml:"timeout"`
	NoTrainingOnCustomer  bool          `yaml:"no_training_on_customer"` // 合同与配置禁止用客户数据训练
}

// DecisionTask 是端点对一个决策任务版本的准入声明。同一任务版本的所有端点必须声明相同的能力与数值含义，
// 主备切换因此只在输出语义可比的端点之间发生。阈值不在目录中登记，由任务版本的所有者按评测结果设定。
type DecisionTask struct {
	Task         string           `yaml:"task"`
	Version      string           `yaml:"version"`
	Capability   api.Capability   `yaml:"capability"`
	ScoreMeaning api.ScoreMeaning `yaml:"score_meaning"`
}

func (d DecisionTask) key() string { return d.Task + "@" + d.Version }

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
		case e.InputPricePer1K < 0 || e.OutputPricePer1K < 0 || e.RequestPrice < 0 || e.Tier < 0 || e.MaxInputTokens < 0 || e.RequestOverheadTokens < 0 || e.Timeout < 0:
			errs = append(errs, fmt.Errorf("端点 %q 的价格、级别、上下文或时限为负", e.ID))
		default:
			errs = append(errs, e.validatePricing(), e.validateDecisionTasks())
		}
		seen[e.ID] = true
	}
	// 同一任务版本在所有端点上的能力与数值含义一致，避免主备切换到不可比的结果。
	decl := map[string]DecisionTask{}
	for _, e := range c.Endpoints {
		for _, d := range e.DecisionTasks {
			if prev, ok := decl[d.key()]; ok && (prev.Capability != d.Capability || prev.ScoreMeaning != d.ScoreMeaning) {
				errs = append(errs, fmt.Errorf("任务 %s 在端点间声明的能力或数值含义不一致", d.key()))
			}
			decl[d.key()] = d
		}
	}
	return errors.Join(errs...)
}

func (e Endpoint) validatePricing() error {
	switch e.Billing {
	case api.BillToken:
		if e.RequestPrice != 0 || e.InputPricePer1K+e.OutputPricePer1K == 0 {
			return fmt.Errorf("端点 %q 按 token 计费，须登记 token 价格且不登记请求价格", e.ID)
		}
	case api.BillRequest:
		if e.RequestPrice <= 0 || e.InputPricePer1K != 0 || e.OutputPricePer1K != 0 {
			return fmt.Errorf("端点 %q 按请求计费，须登记请求价格且不登记 token 价格", e.ID)
		}
	default:
		return fmt.Errorf("端点 %q 未声明计费单位（token 或 request）", e.ID)
	}
	return nil
}

func (e Endpoint) validateDecisionTasks() error {
	var errs []error
	seen := map[string]bool{}
	for _, d := range e.DecisionTasks {
		switch {
		case d.Task == "" || d.Version == "":
			errs = append(errs, fmt.Errorf("端点 %q 的决策任务缺少 task/version", e.ID))
		case seen[d.key()]:
			errs = append(errs, fmt.Errorf("端点 %q 重复声明任务 %s", e.ID, d.key()))
		case !e.has(d.Capability) || !d.Capability.Decision():
			errs = append(errs, fmt.Errorf("端点 %q 的任务 %s 须对应已声明的 classify 或 score 能力", e.ID, d.key()))
		case !validMeaning(d.Capability, d.ScoreMeaning):
			errs = append(errs, fmt.Errorf("端点 %q 的任务 %s 数值含义 %q 不适用于 %s", e.ID, d.key(), d.ScoreMeaning, d.Capability))
		}
		seen[d.key()] = true
	}
	for _, c := range []api.Capability{api.CapClassify, api.CapScore} {
		if e.has(c) && !slices.ContainsFunc(e.DecisionTasks, func(d DecisionTask) bool { return d.Capability == c }) {
			errs = append(errs, fmt.Errorf("端点 %q 声明了 %s 能力但没有登记准入的任务版本", e.ID, c))
		}
	}
	return errors.Join(errs...)
}

func validMeaning(c api.Capability, m api.ScoreMeaning) bool {
	switch c {
	case api.CapClassify:
		return m == api.MeaningNone || m == api.MeaningProbability || m == api.MeaningConfidence
	case api.CapScore:
		return m == api.MeaningScale
	}
	return false
}

// decisionTask 返回端点对该任务版本的准入声明。
func (e Endpoint) decisionTask(key string) (DecisionTask, bool) {
	for _, d := range e.DecisionTasks {
		if d.key() == key {
			return d, true
		}
	}
	return DecisionTask{}, false
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

// estimate 返回调用前的费用上界。按 token 计费且输出有价格时，调用方必须给出输出上限，否则费用无法限定，端点不参与本次调用。
// 输入为负或费用超出 int64 可表示范围时同样无法估算，端点不参与。
func (e Endpoint) estimate(in, maxOut int) (int64, bool) {
	if in < 0 || maxOut < 0 {
		return 0, false
	}
	if e.Billing == api.BillRequest {
		return e.RequestPrice, true
	}
	if maxOut == 0 && e.OutputPricePer1K > 0 {
		return 0, false
	}
	return e.tokenCost(in, maxOut)
}

// inputUpper 返回本端点的输入 token 上界：请求内容的保守估算加适配器附加量，溢出时取最大值（必然超出上限）。
func (e Endpoint) inputUpper(payload int) int {
	if payload > math.MaxInt-e.RequestOverheadTokens {
		return math.MaxInt
	}
	return payload + e.RequestOverheadTokens
}

// usage 把供应商报告的用量换算为费用。按请求计费的成功调用至少计一次请求；失败调用按供应商报告的计费请求数。
// 用量为负或费用超出可表示范围时返回 false：费用未知，由调用方按估算上界扣减预算，不当作免费。
func (e Endpoint) usage(pu ProviderUsage, ok bool) (api.Usage, bool) {
	u := api.Usage{Unit: e.Billing, InputTokens: pu.InputTokens, OutputTokens: pu.OutputTokens, Requests: pu.Requests}
	if pu.InputTokens < 0 || pu.OutputTokens < 0 || pu.Requests < 0 {
		return api.Usage{Unit: e.Billing}, false
	}
	var cost int64
	var fits bool
	if e.Billing == api.BillRequest {
		if ok && u.Requests == 0 {
			u.Requests = 1
		}
		cost, fits = mulInt64(int64(u.Requests), e.RequestPrice)
	} else {
		cost, fits = e.tokenCost(pu.InputTokens, pu.OutputTokens)
	}
	if !fits {
		u.CostMicroYuan = 0
		return u, false
	}
	u.CostMicroYuan = cost
	return u, true
}

// tokenCost 按每千 token 价格向上取整到微元；乘法、取整与求和都检查溢出，超出 int64 时返回 false。
func (e Endpoint) tokenCost(in, out int) (int64, bool) {
	ci, ok1 := mulCeilDiv(int64(in), e.InputPricePer1K, 1000)
	co, ok2 := mulCeilDiv(int64(out), e.OutputPricePer1K, 1000)
	if !ok1 || !ok2 || ci > math.MaxInt64-co {
		return 0, false
	}
	return ci + co, true
}

// mulCeilDiv 计算 ceil(a*b/d)，a、b 非负、d 为正；用 128 位中间结果，不会因乘法或取整加法溢出。
func mulCeilDiv(a, b, d int64) (int64, bool) {
	if a < 0 || b < 0 || d <= 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi >= uint64(d) {
		return 0, false // 商超过 64 位
	}
	q, r := bits.Div64(hi, lo, uint64(d))
	if r != 0 {
		q++ // q < 2^64/d 时加 1 不回绕；越界由下面的范围检查捕获
		if q == 0 {
			return 0, false
		}
	}
	if q > math.MaxInt64 {
		return 0, false
	}
	return int64(q), true
}

// mulInt64 计算 a*b（a、b 非负），溢出时返回 false。
func mulInt64(a, b int64) (int64, bool) { return mulCeilDiv(a, b, 1) }

// addSat 是饱和加法（a、b 非负），用于累计已发生费用的上界。
func addSat(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
