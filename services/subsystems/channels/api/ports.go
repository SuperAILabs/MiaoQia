package api

import (
	"context"
	"errors"
	"time"
)

// 以下是 channels 的出站端口：企业微信的官方开放能力（C10 渠道方案）。真实调用与凭证尚未获准接入，生产组装使用“未接入”实现
// （NotWired）：所有调用返回 ErrNotWired，依赖它们的流程失败关闭（安装 503 channel_unavailable，回调一律拒绝，免登转登录页）。
// 测试使用内存实现（api/wecomtest）。不做任何外部调用。

var (
	// ErrNotWired 表示渠道适配器没有接入。
	ErrNotWired = errors.New("channels：渠道适配器未接入")
	// ErrBadSignature 表示回调验签失败（签名不符或来源不可信）。
	ErrBadSignature = errors.New("channels：回调验签失败")
	// ErrInvalidCode 表示授权码无效、已使用或已过期（明确的拒绝，区别于渠道不可用）。
	ErrInvalidCode = errors.New("channels：授权码无效")
)

// Corp 是渠道授权回调换得的企业（企业微信 corpid 与名称）。渠道凭证由适配器存放于密钥管理系统，不经过本接口。
type Corp struct {
	CorpID string
	Name   string
}

// ChannelIdentity 是渠道网页授权取得的当前渠道账号：所在企业与企业内成员标识（企业微信 corpid + userid）。
type ChannelIdentity struct {
	CorpID      string
	UserID      string
	DisplayName string // 不可得时为空
}

// Callback 是一次原始消息回调（企业微信的加密消息体与签名参数经适配器归一）。
type Callback struct {
	CorpID    string
	EventID   string // 渠道事件标识（去重键）
	Timestamp time.Time
	Nonce     string
	Body      string
	Signature string
}

// InboundEvent 是验签后的入站事件：企业内成员给妙洽发消息或打开应用。
type InboundEvent struct {
	CorpID  string
	UserID  string
	EventID string
	At      time.Time
	Kind    string // message / enter_app
	Name    string // 显示名，不可得时为空
}

// Message 是发给渠道账号的消息（渠道无关表示的最小子集；完整渲染随 PRD 第5段）。
type Message struct {
	Kind string // binding_guide（绑定引导，URL 为一次性绑定链接）/ binding_succeeded（绑定成功）
	Text string
	URL  string
}

// Provider 是一个渠道的官方开放能力：
//   - AuthorizeURL 返回渠道官方授权页地址（state 原样带回）；
//   - ExchangeInstallCode 以授权回调的授权码换得企业；授权码无效为 ErrInvalidCode，其他错误为渠道不可用；
//   - VerifyCallback 验证回调签名与来源并解出事件；验签失败为 ErrBadSignature；
//   - ResolveOAuthCode 以网页授权码取得当前渠道账号；授权码无效为 ErrInvalidCode，其他错误为渠道不可用；
//   - Send 向企业内成员发送消息。
type Provider interface {
	AuthorizeURL(ctx context.Context, state string) (string, error)
	ExchangeInstallCode(ctx context.Context, code string) (*Corp, error)
	VerifyCallback(ctx context.Context, cb *Callback) (*InboundEvent, error)
	ResolveOAuthCode(ctx context.Context, code string) (*ChannelIdentity, error)
	Send(ctx context.Context, corpID, userID string, m Message) error
}

// NotWired 是缺省的渠道适配器：所有调用返回 ErrNotWired。
type NotWired struct{}

func (NotWired) AuthorizeURL(context.Context, string) (string, error)       { return "", ErrNotWired }
func (NotWired) ExchangeInstallCode(context.Context, string) (*Corp, error) { return nil, ErrNotWired }
func (NotWired) VerifyCallback(context.Context, *Callback) (*InboundEvent, error) {
	return nil, ErrNotWired
}
func (NotWired) ResolveOAuthCode(context.Context, string) (*ChannelIdentity, error) {
	return nil, ErrNotWired
}
func (NotWired) Send(context.Context, string, string, Message) error { return ErrNotWired }
