package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"go.yaml.in/yaml/v3"
)

// Clause 是规则所属的子句。
type Clause string

const (
	ClauseRequire  Clause = "require"
	ClauseExclude  Clause = "exclude"
	ClauseWatch    Clause = "watch"
	ClauseBoost    Clause = "boost"
	ClausePenalize Clause = "penalize"
)

// Disposition 是处置结果。
type Disposition string

const (
	NoSignal        Disposition = "no_signal"
	Excluded        Disposition = "excluded"
	WatchSignal     Disposition = "watch_signal"
	ReviewCandidate Disposition = "review_candidate"
	PursueCandidate Disposition = "pursue_candidate"
)

// Rule 是一条规则（第 3.2 节）。
type Rule struct {
	ID             string   `yaml:"id" json:"id,omitempty"`
	Say            string   `yaml:"say" json:"say,omitempty"`
	When           string   `yaml:"when" json:"when,omitempty"`
	Missing        string   `yaml:"missing" json:"missing,omitempty"` // require：reject / review / ignore，默认 review
	OnFail         string   `yaml:"on_fail" json:"on_fail,omitempty"` // require：no_signal / excluded，默认 no_signal
	Reason         string   `yaml:"reason" json:"reason,omitempty"`
	Weight         int      `yaml:"weight" json:"weight,omitempty"`
	IgnoreRequires []string `yaml:"ignore_requires" json:"ignore_requires,omitempty"`
	Locked         bool     `yaml:"locked" json:"locked,omitempty"`
}

// Scope 是适用范围。
type Scope struct {
	FactTypes []string `yaml:"fact_types" json:"fact_types,omitempty"`
	When      string   `yaml:"when" json:"when,omitempty"`
}

// ParamDef 是模板参数定义。
type ParamDef struct {
	Type        string   `yaml:"type" json:"type,omitempty"` // int、double、bool、string、list<…>
	Default     any      `yaml:"default" json:"default"`
	DefaultFrom string   `yaml:"default_from" json:"default_from,omitempty"` // 例如 profile.offering_categories
	Min         *float64 `yaml:"min" json:"min,omitempty"`
	Max         *float64 `yaml:"max" json:"max,omitempty"`
	Say         string   `yaml:"say" json:"say,omitempty"`
}

// FieldEvidence 是字段可被规则使用的证据门槛。
type FieldEvidence struct {
	MinConfidence float64  `yaml:"min_confidence" json:"min_confidence,omitempty"`
	Methods       []string `yaml:"methods" json:"methods,omitempty"`
}

// EvidencePolicy 是证据门槛：default 适用于全部带证据的字段，fields 按视图路径覆盖。
type EvidencePolicy struct {
	Default *FieldEvidence           `yaml:"default" json:"default"`
	Fields  map[string]FieldEvidence `yaml:"fields" json:"fields,omitempty"`
}

// Thresholds 是评分到处置的阈值。
type Thresholds struct {
	BaseScore int `yaml:"base_score" json:"base_score"`
	PursueAt  int `yaml:"pursue_at" json:"pursue_at"`
	DropBelow int `yaml:"drop_below" json:"drop_below"`
}

// Route 是某一处置的后续动作建议，原样输出。
type Route map[string]any

// Example 是带期望处置的样例；given 为内联视图（模板自测）。
type Example struct {
	Name     string         `yaml:"name" json:"name,omitempty"`
	FactType string         `yaml:"fact_type" json:"fact_type,omitempty"` // 缺省取 scope 的第一个事实类型
	Given    map[string]any `yaml:"given" json:"given,omitempty"`
	Expect   Disposition    `yaml:"expect" json:"expect,omitempty"`
}

// Policy 是合成后的完整策略（第 3.3 节）。模板与租户策略都以此结构求值。
type Policy struct {
	ID             string              `yaml:"id" json:"id,omitempty"`
	Version        string              `yaml:"version" json:"version,omitempty"`
	Objective      string              `yaml:"objective" json:"objective,omitempty"`
	Roles          []string            `yaml:"roles" json:"roles,omitempty"`
	FactTypes      []string            `yaml:"fact_types" json:"fact_types,omitempty"`
	RolloutKey     string              `yaml:"rollout_key" json:"rollout_key,omitempty"`
	View           []string            `yaml:"view" json:"view,omitempty"`
	Params         map[string]ParamDef `yaml:"params" json:"params,omitempty"`
	ParamValues    map[string]any      `yaml:"-" json:"param_values,omitempty"` // 租户填写的参数值；未填写时取 default_from 或 default
	Scope          Scope               `yaml:"scope" json:"scope,omitempty"`
	Require        []Rule              `yaml:"require" json:"require,omitempty"`
	Exclude        []Rule              `yaml:"exclude" json:"exclude,omitempty"`
	Watch          []Rule              `yaml:"watch" json:"watch,omitempty"`
	Boost          []Rule              `yaml:"boost" json:"boost,omitempty"`
	Penalize       []Rule              `yaml:"penalize" json:"penalize,omitempty"`
	EvidencePolicy EvidencePolicy      `yaml:"evidence_policy" json:"evidence_policy,omitempty"`
	Thresholds     Thresholds          `yaml:"thresholds" json:"thresholds,omitempty"`
	Route          map[string]Route    `yaml:"route" json:"route,omitempty"`
	Examples       []Example           `yaml:"examples" json:"examples,omitempty"`
}

// Feature 是应用领域包声明的特征函数（第 5.3 节）。
type Feature struct {
	Type string   `yaml:"type" json:"type,omitempty"` // int、double、bool、string
	Say  string   `yaml:"say" json:"say,omitempty"`
	View []string `yaml:"view" json:"view,omitempty"`
	Expr string   `yaml:"expr" json:"expr,omitempty"`
}

// Pack 是应用领域包的策略部分：模板与共享特征。
type Pack struct {
	Templates map[string]*Policy // 键为模板标识
	Features  map[string]Feature
}

// LoadPack 读取应用领域包目录下的 policy/templates/*.yaml 与 policy/features.yaml。
func LoadPack(dir string) (*Pack, error) {
	p := &Pack{Templates: map[string]*Policy{}, Features: map[string]Feature{}}
	var ff struct {
		Features map[string]Feature `yaml:"features" json:"features,omitempty"`
	}
	if err := readYAML(filepath.Join(dir, "policy", "features.yaml"), &ff); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if ff.Features != nil {
		p.Features = ff.Features
	}
	files, err := filepath.Glob(filepath.Join(dir, "policy", "templates", "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	for _, f := range files {
		var tf struct {
			Template *Policy `yaml:"template" json:"template,omitempty"`
		}
		if err := readYAML(f, &tf); err != nil {
			return nil, err
		}
		if tf.Template == nil || tf.Template.ID == "" {
			return nil, fmt.Errorf("%s：缺少 template.id", f)
		}
		p.Templates[tf.Template.ID] = tf.Template
	}
	return p, nil
}

func readYAML(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s：%w", path, err)
	}
	return nil
}

// rules 按子句顺序返回全部规则。
func (p *Policy) rules() []struct {
	Clause Clause
	Rule   *Rule
} {
	var out []struct {
		Clause Clause
		Rule   *Rule
	}
	for _, c := range []struct {
		clause Clause
		rules  []Rule
	}{{ClauseRequire, p.Require}, {ClauseExclude, p.Exclude}, {ClauseWatch, p.Watch}, {ClauseBoost, p.Boost}, {ClausePenalize, p.Penalize}} {
		for i := range c.rules {
			out = append(out, struct {
				Clause Clause
				Rule   *Rule
			}{c.clause, &c.rules[i]})
		}
	}
	return out
}
