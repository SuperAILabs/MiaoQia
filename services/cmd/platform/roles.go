package main

import (
	"fmt"
	"path/filepath"

	"github.com/superailabs/miaoqia/services/kernel/runtime"
)

// loadRole 在任何数据库连接、业务启动或迁移前校验完整角色。
func loadRole(role string) (runtime.RoleSpec, map[string]string, registry, error) {
	var mf manifest
	var inventory registry
	if err := readYAML(filepath.Join(deployDir(), "platform", "roles.yaml"), &mf); err != nil {
		return runtime.RoleSpec{}, nil, inventory, err
	}
	spec, ok := mf.Roles[role]
	if !ok {
		return spec, nil, inventory, fmt.Errorf("未知角色 %q", role)
	}
	if err := readYAML(filepath.Join(deployDir(), "subsystems.yaml"), &inventory); err != nil {
		return spec, nil, inventory, err
	}
	registrations := map[string]runtime.ScopeRegistration{}
	for name, entry := range inventory.Subsystems {
		registrations[name] = runtime.ScopeRegistration{Unit: entry.Unit, DataScopes: entry.DataScopes}
	}
	scopes, err := runtime.RoleScopes(spec, registrations, "platform")
	if err != nil {
		return spec, nil, inventory, err
	}
	available := map[string]bool{}
	for _, mk := range modules {
		available[mk().Name] = true
	}
	for name := range scopes {
		if !available[name] {
			return spec, nil, inventory, fmt.Errorf("未组装子系统 %s", name)
		}
	}
	return spec, scopes, inventory, nil
}
