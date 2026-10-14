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
	ID             string   `yaml:"id"`
	Say            string   `yaml:"say"`
	When           string   `yaml:"when"`
	Missing        string   `yaml:"missing"` // require：reject / review / ignore，默认 review
	OnFail         string   `yaml:"on_fail"` // require：no_signal / excluded，默认 no_signal
	Reason         string   `yaml:"reason"`
	Weight         int      `yaml:"weight"`
	IgnoreRequires []string `yaml:"ignore_requires"`
	Locked         bool     `yaml:"locked"`
}

// Scope 是适用范围。
type Scope struct {
	FactTypes []string `yaml:"fact_types"`
	When      string   `yaml:"when"`
}

// ParamDef 是模板参数定义。
type ParamDef struct {
	Type        string   `yaml:"type"` // int、double、bool、string、list<…>
	Default     any      `yaml:"default"`
	DefaultFrom string   `yaml:"default_from"` // 例如 profile.offering_categories
	Min         *float64 `yaml:"min"`
	Max         *float64 `yaml:"max"`
	Say         string   `yaml:"say"`
}

// FieldEvidence 是字段可被规则使用的证据门槛。
type FieldEvidence struct {
	MinConfidence float64  `yaml:"min_confidence"`
	Methods       []string `yaml:"methods"`
}

// EvidencePolicy 是证据门槛：default 适用于全部带证据的字段，fields 按视图路径覆盖。
type EvidencePolicy struct {
	Default *FieldEvidence           `yaml:"default"`
	Fields  map[string]FieldEvidence `yaml:"fields"`
}

// Thresholds 是评分到处置的阈值。
type Thresholds struct {
	BaseScore int `yaml:"base_score"`
	PursueAt  int `yaml:"pursue_at"`
	DropBelow int `yaml:"drop_below"`
}

// Route 是某一处置的后续动作建议，原样输出。
type Route map[string]any

// Example 是带期望处置的样例；given 为内联视图（模板自测）。
type Example struct {
	Name     string         `yaml:"name"`
	FactType string         `yaml:"fact_type"` // 缺省取 scope 的第一个事实类型
	Given    map[string]any `yaml:"given"`
	Expect   Disposition    `yaml:"expect"`
}

// Policy 是合成后的完整策略（第 3.3 节）。模板与租户策略都以此结构求值。
type Policy struct {
	ID             string              `yaml:"id"`
	Version        string              `yaml:"version"`
	Objective      string              `yaml:"objective"`
	Roles          []string            `yaml:"roles"`
	FactTypes      []string            `yaml:"fact_types"`
	RolloutKey     string              `yaml:"rollout_key"`
	View           []string            `yaml:"view"`
	Params         map[string]ParamDef `yaml:"params"`
	ParamValues    map[string]any      `yaml:"-"` // 租户填写的参数值；未填写时取 default_from 或 default
	Scope          Scope               `yaml:"scope"`
	Require        []Rule              `yaml:"require"`
	Exclude        []Rule              `yaml:"exclude"`
	Watch          []Rule              `yaml:"watch"`
	Boost          []Rule              `yaml:"boost"`
	Penalize       []Rule              `yaml:"penalize"`
	EvidencePolicy EvidencePolicy      `yaml:"evidence_policy"`
	Thresholds     Thresholds          `yaml:"thresholds"`
	Route          map[string]Route    `yaml:"route"`
	Examples       []Example           `yaml:"examples"`
}

// Feature 是应用领域包声明的特征函数（第 5.3 节）。
type Feature struct {
	Type string   `yaml:"type"` // int、double、bool、string
	Say  string   `yaml:"say"`
	View []string `yaml:"view"`
	Expr string   `yaml:"expr"`
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
		Features map[string]Feature `yaml:"features"`
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
			Template *Policy `yaml:"template"`
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
