package runtime

import (
	"fmt"
	"regexp"
	"slices"
)

var cellPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// InstanceScope 校验角色的实例域；跨域能力必须显式选择，租户实例必须绑定单元。
// 这不替代凭证、存储与消息主题的运行时隔离。
func InstanceScope(subsystem string, declared map[string]string, cell string, allowed []string) (string, error) {
	if len(allowed) == 0 {
		return "", fmt.Errorf("%s: 空 data_scopes 登记", subsystem)
	}
	seen := map[string]bool{}
	for _, value := range allowed {
		if (value != "public" && value != "tenant" && value != "platform") || seen[value] {
			return "", fmt.Errorf("%s: 非法 data_scopes 登记", subsystem)
		}
		seen[value] = true
	}
	scope, explicit := declared[subsystem]
	if !explicit && len(allowed) == 1 {
		scope = allowed[0]
	}
	if !slices.Contains(allowed, scope) {
		return "", fmt.Errorf("%s: 缺少或非法 data_scope %q", subsystem, scope)
	}
	if cell != "" && !cellPattern.MatchString(cell) {
		return "", fmt.Errorf("%s: 非法 cell", subsystem)
	}
	if scope == "public" && cell != "" {
		return "", fmt.Errorf("%s: 公共实例不能绑定租户 cell", subsystem)
	}
	if scope == "tenant" && !cellPattern.MatchString(cell) {
		return "", fmt.Errorf("%s: 租户实例缺少有效 cell", subsystem)
	}
	return scope, nil
}

// RoleSpec 是进程角色的实例声明，启动与迁移使用相同选择。
type RoleSpec struct {
	Subsystems []string          `yaml:"subsystems" json:"subsystems"`
	DataScopes map[string]string `yaml:"data_scopes" json:"data_scopes"`
	Cell       string            `yaml:"cell" json:"cell"`
}

type ScopeRegistration struct {
	Unit       string   `json:"unit"`
	DataScopes []string `json:"data_scopes"`
}

// RoleScopes 校验整个声明，避免多余scope、重复模块或未登记模块被静默忽略。
func RoleScopes(spec RoleSpec, registry map[string]ScopeRegistration, unit string) (map[string]string, error) {
	if len(spec.Subsystems) == 0 {
		return nil, fmt.Errorf("角色subsystems必须非空")
	}
	names := map[string]bool{}
	for _, name := range spec.Subsystems {
		if names[name] {
			return nil, fmt.Errorf("重复子系统 %s", name)
		}
		names[name] = true
	}
	for name := range spec.DataScopes {
		if !names[name] {
			return nil, fmt.Errorf("data_scopes引用了未加载的 %s", name)
		}
	}
	scopes := map[string]string{}
	for name := range names {
		entry, ok := registry[name]
		if !ok || (unit != "" && entry.Unit != unit) {
			return nil, fmt.Errorf("%s 未登记或不属于 %s 单元", name, unit)
		}
		scope, err := InstanceScope(name, spec.DataScopes, spec.Cell, entry.DataScopes)
		if err != nil {
			return nil, err
		}
		scopes[name] = scope
	}
	return scopes, nil
}
