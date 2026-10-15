package runtime

import (
	"encoding/json"
	"go.yaml.in/yaml/v3"
	"os"
	"testing"
)

func TestInstanceScope(t *testing.T) {
	for _, tc := range []struct {
		scope, cell string
		ok          bool
	}{
		{"", "cell-01", false}, {"platform", "cell-01", false}, {"tenant", "", false},
		{"tenant", "bad.cell", false}, {"public", "cell-01", false}, {"public", "", true}, {"tenant", "cell-01", true},
	} {
		got, err := InstanceScope("search", map[string]string{"search": tc.scope}, tc.cell, []string{"public", "tenant"})
		if (err == nil) != tc.ok {
			t.Fatalf("%+v: scope=%q err=%v", tc, got, err)
		}
		if tc.ok && got != tc.scope {
			t.Fatalf("scope=%q want=%q", got, tc.scope)
		}
	}
	if got, err := InstanceScope("identity", nil, "", []string{"platform"}); err != nil || got != "platform" {
		t.Fatalf("平台默认域: %q %v", got, err)
	}
}

func TestRoleScopesSharedContract(t *testing.T) {
	data, err := os.ReadFile("../../../tests/contract/instance-scopes.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name     string                       `json:"name"`
		Spec     json.RawMessage              `json:"spec"`
		Registry map[string]ScopeRegistration `json:"registry"`
		Unit     string                       `json:"unit"`
		Valid    bool                         `json:"valid"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			var spec RoleSpec
			if err := json.Unmarshal(tc.Spec, &spec); err != nil {
				if tc.Valid {
					t.Fatal(err)
				}
				return
			}
			scopes, err := RoleScopes(spec, tc.Registry, tc.Unit)
			if (err == nil) != tc.Valid {
				t.Fatalf("scopes=%v err=%v expectedValid=%v", scopes, err, tc.Valid)
			}
		})
	}
}

func TestYAMLRoleDoesNotCoerceScopeTypes(t *testing.T) {
	for _, source := range []string{
		"subsystems: [identity]\ncell: 123\n",
		"subsystems: [identity]\ncell: null\n",
		"subsystems: [identity]\ndata_scopes: null\n",
		"subsystems: [identity]\ndata_scopes: {identity: null}\n",
	} {
		var role RoleSpec
		if err := yaml.Unmarshal([]byte(source), &role); err == nil {
			t.Fatalf("非法类型被接受：%s", source)
		}
	}
}
