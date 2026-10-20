// Package infra 是 customer-context 的外部适配：从扩展包目录读取目标配置与档案校验所需的定义。
package infra

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"go.yaml.in/yaml/v3"

	"github.com/superailabs/miaoqia/services/kernel/policy"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/internal/domain"
)

// LoadCatalog 读取 dir（仓库的 packs/）下全部应用领域包的角色模板、目标实例、覆盖矩阵与策略模板参数，
// 以及全部行业包的品类分类与资质模板。格式见 contracts/packs/，内容由 mqc P9、P10、I1–I5 校验，这里只读取所需字段。
func LoadCatalog(dir string) (*domain.Catalog, error) {
	c := &domain.Catalog{Roles: map[string]domain.Role{}, PolicyTemplates: map[string]domain.PolicyTemplate{}, Industries: map[string]domain.Industry{}}
	apps, err := filepath.Glob(filepath.Join(dir, "application_domains", "*", "pack.yaml"))
	if err != nil {
		return nil, err
	}
	for _, manifest := range apps {
		if err := loadApplication(c, filepath.Dir(manifest)); err != nil {
			return nil, fmt.Errorf("%s：%w", manifest, err)
		}
	}
	inds, err := filepath.Glob(filepath.Join(dir, "industries", "*", "pack.yaml"))
	if err != nil {
		return nil, err
	}
	for _, manifest := range inds {
		if err := loadIndustry(c, filepath.Dir(manifest)); err != nil {
			return nil, fmt.Errorf("%s：%w", manifest, err)
		}
	}
	if len(c.Objectives) == 0 || len(c.Industries) == 0 {
		return nil, fmt.Errorf("customer-context：%s 中没有应用领域包目标或行业包", dir)
	}
	sort.Slice(c.Objectives, func(i, j int) bool { return c.Objectives[i].ID < c.Objectives[j].ID })
	return c, nil
}

func read(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, out)
}

func loadApplication(c *domain.Catalog, dir string) error {
	var manifest struct {
		Pack struct {
			Provides struct {
				Coverage   string   `yaml:"coverage"`
				Roles      []string `yaml:"roles"`
				Objectives []string `yaml:"objectives"`
			} `yaml:"provides"`
		} `yaml:"pack"`
	}
	if err := read(filepath.Join(dir, "pack.yaml"), &manifest); err != nil {
		return err
	}
	p := manifest.Pack.Provides
	for _, f := range p.Roles {
		var r struct {
			Role struct {
				ID       string   `yaml:"id"`
				Name     string   `yaml:"name"`
				Default  []string `yaml:"default_objectives"`
				Optional []string `yaml:"optional_objectives"`
			} `yaml:"role"`
		}
		if err := read(filepath.Join(dir, f), &r); err != nil {
			return err
		}
		c.Roles[r.Role.ID] = domain.Role{ID: r.Role.ID, Name: r.Role.Name, Default: r.Role.Default, Optional: r.Role.Optional}
	}
	var coverage struct {
		Coverage struct {
			Paths map[string]struct {
				Objective string `yaml:"objective"`
				Release   struct {
					Stage string `yaml:"stage"`
				} `yaml:"release"`
			} `yaml:"paths"`
		} `yaml:"coverage"`
	}
	if p.Coverage != "" {
		if err := read(filepath.Join(dir, p.Coverage), &coverage); err != nil {
			return err
		}
	}
	stage := map[string]string{}
	for _, path := range coverage.Coverage.Paths {
		stage[path.Objective] = path.Release.Stage
	}
	for _, f := range p.Objectives {
		var o struct {
			Objective struct {
				ID              string   `yaml:"id"`
				Version         string   `yaml:"version"`
				Name            string   `yaml:"name"`
				Say             string   `yaml:"say"`
				PolicyTemplates []string `yaml:"policy_templates"`
				RequiresDomains []string `yaml:"requires_domains"`
			} `yaml:"objective"`
		}
		if err := read(filepath.Join(dir, f), &o); err != nil {
			return err
		}
		x := o.Objective
		c.Objectives = append(c.Objectives, domain.ObjectiveDef{ID: x.ID, Version: x.Version, Name: x.Name, Say: x.Say, Stage: stage[x.ID],
			PolicyTemplates: x.PolicyTemplates, RequiresDomains: x.RequiresDomains})
	}
	pack, err := policy.LoadPack(dir)
	if err != nil {
		return err
	}
	for id, t := range pack.Templates {
		tpl := domain.PolicyTemplate{ID: id, Version: t.Version}
		for name, d := range t.Params {
			def := domain.ParamDef{Name: name, Type: d.Type, DefaultFrom: d.DefaultFrom, Min: d.Min, Max: d.Max, HasDefault: d.Default != nil}
			// 整数与字符串列表默认值在目录中展示；其他类型（小数、布尔）只记录有默认值，目标只能取模板默认值。
			switch v := d.Default.(type) {
			case int:
				n := int64(v)
				def.DefaultNumber = &n
			case []any:
				def.DefaultValues = []string{}
				for _, item := range v {
					s, ok := item.(string)
					if !ok {
						return fmt.Errorf("模板 %s 参数 %s 的默认值不是字符串列表", id, name)
					}
					def.DefaultValues = append(def.DefaultValues, s)
				}
			}
			tpl.Params = append(tpl.Params, def)
		}
		sort.Slice(tpl.Params, func(i, j int) bool { return tpl.Params[i].Name < tpl.Params[j].Name })
		c.PolicyTemplates[id] = tpl
	}
	return nil
}

