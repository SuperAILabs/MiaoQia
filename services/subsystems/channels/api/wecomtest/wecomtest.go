// Package wecomtest 是企业微信渠道适配器（channels api.Provider）的内存实现，只用于测试：不做任何外部调用。
// 回调签名为 HMAC-SHA256（每个企业一把测试密钥）；网页授权码与安装授权码由测试登记，一次性使用。
package wecomtest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
)

// ErrInjected 是测试注入的渠道故障（渠道不可用）。
var ErrInjected = errors.New("wecomtest：注入的故障")

// Sent 是一条已发送的消息。
type Sent struct {
	CorpID, UserID string
	Message        api.Message
}

// WeCom 是内存中的企业微信。
type WeCom struct {
	mu           sync.Mutex
	secrets      map[string][]byte
	installCodes map[string]api.Corp
	oauthCodes   map[string]api.ChannelIdentity
	sent         []Sent
	failOAuth    bool
	failSend     bool
	seq          int
}

func New() *WeCom {
	return &WeCom{secrets: map[string][]byte{}, installCodes: map[string]api.Corp{}, oauthCodes: map[string]api.ChannelIdentity{}}
}

// Corp 登记一个企业的回调密钥，并返回一个安装授权码（管理员在官方授权页完成授权后回调带回）。
func (w *WeCom) Corp(corpID, name string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.secrets[corpID] = []byte("secret-" + corpID)
	w.seq++
	code := fmt.Sprintf("install-%s-%d", corpID, w.seq)
	w.installCodes[code] = api.Corp{CorpID: corpID, Name: name}
	return code
}

// OAuthCode 登记一次网页授权：返回只能使用一次的授权码，取得的当前渠道账号为 who。
func (w *WeCom) OAuthCode(who api.ChannelIdentity) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	code := fmt.Sprintf("oauth-%s-%d", who.UserID, w.seq)
	w.oauthCodes[code] = who
	return code
}

// SetOAuthFailure 置位时网页授权不可用。
func (w *WeCom) SetOAuthFailure(fail bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failOAuth = fail
}

// SetSendFailure 置位时发送失败。
func (w *WeCom) SetSendFailure(fail bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failSend = fail
}

func (w *WeCom) mac(cb *api.Callback) string {
	h := hmac.New(sha256.New, w.secrets[cb.CorpID])
	for _, part := range []string{cb.CorpID, cb.EventID, strconv.FormatInt(cb.Timestamp.Unix(), 10), cb.Nonce, cb.Body} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Sign 为回调签名（模拟企业微信服务器）。
func (w *WeCom) Sign(cb *api.Callback) *api.Callback {
	w.mu.Lock()
	defer w.mu.Unlock()
	cb.Signature = w.mac(cb)
	return cb
}

// Messages 返回发给某个渠道账号的消息（按发送顺序）。
func (w *WeCom) Messages(corpID, userID string) []api.Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []api.Message
	for _, s := range w.sent {
		if s.CorpID == corpID && s.UserID == userID {
			out = append(out, s.Message)
		}
	}
	return out
}

func (w *WeCom) AuthorizeURL(_ context.Context, state string) (string, error) {
	return "https://open.work.weixin.qq.com/wwopen/sso/3rd_qrConnect?state=" + state, nil
}

func (w *WeCom) ExchangeInstallCode(_ context.Context, code string) (*api.Corp, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c, ok := w.installCodes[code]
	if !ok {
		return nil, api.ErrInvalidCode
	}
	delete(w.installCodes, code)
	return &c, nil
}

func (w *WeCom) VerifyCallback(_ context.Context, cb *api.Callback) (*api.InboundEvent, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.secrets[cb.CorpID]; !ok || !hmac.Equal([]byte(w.mac(cb)), []byte(cb.Signature)) {
		return nil, api.ErrBadSignature
	}
	// 测试回调体为 “<kind>:<userid>”
	kind, user := "message", cb.Body
	for i := range cb.Body {
		if cb.Body[i] == ':' {
			kind, user = cb.Body[:i], cb.Body[i+1:]
			break
		}
	}
	return &api.InboundEvent{CorpID: cb.CorpID, UserID: user, EventID: cb.EventID, At: cb.Timestamp, Kind: kind}, nil
}

func (w *WeCom) ResolveOAuthCode(_ context.Context, code string) (*api.ChannelIdentity, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failOAuth {
		return nil, ErrInjected
	}
	who, ok := w.oauthCodes[code]
	if !ok {
		return nil, api.ErrInvalidCode
	}
	delete(w.oauthCodes, code)
	return &who, nil
}

func (w *WeCom) Send(_ context.Context, corpID, userID string, m api.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failSend {
		return ErrInjected
	}
	w.sent = append(w.sent, Sent{CorpID: corpID, UserID: userID, Message: m})
	return nil
}
