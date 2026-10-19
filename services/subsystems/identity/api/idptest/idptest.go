// Package idptest 提供 identity 出站端口（IdP 账号与租户归属、邀请邮件）的内存实现，只用于测试。
// 它不模拟 IdP 登录或 TOTP 仪式：测试直接设置账号的已验证邮箱与 TOTP 登记状态，强认证时间由测试签发方的令牌给出。
package idptest

import (
	"context"
	"errors"
	"sync"

	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// ErrInjected 是测试注入的端口故障。
var ErrInjected = errors.New("idptest：注入的故障")

// Binding 是写入 IdP 的一次租户归属。
type Binding struct{ Subject, Tenant, Cell string }

// IdP 是内存中的 IdP：账号表与租户归属写入记录。
type IdP struct {
	mu          sync.Mutex
	accounts    map[string]api.Account
	bindings    []Binding
	FailLookup  bool // 置位时 Account 返回 ErrInjected（IdP 不可用）
	FailBinding bool // 置位时 BindTenant 返回 ErrInjected
}

func NewIdP() *IdP { return &IdP{accounts: map[string]api.Account{}} }

// Put 登记或替换账号。
func (f *IdP) Put(a api.Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[a.Subject] = a
}

// SetFailures 设置注入的故障。
func (f *IdP) SetFailures(lookup, binding bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.FailLookup, f.FailBinding = lookup, binding
}

func (f *IdP) Account(_ context.Context, subject string) (*api.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailLookup {
		return nil, ErrInjected
	}
	a, ok := f.accounts[subject]
	if !ok {
		return nil, api.ErrAccountNotFound
	}
	return &a, nil
}

func (f *IdP) BindTenant(_ context.Context, subject, tenant, cell string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailBinding {
		return ErrInjected
	}
	f.bindings = append(f.bindings, Binding{subject, tenant, cell})
	return nil
}

// Bindings 返回已写入的租户归属（按写入顺序，含重复写入）。
func (f *IdP) Bindings() []Binding {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Binding(nil), f.bindings...)
}

// Mailbox 是内存中的邮件渠道：记录每封邀请邮件。
type Mailbox struct {
	mu   sync.Mutex
	sent []api.InvitationMessage
	Fail bool // 置位时 SendInvitation 返回 ErrInjected（发送失败）
}

func (m *Mailbox) SetFail(fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Fail = fail
}

func (m *Mailbox) SendInvitation(_ context.Context, msg api.InvitationMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail {
		return ErrInjected
	}
	m.sent = append(m.sent, msg)
	return nil
}

// Sent 返回发给某个邮箱的邀请邮件（按发送顺序）。
func (m *Mailbox) Sent(email string) []api.InvitationMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []api.InvitationMessage
	for _, msg := range m.sent {
		if msg.Email == email {
			out = append(out, msg)
		}
	}
	return out
}
