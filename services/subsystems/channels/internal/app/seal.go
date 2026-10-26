package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"time"
)

// 客户端持有的状态（卡片按钮的动作令牌、简报历史的翻页游标）都由 channels 签发：整个载荷连同 HMAC-SHA256 签名一起编码
// （base64url(载荷 ‖ HMAC(用途 ‖ 0 ‖ 载荷))），签名不符即拒绝，在任何读取与副作用之前判断。密钥为 MIAOQIA_CHANNELS_SIGNING_KEY
// （base64，至少 32 字节，同一部署的全部实例相同；只有 development 可以缺省，此时每个进程随机生成）。

// MinSigningKey 是签名密钥的最短长度（字节）。
const MinSigningKey = 32

// 签名用途（域分隔：一种用途的签名不能当作另一种使用）。
const (
	purposeAction = "channels/action-token/v1"
	purposeCursor = "channels/briefing-cursor/v1"
)

func (s *Service) mac(purpose string, payload []byte) []byte {
	h := hmac.New(sha256.New, s.Key)
	h.Write([]byte(purpose))
	h.Write([]byte{0})
	h.Write(payload)
	return h.Sum(nil)
}

// seal 签发一个值；密钥不可用时 503（失败关闭）。
func (s *Service) seal(purpose string, v any) (string, error) {
	if len(s.Key) < MinSigningKey {
		return "", failure(503, "signing_key_unavailable")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(append(raw, s.mac(purpose, raw)...)), nil
}

// unseal 先校验签名再解析；签名不符、格式错误时返回 false。
func (s *Service) unseal(purpose, value string, v any) bool {
	if len(s.Key) < MinSigningKey || len(value) > 2048 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) <= sha256.Size {
		return false
	}
	payload, sum := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	if !hmac.Equal(sum, s.mac(purpose, payload)) {
		return false
	}
	return json.Unmarshal(payload, v) == nil
}

// actionClaims 是动作令牌签名的载荷：令牌（= 操作）标识、租户、收件人、信号、动作、所见内容版本、到期时间与所属卡片。
// 服务端另存同样的字段，执行时两者须一致。
type actionClaims struct {
	ID        string    `json:"i"`
	Tenant    string    `json:"t"`
	Recipient string    `json:"r"`
	Signal    string    `json:"s"`
	Action    string    `json:"a"`
	Reason    string    `json:"c,omitempty"`
	Content   int64     `json:"v"`
	Card      string    `json:"k"`
	Expires   time.Time `json:"e"`
}

// historyCursor 是简报历史的翻页位置：上一页最后一期的 (period_end, id)，绑定租户与调用者。
type historyCursor struct {
	Tenant  string    `json:"t"`
	Subject string    `json:"u"`
	End     time.Time `json:"e"`
	ID      string    `json:"i"`
}