func loadIndustry(c *domain.Catalog, dir string) error {
	var manifest struct {
		Pack struct {
			ID       string `yaml:"id"`
			Provides struct {
				Taxonomy       string `yaml:"taxonomy"`
				Qualifications string `yaml:"qualifications"`
			} `yaml:"provides"`
		} `yaml:"pack"`
	}
	if err := read(filepath.Join(dir, "pack.yaml"), &manifest); err != nil {
		return err
	}
	ind := domain.Industry{ID: manifest.Pack.ID, Categories: map[string]bool{}, Qualifications: map[string]domain.QualificationTemplate{}}
	if f := manifest.Pack.Provides.Taxonomy; f != "" {
		var t struct {
			Taxonomy struct {
				Categories []struct {
					Code string `yaml:"code"`
				} `yaml:"categories"`
			} `yaml:"taxonomy"`
		}
		if err := read(filepath.Join(dir, f), &t); err != nil {
			return err
		}
		for _, cat := range t.Taxonomy.Categories {
			ind.Categories[cat.Code] = true
		}
	}
	if f := manifest.Pack.Provides.Qualifications; f != "" {
		var q struct {
			Qualifications struct {
				Templates []struct {
					Code        string   `yaml:"code"`
					Grades      []string `yaml:"grades"`
					HasValidity bool     `yaml:"has_validity"`
				} `yaml:"templates"`
			} `yaml:"qualifications"`
		}
		if err := read(filepath.Join(dir, f), &q); err != nil {
			return err
		}
		for _, t := range q.Qualifications.Templates {
			ind.Qualifications[t.Code] = domain.QualificationTemplate{Code: t.Code, Grades: t.Grades, HasValidity: t.HasValidity}
		}
	}
	c.Industries[ind.ID] = ind
	return nil
}

// PolicyPack 是全部应用领域包的完整策略模板（按模板标识）与共享特征，供草案合成、编译与样例校验（PRD 第7段 F2）。
type PolicyPack struct {
	Templates map[string]*policy.Policy
	Features  map[string]policy.Feature
}

// LoadPolicies 读取 dir 下全部应用领域包的 policy/templates 与 policy/features.yaml。不同包的模板标识或特征名重复时报错。
func LoadPolicies(dir string) (*PolicyPack, error) {
	out := &PolicyPack{Templates: map[string]*policy.Policy{}, Features: map[string]policy.Feature{}}
	apps, err := filepath.Glob(filepath.Join(dir, "application_domains", "*", "pack.yaml"))
	if err != nil {
		return nil, err
	}
	for _, manifest := range apps {
		pack, err := policy.LoadPack(filepath.Dir(manifest))
		if err != nil {
			return nil, fmt.Errorf("%s：%w", manifest, err)
		}
		for id, t := range pack.Templates {
			if _, dup := out.Templates[id]; dup {
				return nil, fmt.Errorf("%s：策略模板 %s 重复", manifest, id)
			}
			out.Templates[id] = t
		}
		for name, f := range pack.Features {
			if _, dup := out.Features[name]; dup {
				return nil, fmt.Errorf("%s：特征 %s 重复", manifest, name)
			}
			out.Features[name] = f
		}
	}
	return out, nil
}
