package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"go.yaml.in/yaml/v3"
)

// Section 是应用领域包声明的一个简报区（briefing.yaml）：目标编号、区顺序、标题与每区条数（缺省值与成员可调范围）。
type Section struct {
	Objective string `yaml:"objective"`
	Order     int    `yaml:"order"`
	Title     string `yaml:"title"`
	Quota     struct {
		Default int `yaml:"default"`
		Min     int `yaml:"min"`
		Max     int `yaml:"max"`
	} `yaml:"quota"`
}

// QuickReason 是卡片上“不相关”可选的原因（反馈标签的前 quick 个，第13节第5项）。
type QuickReason struct {
	Code string `yaml:"code"`
	Say  string `yaml:"say"`
}

// Packs 是 channels 使用的应用领域包内容：简报定义（来源标识、版本、各区）与各目标卡片上的“不相关”快捷原因。
type Packs struct {
	Source   string
	Version  string
	Sections map[string]Section       // 目标编号 → 区
	Quick    map[string][]QuickReason // 目标编号 → 前 quick 个不相关原因（不含“其他”）
}

// Limits 返回各目标区条数的可调范围（偏好校验）。
func (p *Packs) Limits() map[string][2]int {
	out := map[string][2]int{}
	for code, s := range p.Sections {
		out[code] = [2]int{s.Quota.Min, s.Quota.Max}
	}
	return out
}

// order 返回目标区的顺序：包中声明的顺序在前，未声明的目标排在其后（按编号）。
func (p *Packs) order(code string) int {
	if s, ok := p.Sections[code]; ok {
		return s.Order
	}
	return 1000
}

// LoadPacks 读取 dir 下应用领域包的简报定义与反馈原因。多个应用领域包各自声明简报时取第一个（按目录名）为日常简报来源。
func LoadPacks(dir string) (*Packs, error) {
	out := &Packs{Sections: map[string]Section{}, Quick: map[string][]QuickReason{}}
	briefings, err := filepath.Glob(filepath.Join(dir, "application_domains", "*", "briefing.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(briefings)
	for _, path := range briefings {
		var doc struct {
			Briefing struct {
				ID       string    `yaml:"id"`
				Version  string    `yaml:"version"`
				Sections []Section `yaml:"sections"`
			} `yaml:"briefing"`
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("%s：%w", path, err)
		}
		if out.Source == "" {
			out.Source, out.Version = doc.Briefing.ID, doc.Briefing.Version
		}
		for _, s := range doc.Briefing.Sections {
			if s.Quota.Default < s.Quota.Min || s.Quota.Default > s.Quota.Max || s.Quota.Min < 1 {
				return nil, fmt.Errorf("%s：%s 的条数声明无效", path, s.Objective)
			}
			out.Sections[s.Objective] = s
		}
	}
	feedback, err := filepath.Glob(filepath.Join(dir, "application_domains", "*", "feedback", "*.yaml"))
	if err != nil {
		return nil, err
	}
	for _, path := range feedback {
		var doc struct {
			Feedback struct {
				Objective  string        `yaml:"objective"`
				Quick      int           `yaml:"quick"`
				Irrelevant []QuickReason `yaml:"irrelevant"`
			} `yaml:"feedback"`
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("%s：%w", path, err)
		}
		var quick []QuickReason
		for _, r := range doc.Feedback.Irrelevant {
			if len(quick) == doc.Feedback.Quick {
				break
			}
			if r.Code != "OTHER" {
				quick = append(quick, r)
			}
		}
		out.Quick[doc.Feedback.Objective] = quick
	}
	if out.Source == "" {
		out.Source, out.Version = "BRF-DAILY", "0.0.0"
	}
	return out, nil
}
