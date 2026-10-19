// Package domain 是 channels 的业务规则（纯函数，只依赖标准库）：绑定请求的有效期与内容摘要、免登签发凭据的时限、
// 绑定失效何时算完成。规则来源：PRD 第1段 F5–F7、第9节；docs/03-architecture/subsystems/channels.md 第5、6节。
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

const (
	// BindingLinkTTL 是一次性绑定链接的有效期（PRD 第1段 F5 第2步）。
	BindingLinkTTL = 10 * time.Minute
	// CredentialLifetime 是免登签发凭据的有效期；CredentialSkew 是时钟偏差：identity 只接受早于“到期前5秒”到达的凭据，
	// channels 在“到期后5秒”才认为最后一张凭据已过期（PRD 第1段 F6 第1、4步）。
	CredentialLifetime = 30 * time.Second
	CredentialSkew     = 5 * time.Second
	// CardLinkTTL 是卡片详情短时授权链接的有效期（本切片的缺省值，见 channels.md 第6节实现状况）。
	CardLinkTTL = 72 * time.Hour
	// CallbackWindow 是渠道回调时间戳与业务时钟的最大偏差；超出视为重放（PRD 第1段第9节）。
	CallbackWindow = 5 * time.Minute
	// InstallStateTTL 是安装授权 state 的有效期。
	InstallStateTTL = 10 * time.Minute
)

// 绑定状态（chn_bindings.state）。
const (
	Bound       = "bound"
	Revoking    = "revoking"
	Invalidated = "invalidated"
)

// 绑定请求状态（chn_binding_requests.state）：已发起 → 已固定会话 → 已完成；过期、渠道身份不一致、会话替换时作废。
const (
	RequestInitiated = "initiated"
	RequestPinned    = "pinned"
	RequestCompleted = "completed"
	RequestVoided    = "voided"
)

// Confirmation 是绑定请求内容的摘要：请求、渠道账号、成员与固定的 Web 会话。确认时须原样提交，内容改变即不一致。
func Confirmation(requestID, channel, corpID, userID, tenant, subject, session string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{"binding-request/v1", requestID, channel, corpID, userID, tenant, subject, session}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// CredentialExpired 判断最后一张签发凭据在 now 时是否已过期（含偏差）；从未签发过凭据时为真。
func CredentialExpired(lastExpiresAt *time.Time, now time.Time) bool {
	return lastExpiresAt == nil || !now.Before(lastExpiresAt.Add(CredentialSkew))
}

// RevocationComplete 判断失效中的绑定能否标为已失效：identity 已确认撤销到当前版本，且最后一张签发凭据已过期（含偏差）。
// 两条缺一不可；状态不是失效中时为假（已失效或仍有效的绑定不经此路径改变）。
func RevocationComplete(state string, version, confirmedVersion int64, lastExpiresAt *time.Time, now time.Time) bool {
	return state == Revoking && confirmedVersion >= version && CredentialExpired(lastExpiresAt, now)
}

// IssuableUntil 是 channels 仍可把凭据交给 identity 的最晚时刻：到期前5秒之后 identity 必然拒绝，channels 不再调用（不在有效期外重试）。
func IssuableUntil(expiresAt time.Time) time.Time { return expiresAt.Add(-CredentialSkew) }
