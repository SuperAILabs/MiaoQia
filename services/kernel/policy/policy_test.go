package policy

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"go.yaml.in/yaml/v3"
)

func repoRoot(t testing.TB) string {
	t.Helper()
	dir, _ := os.Getwd()
	for ; dir != "/"; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "contracts", "identifiers.yaml")); err == nil {
			return dir
		}
	}
	t.Fatal("找不到仓库根目录")
	return ""
}

func loadTendering(t testing.TB) *Pack {
	t.Helper()
	p, err := LoadPack(filepath.Join(repoRoot(t), "packs", "application_domains", "tendering"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// 全部 R1 模板编译通过，样例得出期望处置。
func TestTemplateExamples(t *testing.T) {
	pack := loadTendering(t)
	if len(pack.Templates) != 5 {
		t.Fatalf("期望 5 个 R1 模板，得到 %d", len(pack.Templates))
	}
	for id, tpl := range pack.Templates {
		c, issues := Compile(tpl, pack.Features, DefaultLimits())
		if issues != nil {
			t.Errorf("%s 编译失败：%v", id, issues)
			continue
		}
		for _, ex := range tpl.Examples {
			in, err := tpl.ExampleInput(ex)
			if err != nil {
				t.Fatal(err)
			}
			r, err := c.Evaluate(in)
			if err != nil {
				t.Errorf("%s「%s」：%v", id, ex.Name, err)
				continue
			}
			if r.Disposition != ex.Expect {
				t.Errorf("%s「%s」：期望 %s，得到 %s（得分 %d）\n%s", id, ex.Name, ex.Expect, r.Disposition, r.Score, traceText(r))
			}
			if len(r.Alerts) > 0 {
				t.Errorf("%s「%s」出现告警：%v", id, ex.Name, r.Alerts)
			}
		}
	}
}

func traceText(r *Result) string {
	s := ""
	for _, t := range r.Trace {
		s += "  " + t.ID + " " + t.Result.String()
		if len(t.Unknown) > 0 {
			s += " 未知字段=" + joinStr(t.Unknown)
		}
		if t.Error != "" {
			s += " 错误=" + t.Error
		}
		s += "\n"
	}
	return s
}

func joinStr(a []string) string {
	out := ""
	for i, x := range a {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

// Go 实现与 contracts/policy/cel-environment.yaml 一致。
func TestEnvironmentMatchesContract(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "contracts", "policy", "cel-environment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var decl struct {
		Variables map[string]any `yaml:"variables"`
		Functions map[string]struct {
			Args []string `yaml:"args"`
		} `yaml:"functions"`
		SizeBounds struct {
			DefaultList   uint64            `yaml:"default_list"`
			DefaultString uint64            `yaml:"default_string"`
			Paths         map[string]uint64 `yaml:"paths"`
		} `yaml:"size_bounds"`
		Limits struct {
			RulesPerPolicy        int    `yaml:"rules_per_policy"`
			ExpressionLength      int    `yaml:"expression_length"`
			ExpressionStaticCost  uint64 `yaml:"expression_static_cost"`
			EvaluationRuntimeCost uint64 `yaml:"evaluation_runtime_cost"`
		} `yaml:"limits"`
	}
	if err := yaml.Unmarshal(b, &decl); err != nil {
		t.Fatal(err)
	}
	var vars []string
	for k := range decl.Variables {
		vars = append(vars, k)
	}
	sort.Strings(vars)
	want := append([]string(nil), variableNames...)
	sort.Strings(want)
	if !reflect.DeepEqual(vars, want) {
		t.Errorf("变量不一致：契约 %v，实现 %v", vars, want)
	}
	fns := map[string]int{}
	for k, f := range decl.Functions {
		fns[k] = len(f.Args)
	}
	if !reflect.DeepEqual(fns, platformFunctions) {
		t.Errorf("平台函数不一致：契约 %v，实现 %v", fns, platformFunctions)
	}
	if decl.SizeBounds.DefaultList != defaultListBound || decl.SizeBounds.DefaultString != defaultStringBound ||
		!reflect.DeepEqual(decl.SizeBounds.Paths, pathBounds) {
		t.Errorf("规模上界不一致")
	}
	l := DefaultLimits()
	if decl.Limits.RulesPerPolicy != l.RulesPerPolicy || decl.Limits.ExpressionLength != l.ExpressionLength ||
		decl.Limits.ExpressionStaticCost != l.ExpressionStaticCost || decl.Limits.EvaluationRuntimeCost != l.EvaluationCost {
		t.Errorf("限制不一致：契约 %+v，实现 %+v", decl.Limits, l)
	}
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	for name := range platformFunctions {
		if _, ok := env.Functions()[name]; !ok {
			t.Errorf("环境缺少函数 %s", name)
		}
	}
}

var report = flag.Bool("report", false, "输出成本报告")

// 合成后的策略以 JSON 存储（customer-context 的策略版本）：JSON 往返后编译与样例结果不变（含取值为 0 的默认值）。
func TestPolicyJSONRoundTrip(t *testing.T) {
	pack := loadTendering(t)
	for id, tpl := range pack.Templates {
		raw, err := json.Marshal(tpl)
		if err != nil {
			t.Fatal(err)
		}
		var back Policy
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		c, issues := Compile(&back, pack.Features, DefaultLimits())
		if issues != nil {
			t.Fatalf("%s：%v", id, issues)
		}
		for _, ex := range back.Examples {
			in, err := back.ExampleInput(ex)
			if err != nil {
				t.Fatal(err)
			}
			r, err := c.Evaluate(in)
			if err != nil || r.Disposition != ex.Expect {
				t.Errorf("%s「%s」：期望 %s，得到 %v %v", id, ex.Name, ex.Expect, r, err)
			}
		}
	}
}
