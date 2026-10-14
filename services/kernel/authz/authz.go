// Package authz 是统一授权 SDK：子系统经它调用 identity 的授权决策点（Check），不自行判断权限。
package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// Request 是一次授权决策请求（ReBAC：主体 — 关系 — 资源）。
type Request struct {
	Subject  string // 缺省为请求上下文中的调用主体
	Relation string // 例如 can_trigger_probe
	Object   string // 例如 platform:miaoqia
}

// Decider 是授权决策点，由 identity 实现（本地或远程绑定）。
type Decider interface {
	Check(ctx context.Context, req Request) (bool, error)
}

// ErrForbidden 表示授权拒绝。
var ErrForbidden = errors.New("authz: 无权执行该操作")

// Subject 把请求上下文中的调用主体转换为授权模型中的主体，例如 user:usr_…、service:operations。
func Subject(p reqctx.Principal) string {
	switch p.Kind {
	case "service":
		return "service:" + trimSvc(p.ID)
	case "service_account":
		return "service_account:" + p.ID
	}
	return "user:" + p.ID
}

func trimSvc(id string) string {
	if len(id) > 4 && id[:4] == "svc:" {
		return id[4:]
	}
	return id
}

// Require 检查当前调用主体对资源具有关系，否则返回 ErrForbidden。
func Require(ctx context.Context, d Decider, relation, object string) error {
	rc, ok := reqctx.From(ctx)
	if !ok || rc.Principal.ID == "" {
		return reqctx.ErrMissing
	}
	allowed, err := d.Check(ctx, Request{Subject: Subject(rc.Principal), Relation: relation, Object: object})
	if err != nil {
		return fmt.Errorf("授权决策失败：%w", err)
	}
	if !allowed {
		return fmt.Errorf("%w：%s %s %s", ErrForbidden, Subject(rc.Principal), relation, object)
	}
	return nil
}
