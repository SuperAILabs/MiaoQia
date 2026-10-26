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

// InboundEvent 是验签后的入站事件：企业内成员给妙洽发消息、打开应用或点击卡片按钮。
type InboundEvent struct {
	CorpID  string
	UserID  string
	EventID string
	At      time.Time
	Kind    string // message / enter_app / card_action
	Name    string // 显示名，不可得时为空
	// 卡片按钮（Kind 为 card_action）：Action 是按钮携带的动作令牌（channels 签发的不透明值，原样带回），CardID 是被点击的卡片。
	Action string
	CardID string
}

// Message 是发给渠道账号的消息（渠道无关表示）。Kind：binding_guide（绑定引导，URL 为一次性绑定链接）、binding_succeeded（绑定成功）、
// briefing_overview（简报总览卡）、signal_card（信号卡片）、action_result（卡片动作的结果消息）。卡片类消息带 Card。
type Message struct {
	Kind string
	Text string
	URL  string
	Card *Card
}

// Card 是一张卡片：CardID 由 channels 生成（更新卡片时引用）；Lines 是按优先级渲染并截断后的正文行；Buttons 是按钮（动作令牌或链接）。
type Card struct {
	CardID  string
	Title   string
	Lines   []string
	Buttons []Button
}

// Button 是卡片上的一个按钮：Token 为动作令牌（点击经回调带回）；URL 为链接按钮（“查看”“其他”“在收件箱查看”）。二者只有其一。
type Button struct {
	Key   string
	Label string
	Token string
	URL   string
}

// Capabilities 是渠道适配器的能力声明（channels.md 第4.2节）：卡片各字段的长度与条数上限，以及能否更新已发送的卡片。
type Capabilities struct {
	TitleMax    int // 卡片标题的最大字符数
	LineMax     int // 每行的最大字符数
	LinesMax    int // 正文的最大行数
	UpdateCards bool
}

// Provider 是一个渠道的官方开放能力：
//   - AuthorizeURL 返回渠道官方授权页地址（state 原样带回）；
//   - ExchangeInstallCode 以授权回调的授权码换得企业；授权码无效为 ErrInvalidCode，其他错误为渠道不可用；
//   - VerifyCallback 验证回调签名与来源并解出事件；验签失败为 ErrBadSignature；
//   - ResolveOAuthCode 以网页授权码取得当前渠道账号；授权码无效为 ErrInvalidCode，其他错误为渠道不可用；
//   - Send 向企业内成员发送消息；
//   - UpdateCard 把已发送给该成员的卡片（CardID）更新为新内容；不支持或失败时调用方另发一条消息；
//   - Capabilities 返回能力声明。
type Provider interface {
	AuthorizeURL(ctx context.Context, state string) (string, error)
	ExchangeInstallCode(ctx context.Context, code string) (*Corp, error)
	VerifyCallback(ctx context.Context, cb *Callback) (*InboundEvent, error)
	ResolveOAuthCode(ctx context.Context, code string) (*ChannelIdentity, error)
	Send(ctx context.Context, corpID, userID string, m Message) error
	UpdateCard(ctx context.Context, corpID, userID, cardID string, m Message) error
	Capabilities() Capabilities
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
func (NotWired) UpdateCard(context.Context, string, string, string, Message) error {
	return ErrNotWired
}

// Capabilities 是未接入时的保守声明（按企业微信模板卡片的公开上限取较小值）。
func (NotWired) Capabilities() Capabilities {
	return Capabilities{TitleMax: 26, LineMax: 40, LinesMax: 8}
}

// 以下是简报组装的出站端口（PRD 第5段 F1 第11条、F3 第3条）：
//
//   - Coverage 是 domain-intelligence 的 GetCoverage：各信息领域的数据截至时间与来源中断；
//   - Rights 是来源权利的展示级别：信号各展示字段的来源级别（逐维，可能多项）与原文链接。
//
// 两者的生产适配器尚未接入（NotWired）：覆盖信息不可得时各领域标“未知”（不以“正常”代替，不当作中断）；权利不可得时失败关闭，
// 来源字段一律按 none 展示（只有命中理由、处置、标识与链接）。测试使用标明为夹具的内存实现。

// DomainCoverage 是一个信息领域的覆盖：Interrupted 为来源中断，AsOf 为数据截至时间。
type DomainCoverage struct {
	Domain      string
	Interrupted bool
	AsOf        time.Time
}

// Coverage 返回给定信息领域的覆盖（以带租户的服务主体调用）。
type Coverage interface {
	Coverage(ctx context.Context, domains []string) ([]DomainCoverage, error)
}

// FieldRight 是一个展示字段的来源权利：Levels 是该字段全部来源的展示级别（full_text / excerpt / citation_only / none，
// 取最严格者），SourceURL 是原文链接（citation_only 时只显示它）。
type FieldRight struct {
	Levels    []string
	SourceURL string
}

// Rights 返回信号各展示字段（project、winner、amount、region、categories）的来源权利（以带租户的服务主体调用）。
// 缺少的信号或字段按 none 处理。
type Rights interface {
	FieldRights(ctx context.Context, signals []string) (map[string]map[string]FieldRight, error)
}

func (NotWired) Coverage(context.Context, []string) ([]DomainCoverage, error) {
	return nil, ErrNotWired
}
func (NotWired) FieldRights(context.Context, []string) (map[string]map[string]FieldRight, error) {
	return nil, ErrNotWired
}
