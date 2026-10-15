// Package archtest 检查 Go 代码的依赖规则（docs/03-architecture/09-codebase-structure.md 第 3 节）：
//
//	R1 子系统 A 只能导入子系统 B 的 api 包（含其子包），不得导入 B 的其他包（internal 由编译器另行保证）
//	R2 依赖方向：交互 → 客户智能 → 数据供应；数据供应平面不得导入客户智能与交互平面，客户智能平面不得导入交互平面
//	R3 kernel 不依赖任何子系统
//	R5 domain 层只依赖标准库与本子系统的 domain 包
//
// 平面与子系统的对应取自 deploy/subsystems.yaml（由子系统索引生成）。
package archtest

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const module = "github.com/superailabs/miaoqia/services"

type pkg struct {
	ImportPath string
	Imports    []string
	Standard   bool
}

func listPackages(t *testing.T) []pkg {
	t.Helper()
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = ".."
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(out)
	var pkgs []pkg
	for {
		var p pkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	return pkgs
}

// planes 返回 Go 包目录名 → 平面。
func planes(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "subsystems.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Subsystems map[string]struct {
			Plane    string `yaml:"plane"`
			Language string `yaml:"language"`
		} `yaml:"subsystems"`
	}
	if err := yaml.Unmarshal(b, &reg); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for name, s := range reg.Subsystems {
		if s.Language == "go" {
			out[strings.ReplaceAll(name, "-", "")] = s.Plane
		}
	}
	return out
}

// subsystemOf 返回包所属的子系统目录名与其后的路径（如 operations、internal/app）。
func subsystemOf(path string) (string, string, bool) {
	rest, ok := strings.CutPrefix(path, module+"/subsystems/")
	if !ok {
		return "", "", false
	}
	name, sub, _ := strings.Cut(rest, "/")
	return name, sub, true
}

// 不允许的依赖方向：键为导入方平面，值为不可导入的平面。
var forbiddenPlanes = map[string][]string{
	"data_supply":           {"customer_intelligence", "interaction"},
	"customer_intelligence": {"interaction"},
	"platform":              {"data_supply", "customer_intelligence", "interaction"},
}

func forbiddenDependency(from, fromPlane, toPlane string) bool {
	if from == "operations" && fromPlane == "platform" {
		return false // 内部交互客户端，例外已在总体架构登记。
	}
	for _, bad := range forbiddenPlanes[fromPlane] {
		if toPlane == bad {
			return true
		}
	}
	return false
}

func TestDependencyRules(t *testing.T) {
	pl := planes(t)
	pkgs := listPackages(t)
	std := map[string]bool{}
	for _, p := range pkgs {
		if p.Standard {
			std[p.ImportPath] = true
		}
	}
	isStd := func(imp string) bool { return !strings.Contains(strings.Split(imp, "/")[0], ".") }
	checked := 0
	for _, p := range pkgs {
		from, fromSub, inSub := subsystemOf(p.ImportPath)
		if inSub {
			if _, ok := pl[from]; !ok {
				t.Errorf("%s：子系统目录 %s 不在子系统索引中", p.ImportPath, from)
			}
		}
		for _, imp := range p.Imports {
			to, toSub, toIsSub := subsystemOf(imp)
			switch {
			case strings.HasPrefix(p.ImportPath, module+"/kernel") && toIsSub:
				t.Errorf("R3 %s 不得依赖子系统 %s", p.ImportPath, imp)
			case inSub && toIsSub && to != from:
				if toSub != "api" && !strings.HasPrefix(toSub, "api/") {
					t.Errorf("R1 %s 只能导入 %s 的 api 包，实际导入 %s", p.ImportPath, to, imp)
				}
				if forbiddenDependency(from, pl[from], pl[to]) {
					t.Errorf("R2 %s（%s）不得依赖 %s（%s）", from, pl[from], to, pl[to])
				}
				if from == "learning" && to == "detection" {
					t.Errorf("仿真契约：learning 直接复用 kernel/policy，不依赖 %s", imp)
				}
			}
			if inSub && (fromSub == "internal/domain" || strings.HasPrefix(fromSub, "internal/domain/")) {
				ok := isStd(imp) || (toIsSub && to == from && strings.HasPrefix(toSub, "internal/domain"))
				if !ok {
					t.Errorf("R5 %s 的 domain 层不得依赖 %s", from, imp)
				}
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("没有检查到任何依赖")
	}
}

func TestPlatformDependencyRule(t *testing.T) {
	for _, plane := range []string{"data_supply", "customer_intelligence", "interaction"} {
		if !forbiddenDependency("identity", "platform", plane) {
			t.Fatalf("平台依赖 %s 未被拒绝", plane)
		}
		if forbiddenDependency("operations", "platform", plane) {
			t.Fatalf("operations 的客户端例外被拒绝：%s", plane)
		}
	}
	if forbiddenDependency("identity", "platform", "platform") {
		t.Fatal("平台内部依赖应允许")
	}
}
