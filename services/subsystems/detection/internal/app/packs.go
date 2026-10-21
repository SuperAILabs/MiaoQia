package app

import (
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"

	"github.com/superailabs/miaoqia/services/kernel/policy"
)

// Reason 是一个反馈原因标签（应用领域包 feedback/*.yaml）。
type Reason struct {
	Code         string `yaml:"code"`
	Say          string `yaml:"say"`
	NoteRequired bool   `yaml:"note_required"`
}

// FeedbackReasons 是一个目标的反馈原因标签，按反馈结果分组（exclusion_correct 没有原因标签）。
type FeedbackReasons struct {
	Objective      string   `yaml:"objective"`
	Irrelevant     []Reason `yaml:"irrelevant"`
	Relevant       []Reason `yaml:"relevant"`
	ExclusionWrong []Reason `yaml:"exclusion_wrong"`
}

func (f *FeedbackReasons) of(result string) []Reason {
	switch result {
	case "irrelevant":
		return f.Irrelevant
	case "relevant":
		return f.Relevant
	case "exclusion_wrong":
		return f.ExclusionWrong
	}
	return nil
}

// Packs 是 detection 使用的应用领域包内容：共享特征、各事实类型视图的重新求值触发事件、各目标的反馈原因标签。
type Packs struct {
	Features map[string]policy.Feature
	Triggers map[string][]string         // 事实类型 → 触发重新求值的领域事件（缺省为空：只在来源事实首次发布时求值）
	Feedback map[string]*FeedbackReasons // 目标编号（如 APP-TENDER.A02）→ 原因标签
}

// LoadPacks 读取 dir 下全部应用领域包。
func LoadPacks(dir string) (*Packs, error) {
	out := &Packs{Features: map[string]policy.Feature{}, Triggers: map[string][]string{}, Feedback: map[string]*FeedbackReasons{}}
	apps, err := filepath.Glob(filepath.Join(dir, "application_domains", "*", "pack.yaml"))
	if err != nil {
		return nil, err
	}
	for _, manifest := range apps {
		root := filepath.Dir(manifest)
		pack, err := policy.LoadPack(root)
		if err != nil {
			return nil, fmt.Errorf("%s：%w", manifest, err)
		}
		for name, f := range pack.Features {
			if _, dup := out.Features[name]; dup {
				return nil, fmt.Errorf("%s：特征 %s 重复", manifest, name)
			}
			out.Features[name] = f
		}
		var views struct {
			FactTypes map[string]struct {
				Triggers []string `yaml:"triggers"`
			} `yaml:"fact_types"`
		}
		if err := readYAML(filepath.Join(root, "policy", "views.yaml"), &views); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for t, v := range views.FactTypes {
			out.Triggers[t] = v.Triggers
		}
		files, err := filepath.Glob(filepath.Join(root, "feedback", "*.yaml"))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			var doc struct {
				Feedback *FeedbackReasons `yaml:"feedback"`
			}
			if err := readYAML(f, &doc); err != nil {
				return nil, err
			}
			if doc.Feedback == nil || doc.Feedback.Objective == "" {
				return nil, fmt.Errorf("%s：缺少 feedback.objective", f)
			}
			out.Feedback[doc.Feedback.Objective] = doc.Feedback
		}
	}
	return out, nil
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
