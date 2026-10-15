// Package reqctx 是请求上下文：租户、单元、主体、用途。它由 gateway 从已验证的令牌派生，
// 由框架在进程内（context.Context）与进程间（内部调用头、事件信封）传递，不出现在业务参数中。
package reqctx

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
)

// Zone 是数据域。
type Zone string

const (
	ZonePublic   Zone = "public"   // 公共域：不带租户
	ZoneTenant   Zone = "tenant"   // 租户域：必须带租户与单元
	ZonePlatform Zone = "platform" // 平台域：可带租户
)

// Principal 是调用主体。
type Principal struct {
	ID   string `json:"id"`   // 例如 usr_…、svc:operations
	Kind string `json:"kind"` // user / service / service_account
}

// Context 是请求上下文。
type Context struct {
	Tenant    string    `json:"tenant,omitempty"`
	Cell      string    `json:"cell,omitempty"`
	Principal Principal `json:"principal"`
	Purpose   string    `json:"purpose,omitempty"` // 处理目的，例如 interactive、monitoring、research
}

type key struct{}

// With 返回携带请求上下文的 context。
func With(ctx context.Context, c Context) context.Context { return context.WithValue(ctx, key{}, c) }

// From 取出请求上下文。
func From(ctx context.Context) (Context, bool) {
	c, ok := ctx.Value(key{}).(Context)
	return c, ok
}

// Service 返回以服务身份发起的请求上下文（事件消费、工作流、定时任务）。
func Service(ctx context.Context, name string) context.Context {
	c, _ := From(ctx)
	c.Principal = Principal{ID: "svc:" + name, Kind: "service"}
	return With(ctx, c)
}

// ErrMissing 表示需要请求上下文而未提供。
var ErrMissing = errors.New("reqctx: 缺少请求上下文")

var cellPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (c Context) Validate() error {
	if c.Principal.ID == "" || (c.Principal.Kind != "user" && c.Principal.Kind != "service" && c.Principal.Kind != "service_account") {
		return errors.New("reqctx: 主体无效")
	}
	if (c.Tenant == "") != (c.Cell == "") || (c.Cell != "" && !cellPattern.MatchString(c.Cell)) {
		return errors.New("reqctx: 租户与单元无效")
	}
	return nil
}

// Header 是内部调用传递请求上下文的头。只在内部监听端口上接受；外部请求的上下文只由 gateway 从令牌派生。
const Header = "Miaoqia-Context"

// Encode 编码为内部调用头的取值。
func Encode(c Context) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Decode 解码内部调用头。
func Decode(s string) (Context, error) {
	var c Context
	if len(s) > 8192 {
		return c, errors.New("reqctx: 上下文过长")
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, err
	}
	if base64.RawURLEncoding.EncodeToString(b) != s {
		return c, errors.New("reqctx: 非规范Base64URL")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || raw == nil {
		return c, errors.New("reqctx: 上下文必须为对象")
	}
	for key, value := range raw {
		if bytes.Equal(value, []byte("null")) || bytes.Equal(value, []byte(`""`)) {
			return c, errors.New("reqctx: 空上下文字段" + key)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err != nil {
		return c, err
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return c, errors.New("reqctx: 多余JSON内容")
	}
	return c, c.Validate()
}
