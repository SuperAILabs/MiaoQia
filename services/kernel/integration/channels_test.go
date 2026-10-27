package integration_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	chn "github.com/superailabs/miaoqia/services/contracts/apis/channels"
	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/contracts/events"
	"github.com/superailabs/miaoqia/services/internal/testissuer"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/channels"
	chapi "github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api/wecomtest"
	ccapi "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	detectionapi "github.com/superailabs/miaoqia/services/subsystems/detection/api"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	idapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api/idptest"
	idremote "github.com/superailabs/miaoqia/services/subsystems/identity/api/remote"
	pursuitapi "github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// hookedChannelSessions 是 channels 看到的 identity 免登会话接口：可注入签发前/后的屏障与故障（模拟超时、不可用），
// 其余调用照常转发给 identity。
type hookedChannelSessions struct {
	idapi.ChannelSessions
	failIssue, failRevoke atomic.Bool
	failLifecycle         atomic.Bool
	mu                    sync.Mutex
	before                func(req *idapi.IssueChannelSessionRequest) error
	after                 func(s *idapi.ChannelSession)
}

func (h *hookedChannelSessions) hooks(before func(*idapi.IssueChannelSessionRequest) error, after func(*idapi.ChannelSession)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.before, h.after = before, after
}

func (h *hookedChannelSessions) IssueChannelSession(ctx context.Context, req *idapi.IssueChannelSessionRequest) (*idapi.ChannelSession, error) {
	h.mu.Lock()
	before, after := h.before, h.after
	h.mu.Unlock()
	if h.failIssue.Load() {
		return nil, errors.New("identity 不可用（测试注入）")
	}
	if before != nil {
		if err := before(req); err != nil {
			return nil, err
		}
	}
	s, err := h.ChannelSessions.IssueChannelSession(ctx, req)
	if err == nil && after != nil {
		after(s)
	}
	return s, err
}

func (h *hookedChannelSessions) MemberLifecycle(ctx context.Context, subject string) (*idapi.ChannelMemberLifecycle, error) {
	if h.failLifecycle.Load() {
		return nil, &idapi.Error{Status: 503, Code: "identity_unavailable"}
	}
	return h.ChannelSessions.MemberLifecycle(ctx, subject)
}

func (h *hookedChannelSessions) RevokeChannelSessions(ctx context.Context, req *idapi.RevokeChannelSessionsRequest) (*idapi.ChannelSessionRevocation, error) {
	if h.failRevoke.Load() {
		return nil, &idapi.Error{Status: 503, Code: "identity_unavailable"}
	}
	return h.ChannelSessions.RevokeChannelSessions(ctx, req)
}

// 企业微信渠道的安装、绑定、解绑与免登撤销、卡片跳转、回调（PRD 第1段第12节用例 8–16 的服务端部分）。真实 PostgreSQL + OpenFGA +
// 网关令牌校验 + mTLS 内部端口；企业微信使用内存实现（wecomtest，不做外部调用），IdP 使用内存端口（idptest）。免登会话令牌由测试签发方
// 按 identity 返回的会话标识签发（IdP 侧铸造尚未接入）。业务时钟由测试控制，用于凭据过期、12小时会话与卡片链接过期。
func TestChannelBindingAndSSO(t *testing.T) {
	fga := os.Getenv("MIAOQIA_TEST_OPENFGA")
	if fga == "" || os.Getenv("MIAOQIA_TEST_PG") == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI 中必须设置 MIAOQIA_TEST_PG 与 MIAOQIA_TEST_OPENFGA")
		}
		t.Skip("MIAOQIA_TEST_PG 或 MIAOQIA_TEST_OPENFGA 未设置")
	}
	t.Setenv("MIAOQIA_ENV", "production")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	idp, clock, wc := idptest.NewIdP(), &testClock{}, wecomtest.New()
	iam := testenv.NewDatabase(t)
	idModule := identity.ModuleWith(identity.Adapters{IdP: idp, Notifier: &idptest.Mailbox{}, Now: clock.Now})
	if err := db.Migrate(ctx, iam.OwnerDSN, iam.Name+"_app", idModule.Migrations); err != nil {
		t.Fatal(err)
	}
	t1, t2 := ids.New("tnt"), ids.New("tnt")
	suffix := strings.ToLower(iam.Name)
	user := func(name string) string { return name + "-" + suffix }
	admin, bd, dm, ex, lv, ra, rj, rk, admin2 := user("admin"), user("bd"), user("dm"), user("ex"), user("lv"), user("ra"), user("rj"), user("rk"), user("admin2")
	iamOwner, err := pgx.Connect(ctx, iam.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer iamOwner.Close(ctx)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO identity_tenants VALUES ($1,'甲','cell-01','active',1), ($2,'乙','cell-01','active',1)`, []any{t1, t2}},
		{`INSERT INTO identity_members VALUES ($1,$2,'active',ARRAY['admin'],1), ($1,$3,'active',ARRAY['business_developer'],1),
 ($1,$4,'active',ARRAY['decision_maker'],1), ($1,$5,'active',ARRAY['domain_expert'],1), ($1,$6,'active',ARRAY['business_developer'],1),
 ($1,$7,'active',ARRAY['research_analyst'],1), ($8,$9,'active',ARRAY['admin'],1), ($1,$10,'active',ARRAY['business_developer'],1), ($1,$11,'active',ARRAY['business_developer'],1)`, []any{t1, admin, bd, dm, ex, lv, ra, t2, admin2, rj, rk}},
	} {
		if _, err := iamOwner.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []string{admin, bd, dm, ex, lv, ra, rj, rk, admin2} {
		idp.Put(idapi.Account{Subject: s, Email: s + "@example.com", EmailVerified: true, TOTPEnrolled: true})
	}
	iamApp, err := db.Open(ctx, iam.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer iamApp.Close()
	bindings := runtime.NewBindings()
	idEnv := &runtime.Env{DB: iamApp, Bindings: bindings, Log: quiet, Cell: "cell-01", Config: func(string) string { return fga }}
	if err := idModule.Start(ctx, idEnv); err != nil {
		t.Fatal(err)
	}
	identitySessions, _ := runtime.Lookup[idapi.ChannelSessions](bindings, idapi.ChannelSessionsBinding)
	hooked := &hookedChannelSessions{ChannelSessions: identitySessions}
	bindings.Provide(idapi.ChannelSessionsBinding, idapi.ChannelSessions(hooked))

	chnDB := testenv.NewDatabase(t)
	// 第6段：channels 依赖 pursuit 的渠道内动作；本用例不开启卡片“跟进”入口，以未接入的实现占位
	bindings.Provide(pursuitapi.ActionsBinding, pursuitapi.Actions(unwiredPursuits{}))
	bindings.Provide(pursuitapi.TrackingsBinding, pursuitapi.Trackings(unwiredPursuits{}))
	module := channels.ModuleWith(channels.Adapters{WeCom: wc, Now: clock.Now})
	if err := db.Migrate(ctx, chnDB.OwnerDSN, chnDB.Name+"_app", module.Migrations); err != nil {
		t.Fatal(err)
	}
	chnOwner, err := pgx.Connect(ctx, chnDB.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer chnOwner.Close(ctx)
	chnApp, err := db.Open(ctx, chnDB.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer chnApp.Close()
	// 第5段的简报依赖在本测试中不使用：以失败关闭的替身登记（调用一律不可用）
	bindings.Provide(detectionapi.BriefingBinding, detectionapi.Briefing(unavailableBriefing{}))
	bindings.Provide(ccapi.AccessBinding, ccapi.ObjectiveAccess(unavailableContext{}))
	bindings.Provide(ccapi.ActivePoliciesBinding, ccapi.ActivePolicies(unavailableContext{}))
	env := &runtime.Env{DB: chnApp, Bindings: bindings, Log: quiet, Cell: "cell-01", Config: func(k string) string {
		if k == "SIGNING_KEY" {
			return base64.StdEncoding.EncodeToString([]byte("channels-test-signing-key-0123456789"))
		}
		return ""
	}}
	if err := module.Start(ctx, env); err != nil {
		t.Fatal(err)
	}
	callbacks, _ := runtime.Lookup[chapi.Callbacks](bindings, chapi.CallbacksBinding)
	messaging, _ := runtime.Lookup[chapi.Messaging](bindings, chapi.MessagingBinding)
	maintenance, _ := runtime.Lookup[chapi.Maintenance](bindings, chapi.MaintenanceBinding)
	directory, _ := runtime.Lookup[idapi.Directory](bindings, idapi.DirectoryBinding)
	if callbacks == nil || messaging == nil || maintenance == nil || directory == nil {
		t.Fatal("channels 或 identity 未提供本地绑定")
	}

	public, internal := http.NewServeMux(), http.NewServeMux()
	idModule.Routes(idEnv, public, internal)
	module.Routes(env, public, internal)
	issuer := testissuer.New(t)
	resolver, _ := runtime.Lookup[idapi.SessionResolver](bindings, idapi.SessionBinding)
	secured, err := gateway.Public(ctx, issuer.Server.URL, "miaoqia-api", public, resolver)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(secured)
	defer server.Close()
	certs := testenv.NewTLS(t)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CERT", certs.CertPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_KEY", certs.KeyPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CA", certs.CA)
	t.Setenv("MIAOQIA_INTERNAL_ALLOWED_PEERS", "spiffe://miaoqia/service/platform-api")
	intern := httptest.NewUnstartedServer(internal)
	intern.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: certs.Pool, Certificates: []tls.Certificate{certs.Cert}, MinVersion: tls.VersionTLS13}
	intern.StartTLS()
	defer intern.Close()
	remoteClient, err := idremote.NewAuthorizer(intern.URL + "/internal")
	if err != nil {
		t.Fatal(err)
	}
	remoteSessions, ok := remoteClient.(idapi.ChannelSessions)
	if !ok {
		t.Fatal("identity 远程客户端未实现 ChannelSessions")
	}

	tt := t
	run := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			prev := tt
			tt = t
			defer func() { tt = prev }()
			f(t)
		})
	}
	// 令牌：web 为带 Web 会话标识（sid）、刚完成第二因素的交互登录；channelTok 为免登会话令牌
	tok := func(subject, tenant string, claims map[string]any) string {
		cell := ""
		if tenant != "" {
			cell = "cell-01"
		}
		return issuer.TokenWith(tt, subject, tenant, cell, claims)
	}
	web := func(subject, tenant, sid string) string {
		return tok(subject, tenant, map[string]any{"amr": []string{"pwd", "mfa"}, "auth_time": clock.Now().Unix(), "sid": sid})
	}
	stale := func(subject, tenant string) string {
		return tok(subject, tenant, map[string]any{"amr": []string{"pwd", "mfa"}, "auth_time": clock.Now().Add(-16 * time.Minute).Unix(), "sid": "stale"})
	}
	channelTok := func(subject, tenant, session string) string {
		return tok(subject, tenant, map[string]any{"miaoqia_session_source": "channel", "miaoqia_channel_session": session, "auth_time": clock.Now().Unix()})
	}
	var keys atomic.Int64
	do := func(token, method, path, body string) (int, []byte) {
		tt.Helper()
		r, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(method, path, body, keys.Add(1)))))[:40])
		resp, err := server.Client().Do(r)
		if err != nil {
			tt.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	codeOf := func(raw []byte) string {
		var p struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &p)
		return p.Code
	}
	expect := func(token, method, path, body string, status int, code string) []byte {
		tt.Helper()
		got, raw := do(token, method, path, body)
		if got != status || (code != "" && codeOf(raw) != code) {
			tt.Fatalf("%s %s：得到 %d %s，期望 %d %s", method, path, got, raw, status, code)
		}
		return raw
	}
	decode := func(raw []byte, v any) {
		tt.Helper()
		if err := json.Unmarshal(raw, v); err != nil {
			tt.Fatalf("%v：%s", err, raw)
		}
	}
	count := func(conn *pgx.Conn, q string, args ...any) int {
		tt.Helper()
		var n int
		if err := conn.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			tt.Fatal(err)
		}
		return n
	}
	errCode := func(err error) string {
		var a *idapi.Error
		var c *chapi.Error
		switch {
		case err == nil:
			return ""
		case errors.As(err, &a):
			return a.Code
		case errors.As(err, &c):
			return c.Code
		}
		return err.Error()
	}
	gw := reqctx.With(ctx, reqctx.Context{Principal: reqctx.Principal{ID: "svc:gateway", Kind: "service"}})
	svc := func(tenant, principal string) context.Context {
		return reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: "cell-01", Principal: reqctx.Principal{ID: principal, Kind: "service"}})
	}
	ident := func(corp, userID string) chapi.ChannelIdentity {
		return chapi.ChannelIdentity{CorpID: corp, UserID: userID, DisplayName: "成员" + userID}
	}
	oauth := func(corp, userID string) string { return wc.OAuthCode(ident(corp, userID)) }

	// 安装：管理员开始安装 → 渠道官方授权页回调 → 待验证
	installations := func(token string) []chn.Installation {
		var list chn.InstallationList
		decode(expect(token, "GET", "/v1/channels/installations", "", 200, ""), &list)
		return list.Installations
	}
	install := func(tenant, adminSubject, corp string) chn.Installation {
		tt.Helper()
		var started chn.InstallationStarted
		decode(expect(web(adminSubject, tenant, "install"), "POST", "/v1/channels/installations", `{"channel":"wecom"}`, 200, ""), &started)
		u, ok := started.AuthorizeURL.Get()
		if !ok || started.Installation.State != "authorizing" {
			tt.Fatalf("%+v", started)
		}
		state := u.Query().Get("state")
		inst, err := callbacks.InstallCallback(gw, "wecom", state, wc.Corp(corp, "企业"+corp))
		if err != nil || inst.State != "pending_verification" || inst.CorpID != corp || inst.Tenant != tenant {
			tt.Fatalf("%+v %v", inst, err)
		}
		// 同一 state 不能再次使用
		if _, err := callbacks.InstallCallback(gw, "wecom", state, wc.Corp(corp, "企业"+corp)); err == nil {
			tt.Fatal("安装 state 必须一次性")
		}
		return installations(web(adminSubject, tenant, "install"))[0]
	}
	// 消息回调：企业微信服务器签名后的回调
	var eventSeq atomic.Int64
	inbound := func(corp, userID string, at time.Time) (*chapi.InboundResult, error) {
		return callbacks.ReceiveInbound(gw, "wecom", wc.Sign(&chapi.Callback{CorpID: corp, EventID: fmt.Sprintf("ev-%d", eventSeq.Add(1)), Timestamp: at,
			Nonce: "n", Body: "message:" + userID}))
	}
	// guide 让渠道账号发一条消息并取回绑定引导中的一次性链接
	guide := func(corp, userID string) string {
		tt.Helper()
		res, err := inbound(corp, userID, clock.Now())
		if err != nil || res.Outcome != chapi.InboundGuideSent {
			tt.Fatalf("%+v %v", res, err)
		}
		msgs := wc.Messages(corp, userID)
		last := msgs[len(msgs)-1]
		u, err := url.Parse(last.URL)
		if err != nil || last.Kind != "binding_guide" {
			tt.Fatalf("%+v", last)
		}
		return u.Query().Get("link")
	}
	linkBody := func(link, code string, confirmation ...string) string {
		m := map[string]any{"link_token": link}
		if code != "" {
			m["oauth_code"] = code
		}
		if len(confirmation) > 0 {
			m["confirmation"] = confirmation[0]
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	prepare := func(token, link, code string) (int, []byte) {
		return do(token, "POST", "/v1/channels/bindings:prepare", linkBody(link, code))
	}
	confirm := func(token, link, code, confirmation string) (int, []byte) {
		return do(token, "POST", "/v1/channels/bindings:confirm", linkBody(link, code, confirmation))
	}
	preview := func(token, link, corp, userID string) chn.BindingPreview {
		tt.Helper()
		status, raw := prepare(token, link, oauth(corp, userID))
		if status != 200 {
			tt.Fatalf("打开绑定链接：%d %s", status, raw)
		}
		var p chn.BindingPreview
		decode(raw, &p)
		return p
	}
	// bindAccount 走完整流程：渠道消息 → 绑定链接 → 内置浏览器中以妙洽账号打开并确认
	bindAccount := func(tenant, subject, corp, userID string) chn.Binding {
		tt.Helper()
		link := guide(corp, userID)
		token := web(subject, tenant, "web-"+subject)
		p := preview(token, link, corp, userID)
		status, raw := confirm(token, link, oauth(corp, userID), string(p.Confirmation))
		var out chn.BindingConfirmation
		if status != 200 {
			tt.Fatalf("确认绑定：%d %s", status, raw)
		}
		decode(raw, &out)
		if out.Outcome != "bound" || out.Binding.State != "bound" || out.Binding.Version != 1 || out.Binding.Subject != subject {
			tt.Fatalf("%+v", out)
		}
		return out.Binding
	}
	bindingRow := func(id string) (state, reason string, version, confirmed int64) {
		tt.Helper()
		if err := chnOwner.QueryRow(ctx, `SELECT state, coalesce(reason,''), version, revoke_confirmed_version FROM chn_bindings WHERE id=$1`, id).
			Scan(&state, &reason, &version, &confirmed); err != nil {
			tt.Fatal(err)
		}
		return
	}
	sso := func(corp, userID string) *chapi.SSOResult {
		tt.Helper()
		res, err := callbacks.ChannelSSO(gw, "wecom", oauth(corp, userID))
		if err != nil {
			tt.Fatal(err)
		}
		return res
	}
	login := func(res *chapi.SSOResult, reason string) {
		tt.Helper()
		if res.Outcome != "login" || res.Reason != reason || res.Session != nil {
			tt.Fatalf("应转登录页（%s）：%+v", reason, res)
		}
	}
	session := func(corp, userID, subject string) string {
		tt.Helper()
		res := sso(corp, userID)
		if res.Outcome != "session" || res.Session == nil || res.Subject != subject || res.Tenant != t1 {
			tt.Fatalf("%+v", res)
		}
		if !res.Session.ExpiresAt.Equal(res.Session.IssuedAt.Add(12 * time.Hour)) {
			tt.Fatalf("免登会话须为12小时：%+v", res.Session)
		}
		return string(res.Session.ID)
	}
	liveSessions := func(binding string) int {
		return count(iamOwner, `SELECT count(*) FROM identity_channel_sessions WHERE binding_id=$1 AND revoked_at IS NULL`, binding)
	}
	unbind := func(token, id string, version int64, reason string) (int, []byte) {
		body := fmt.Sprintf(`{"expected_version":%d}`, version)
		if reason != "" {
			body = fmt.Sprintf(`{"expected_version":%d,"reason":%q}`, version, reason)
		}
		return do(token, "POST", "/v1/channels/bindings/"+id+"/unbind", body)
	}
	// 时钟：冻结在“现在”，需要凭据或会话过期时前移
	base := time.Now().UTC().Truncate(time.Second)
	clock.set(base, 0)
	advance := func(d time.Duration) { base = base.Add(d); clock.set(base, 0) }
	retry := func() int {
		tt.Helper()
		n, err := maintenance.RetryRevocations(svc(t1, "svc:channels"), 100)
		if err != nil {
			tt.Fatal(err)
		}
		return n
	}
	// 发件箱中的事件交给消费处理（模拟中继与消费者）：channels 自己的撤销重试、identity 的撤销重试、channels 的成员变化
	outboxEnvelopes := func(conn *pgx.Conn, typ, subject string) []outbox.Envelope {
		tt.Helper()
		rows, err := conn.Query(ctx, `SELECT envelope FROM kernel_outbox WHERE envelope->>'type'=$1 AND envelope->>'subject'=$2 ORDER BY seq`, typ, subject)
		if err != nil {
			tt.Fatal(err)
		}
		defer rows.Close()
		var out []outbox.Envelope
		for rows.Next() {
			var raw []byte
			var e outbox.Envelope
			if err := rows.Scan(&raw); err != nil || json.Unmarshal(raw, &e) != nil {
				tt.Fatal(err)
			}
			out = append(out, e)
		}
		return out
	}
	consume := func(m runtime.Module, e *runtime.Env, consumer, typ string, envelope outbox.Envelope) error {
		for _, c := range m.Consumers(e) {
			if c.Name == consumer {
				ectx := reqctx.Service(reqctx.With(ctx, reqctx.Context{Tenant: envelope.Tenant, Cell: envelope.Cell, Purpose: "event"}), c.Subsystem)
				return e.DB.Tx(ectx, func(tx pgx.Tx) error { return c.Handlers[typ](ectx, tx, &envelope) })
			}
		}
		tt.Fatalf("没有消费者 %s", consumer)
		return nil
	}
	revokingEvent := func(binding string) outbox.Envelope {
		tt.Helper()
		for _, e := range outboxEnvelopes(chnOwner, "channels.binding.changed.v1", binding) {
			var d struct {
				State string `json:"state"`
			}
			_ = json.Unmarshal(e.Data, &d)
			if d.State == "revoking" {
				return e
			}
		}
		tt.Fatalf("绑定 %s 没有失效事件", binding)
		return outbox.Envelope{}
	}

	var instA chn.Installation
	const corpA, corpB = "corp-a", "corp-b"
	run("安装：管理员与强认证、待验证、验证需本人绑定、启用", func(t *testing.T) {
		expect(web(bd, t1, "s"), "POST", "/v1/channels/installations", `{"channel":"wecom"}`, 403, "forbidden")
		expect(stale(admin, t1), "POST", "/v1/channels/installations", `{"channel":"wecom"}`, 403, "mfa_required")
		instA = install(t1, admin, corpA)
		if instA.State != "pending_verification" || instA.CorpID.Value != corpA || instA.Version != 2 {
			t.Fatalf("%+v", instA)
		}
		expect(web(admin, t1, "s"), "POST", "/v1/channels/installations", `{"channel":"wecom"}`, 409, "channel_already_installed")
		// 同一企业不能被另一租户安装
		var started chn.InstallationStarted
		decode(expect(web(admin2, t2, "s"), "POST", "/v1/channels/installations", `{"channel":"wecom"}`, 200, ""), &started)
		u, _ := started.AuthorizeURL.Get()
		if _, err := callbacks.InstallCallback(gw, "wecom", u.Query().Get("state"), wc.Corp(corpA, "企业A")); errCode(err) != "corp_installed_elsewhere" {
			t.Fatalf("%v", err)
		}
		// 重新开始授权：版本加一，此前的 state 失效
		decode(expect(web(admin2, t2, "s"), "POST", "/v1/channels/installations", `{"channel":"wecom"}`, 200, ""), &started)
		if started.Installation.Version != 2 || started.Installation.State != "authorizing" {
			t.Fatalf("%+v", started.Installation)
		}
		if _, err := callbacks.InstallCallback(gw, "wecom", u.Query().Get("state"), wc.Corp(corpB, "企业B")); err == nil {
			t.Fatal("旧版本的授权 state 必须失效")
		}
		u, _ = started.AuthorizeURL.Get()
		if inst, err := callbacks.InstallCallback(gw, "wecom", u.Query().Get("state"), wc.Corp(corpB, "企业B")); err != nil || inst.CorpID != corpB {
			t.Fatalf("%+v %v", inst, err)
		}
		// 验证：须管理员本人已在该企业绑定（作为“发送测试消息”的替代证明）
		verify := fmt.Sprintf(`{"expected_version":%d}`, instA.Version)
		expect(web(admin, t1, "s"), "POST", "/v1/channels/installations/"+string(instA.ID)+"/verify", verify, 409, "verification_binding_required")
		bindAccount(t1, admin, corpA, "u-admin")
		expect(stale(admin, t1), "POST", "/v1/channels/installations/"+string(instA.ID)+"/verify", verify, 403, "mfa_required")
		var enabled chn.Installation
		decode(expect(web(admin, t1, "s"), "POST", "/v1/channels/installations/"+string(instA.ID)+"/verify", verify, 200, ""), &enabled)
		if enabled.State != "enabled" || enabled.BoundCount != 1 {
			t.Fatalf("%+v", enabled)
		}
		instA = enabled
		// 安装状态只对管理员可见，且只看到本租户
		expect(web(bd, t1, "s"), "GET", "/v1/channels/installations", "", 403, "forbidden")
		if list := installations(web(admin, t1, "s")); len(list) != 1 || list[0].ID != instA.ID {
			t.Fatalf("%+v", list)
		}
		if msgs := wc.Messages(corpA, "u-admin"); len(msgs) != 2 || msgs[1].Kind != "binding_succeeded" {
			t.Fatalf("%+v", msgs)
		}
	})

	var bdBinding chn.Binding
	run("用例8：绑定链接转发到他人企业微信被拒绝；内置浏览器以外只提示改用企业微信", func(t *testing.T) {
		link := guide(corpA, "u-bd")
		token := web(bd, t1, "web-bd")
		// 在内置浏览器以外打开：没有渠道网页授权码
		if status, raw := prepare(token, link, ""); status != 400 || codeOf(raw) != "channel_browser_required" {
			t.Fatalf("%d %s", status, raw)
		}
		// 转发：他人在自己的企业微信中打开
		if status, raw := prepare(web(dm, t1, "web-dm"), link, oauth(corpA, "u-dm")); status != 403 || codeOf(raw) != "channel_identity_mismatch" {
			t.Fatalf("%d %s", status, raw)
		}
		// 链接已作废，原账号也不能再用
		if status, raw := prepare(token, link, oauth(corpA, "u-bd")); status != 410 || codeOf(raw) != "binding_link_voided" {
			t.Fatalf("%d %s", status, raw)
		}
		if n := count(chnOwner, `SELECT count(*) FROM chn_bindings WHERE channel_user_id='u-bd'`); n != 0 {
			t.Fatalf("不得产生绑定：%d", n)
		}
		// 免登会话不能用来绑定（须本人交互登录）
		adminSession := session(corpA, "u-admin", admin)
		link = guide(corpA, "u-bd")
		if status, raw := prepare(channelTok(admin, t1, adminSession), link, oauth(corpA, "u-bd")); status != 403 || codeOf(raw) != "interactive_login_required" {
			t.Fatalf("%d %s", status, raw)
		}
		// 链接过期
		advance(11 * time.Minute)
		if status, raw := prepare(token, link, oauth(corpA, "u-bd")); status != 410 || codeOf(raw) != "binding_link_expired" {
			t.Fatalf("%d %s", status, raw)
		}
	})

	run("用例9：固定会话后换人或会话替换被拒绝；已完成的链接重放被拒绝且不产生绑定或通知", func(t *testing.T) {
		// 同一成员换了 Web 会话
		link := guide(corpA, "u-bd")
		p := preview(web(bd, t1, "web-bd-1"), link, corpA, "u-bd")
		if p.Subject != bd || p.CorpID != corpA || p.ChannelUserID != "u-bd" {
			t.Fatalf("%+v", p)
		}
		if status, raw := confirm(web(bd, t1, "web-bd-2"), link, oauth(corpA, "u-bd"), string(p.Confirmation)); status != 409 || codeOf(raw) != "binding_session_mismatch" {
			t.Fatalf("%d %s", status, raw)
		}
		if status, raw := confirm(web(bd, t1, "web-bd-1"), link, oauth(corpA, "u-bd"), string(p.Confirmation)); status != 410 || codeOf(raw) != "binding_link_voided" {
			t.Fatalf("%d %s", status, raw)
		}
		// 退出后以他人登录
		link = guide(corpA, "u-bd")
		p = preview(web(bd, t1, "web-bd-1"), link, corpA, "u-bd")
		if status, raw := confirm(web(dm, t1, "web-bd-1"), link, oauth(corpA, "u-bd"), string(p.Confirmation)); status != 409 || codeOf(raw) != "binding_session_mismatch" {
			t.Fatalf("%d %s", status, raw)
		}
		// 内容改变（摘要不一致）
		link = guide(corpA, "u-bd")
		token := web(bd, t1, "web-bd-1")
		p = preview(token, link, corpA, "u-bd")
		if status, raw := confirm(token, link, oauth(corpA, "u-bd"), strings.Repeat("0", 64)); status != 409 || codeOf(raw) != "binding_request_changed" {
			t.Fatalf("%d %s", status, raw)
		}
		status, raw := confirm(token, link, oauth(corpA, "u-bd"), string(p.Confirmation))
		var first chn.BindingConfirmation
		if status != 200 {
			t.Fatalf("%d %s", status, raw)
		}
		decode(raw, &first)
		bdBinding = first.Binding
		notified := len(wc.Messages(corpA, "u-bd"))
		// 同一会话、同一渠道身份、内容未变的重试：返回原结果
		var again chn.BindingConfirmation
		decode(expect(token, "POST", "/v1/channels/bindings:confirm", linkBody(link, oauth(corpA, "u-bd"), string(p.Confirmation)), 200, ""), &again)
		if again.Outcome != "already_bound" || again.Binding.ID != first.Binding.ID {
			t.Fatalf("%+v", again)
		}
		// 其他会话、其他渠道身份、改变内容的重放：拒绝
		for _, c := range []struct{ token, code, confirmation string }{
			{web(bd, t1, "web-bd-9"), oauth(corpA, "u-bd"), string(p.Confirmation)},
			{token, oauth(corpA, "u-dm"), string(p.Confirmation)},
			{token, oauth(corpA, "u-bd"), strings.Repeat("a", 64)},
			{web(dm, t1, "web-bd-1"), oauth(corpA, "u-bd"), string(p.Confirmation)},
		} {
			if status, raw := confirm(c.token, link, c.code, c.confirmation); status != 410 || codeOf(raw) != "binding_link_used" {
				t.Fatalf("%d %s", status, raw)
			}
		}
		if status, raw := prepare(token, link, oauth(corpA, "u-bd")); status != 410 || codeOf(raw) != "binding_link_used" {
			t.Fatalf("%d %s", status, raw)
		}
		if n := count(chnOwner, `SELECT count(*) FROM chn_bindings WHERE channel_user_id='u-bd'`); n != 1 {
			t.Fatalf("重放不得产生绑定：%d", n)
		}
		if got := len(wc.Messages(corpA, "u-bd")); got != notified {
			t.Fatalf("重放不得再次通知：%d → %d", notified, got)
		}
		// 已绑定账号再发消息：不再收到绑定引导
		if res, err := inbound(corpA, "u-bd", clock.Now()); err != nil || res.Outcome != chapi.InboundBound {
			t.Fatalf("%+v %v", res, err)
		}
	})

	run("用例10：其他租户企业的账号不能绑定到本租户；同一渠道账号不能绑定第二个成员", func(t *testing.T) {
		// 乙租户企业的账号收到的链接，在甲租户成员会话中确认
		link := guide(corpB, "u-x")
		if status, raw := prepare(web(dm, t1, "web-dm"), link, oauth(corpB, "u-x")); status != 403 || codeOf(raw) != "channel_tenant_mismatch" {
			t.Fatalf("%d %s", status, raw)
		}
		// 本租户企业的账号，持乙租户成员会话
		link = guide(corpA, "u-y")
		if status, raw := prepare(web(admin2, t2, "web-admin2"), link, oauth(corpA, "u-y")); status != 403 || codeOf(raw) != "channel_tenant_mismatch" {
			t.Fatalf("%d %s", status, raw)
		}
		// 同一渠道账号的两条链接分别固定到两名成员：先确认者绑定，后确认者被拒绝
		l1 := guide(corpA, "u-shared")
		p1 := preview(web(dm, t1, "web-dm"), l1, corpA, "u-shared")
		l2 := guide(corpA, "u-shared")
		p2 := preview(web(ex, t1, "web-ex"), l2, corpA, "u-shared")
		if status, raw := confirm(web(dm, t1, "web-dm"), l1, oauth(corpA, "u-shared"), string(p1.Confirmation)); status != 200 {
			t.Fatalf("%d %s", status, raw)
		}
		if status, raw := confirm(web(ex, t1, "web-ex"), l2, oauth(corpA, "u-shared"), string(p2.Confirmation)); status != 409 || codeOf(raw) != "channel_account_already_bound" {
			t.Fatalf("%d %s", status, raw)
		}
		// 已绑定的成员不能再绑定第二个账号
		l3 := guide(corpA, "u-dm2")
		p3 := preview(web(dm, t1, "web-dm"), l3, corpA, "u-dm2")
		if status, raw := confirm(web(dm, t1, "web-dm"), l3, oauth(corpA, "u-dm2"), string(p3.Confirmation)); status != 409 || codeOf(raw) != "member_already_bound" {
			t.Fatalf("%d %s", status, raw)
		}
		if n := count(chnOwner, `SELECT count(*) FROM chn_bindings WHERE state='bound' AND (channel_user_id IN ('u-shared','u-dm2','u-x','u-y'))`); n != 1 {
			t.Fatalf("%d", n)
		}
		// 绑定列表：成员只看到自己的，管理员看到全部；按页读取
		var mine chn.BindingList
		decode(expect(web(dm, t1, "s"), "GET", "/v1/channels/bindings", "", 200, ""), &mine)
		if len(mine.Bindings) != 1 || mine.Bindings[0].Subject != dm {
			t.Fatalf("%+v", mine)
		}
		var page chn.BindingList
		decode(expect(web(admin, t1, "s"), "GET", "/v1/channels/bindings?limit=2", "", 200, ""), &page)
		cursor, ok := page.NextCursor.Get()
		if len(page.Bindings) != 2 || !page.Truncated || !ok {
			t.Fatalf("%+v", page)
		}
		var rest chn.BindingList
		decode(expect(web(admin, t1, "s"), "GET", "/v1/channels/bindings?limit=2&cursor="+string(cursor), "", 200, ""), &rest)
		if len(rest.Bindings) != 1 || rest.Truncated {
			t.Fatalf("%+v", rest)
		}
		expect(web(dm, t1, "s"), "GET", "/v1/channels/bindings?limit=2&cursor="+string(cursor), "", 400, "invalid_cursor")
	})

	run("用例11：免登会话；解绑后新的免登被拒绝、已有会话下一次请求被拒绝；失效中直到撤销确认且凭据过期", func(t *testing.T) {
		chs := session(corpA, "u-bd", bd)
		token := channelTok(bd, t1, chs)
		expect(token, "GET", "/v1/channels/bindings", "", 200, "")
		// 免登会话不满足强认证（须重新完成 TOTP）
		adminSession := session(corpA, "u-admin", admin)
		expect(channelTok(admin, t1, adminSession), "POST", "/v1/identity/org-units", `{"name":"免登","parent_id":"root"}`, 403, "mfa_required")
		// 免登会话标识缺失、伪造或属于他人
		expect(channelTok(bd, t1, ""), "GET", "/v1/channels/bindings", "", 401, "channel_session_invalid")
		expect(channelTok(bd, t1, ids.New("chs")), "GET", "/v1/channels/bindings", "", 401, "channel_session_invalid")
		expect(channelTok(dm, t1, chs), "GET", "/v1/channels/bindings", "", 401, "channel_session_invalid")
		// 12小时绝对时长
		advance(12 * time.Hour)
		expect(token, "GET", "/v1/channels/bindings", "", 401, "channel_session_expired")
		chs = session(corpA, "u-bd", bd)
		token = channelTok(bd, t1, chs)
		// 本人解绑：立即失效中，已有会话的下一次请求被拒绝，新的免登被拒绝
		expect(web(bd, t1, "s"), "POST", "/v1/channels/bindings/"+string(bdBinding.ID)+"/unbind", `{"expected_version":9}`, 409, "version_conflict")
		var unbound chn.Binding
		decode(expect(web(bd, t1, "s"), "POST", "/v1/channels/bindings/"+string(bdBinding.ID)+"/unbind", `{"expected_version":1}`, 200, ""), &unbound)
		if unbound.State != "revoking" || unbound.Version != 2 || unbound.Reason.Value != "member_unbind" {
			t.Fatalf("%+v", unbound)
		}
		expect(token, "GET", "/v1/channels/bindings", "", 401, "channel_session_revoked")
		login(sso(corpA, "u-bd"), "binding_inactive")
		target, err := messaging.DeliveryTarget(svc(t1, "svc:briefing"), bd)
		if err != nil || target.Channel != "in_app" || target.Reason != "not_bound" {
			t.Fatalf("%+v %v", target, err)
		}
		// 撤销已确认，但最后一张签发凭据尚未过期（含5秒偏差）：仍是失效中
		if state, _, version, confirmed := bindingRow(string(bdBinding.ID)); state != "revoking" || confirmed != version {
			t.Fatalf("%s %d/%d", state, confirmed, version)
		}
		advance(30 * time.Second)
		retry()
		if state, _, _, _ := bindingRow(string(bdBinding.ID)); state != "revoking" {
			t.Fatalf("凭据过期（含偏差）前不得完成：%s", state)
		}
		advance(5 * time.Second)
		if n := retry(); n < 1 {
			t.Fatalf("%d", n)
		}
		if state, _, _, _ := bindingRow(string(bdBinding.ID)); state != "invalidated" || liveSessions(string(bdBinding.ID)) != 0 {
			t.Fatalf("%s %d", state, liveSessions(string(bdBinding.ID)))
		}
		login(sso(corpA, "u-bd"), "not_bound")
		// 重复解绑
		expect(web(bd, t1, "s"), "POST", "/v1/channels/bindings/"+string(bdBinding.ID)+"/unbind", `{"expected_version":2}`, 409, "binding_not_bound")
	})

	run("用例11：管理员强制解绑须强认证与原因；非管理员看不到他人绑定", func(t *testing.T) {
		b := bindAccount(t1, bd, corpA, "u-bd")
		if b.ID == bdBinding.ID {
			t.Fatal("重新绑定须是新的绑定")
		}
		bdBinding = b
		if status, raw := unbind(web(dm, t1, "s"), string(b.ID), 1, "x"); status != 404 || codeOf(raw) != "binding_not_found" {
			t.Fatalf("%d %s", status, raw)
		}
		if status, raw := unbind(stale(admin, t1), string(b.ID), 1, "离职交接"); status != 403 || codeOf(raw) != "mfa_required" {
			t.Fatalf("%d %s", status, raw)
		}
		if status, raw := unbind(web(admin, t1, "s"), string(b.ID), 1, ""); status != 400 || codeOf(raw) != "reason_required" {
			t.Fatalf("%d %s", status, raw)
		}
		if status, raw := unbind(web(admin2, t2, "s"), string(b.ID), 1, "跨租户"); status != 404 || codeOf(raw) != "binding_not_found" {
			t.Fatalf("%d %s", status, raw)
		}
		status, raw := unbind(web(admin, t1, "s"), string(b.ID), 1, "离职交接")
		var out chn.Binding
		decode(raw, &out)
		if status != 200 || out.Reason.Value != "admin_unbind" || out.Note.Value != "离职交接" {
			t.Fatalf("%d %s", status, raw)
		}
	})

	run("用例11：同步撤销失败时经事件重试与对账撤销", func(t *testing.T) {
		b := bindAccount(t1, bd, corpA, "u-bd")
		chs := session(corpA, "u-bd", bd)
		hooked.failRevoke.Store(true)
		status, raw := unbind(web(bd, t1, "s"), string(b.ID), 1, "")
		if status != 200 {
			t.Fatalf("同步撤销失败不影响解绑：%d %s", status, raw)
		}
		if _, _, _, confirmed := bindingRow(string(b.ID)); confirmed != 0 {
			t.Fatalf("撤销未送达：%d", confirmed)
		}
		// 撤销送达前已建立的会话仍可用；新的免登已被拒绝
		expect(channelTok(bd, t1, chs), "GET", "/v1/channels/bindings", "", 200, "")
		login(sso(corpA, "u-bd"), "binding_inactive")
		// 事件路径一：channels 自己的撤销重试（identity 不可用时返回错误，由消费者重试）
		e := revokingEvent(string(b.ID))
		if err := consume(module, env, "channels.binding-revocations", "channels.binding.changed.v1", e); errCode(err) != "identity_unavailable" {
			t.Fatalf("%v", err)
		}
		// 事件路径二：identity 消费同一事件，直接抬高水位并作废会话
		if err := consume(idModule, idEnv, "identity.channel-revocations", "channels.binding.changed.v1", e); err != nil {
			t.Fatal(err)
		}
		expect(channelTok(bd, t1, chs), "GET", "/v1/channels/bindings", "", 401, "channel_session_revoked")
		// 撤销仍未在 channels 确认：凭据过期后也不显示为已完成
		advance(36 * time.Second)
		retry()
		if state, _, _, _ := bindingRow(string(b.ID)); state != "revoking" {
			t.Fatal(state)
		}
		hooked.failRevoke.Store(false)
		if err := consume(module, env, "channels.binding-revocations", "channels.binding.changed.v1", e); err != nil {
			t.Fatal(err)
		}
		if state, _, _, _ := bindingRow(string(b.ID)); state != "invalidated" {
			t.Fatal(state)
		}

		// 对账：identity 中仍有有效会话、channels 中绑定已失效
		b = bindAccount(t1, bd, corpA, "u-bd")
		chs = session(corpA, "u-bd", bd)
		hooked.failRevoke.Store(true)
		if status, raw := unbind(web(bd, t1, "s"), string(b.ID), 1, ""); status != 200 {
			t.Fatalf("%d %s", status, raw)
		}
		if _, err := maintenance.ReconcileChannelSessions(svc(t1, "svc:channels")); errCode(err) != "identity_unavailable" {
			t.Fatalf("%v", err)
		}
		expect(channelTok(bd, t1, chs), "GET", "/v1/channels/bindings", "", 200, "")
		hooked.failRevoke.Store(false)
		n, err := maintenance.ReconcileChannelSessions(svc(t1, "svc:channels"))
		if err != nil || n != 1 {
			t.Fatalf("%d %v", n, err)
		}
		expect(channelTok(bd, t1, chs), "GET", "/v1/channels/bindings", "", 401, "channel_session_revoked")
		// 有效绑定的会话不受对账影响
		if n, err := maintenance.ReconcileChannelSessions(svc(t1, "svc:channels")); err != nil || n != 0 {
			t.Fatalf("%d %v", n, err)
		}
		// 维护只接受带租户的服务主体
		if _, err := maintenance.RetryRevocations(svc(t1, "svc:channels"), 0); errCode(err) != "invalid_request" {
			t.Fatalf("%v", err)
		}
		if _, err := maintenance.RetryRevocations(reqctx.With(ctx, reqctx.Context{Tenant: t1, Cell: "cell-01", Principal: reqctx.Principal{ID: bd, Kind: "user"}}), 10); errCode(err) != "forbidden" {
			t.Fatalf("%v", err)
		}
		advance(36 * time.Second)
		retry()
	})

	run("用例11a：核对通过、签发进行中时解绑", func(t *testing.T) {
		// 签发已写入会话、尚未返回 channels 时解绑：撤销作废该会话，channels 不交出会话
		b := bindAccount(t1, bd, corpA, "u-bd")
		reached, release := make(chan string, 1), make(chan struct{})
		hooked.hooks(nil, func(s *idapi.ChannelSession) { reached <- string(s.ID); <-release })
		result := make(chan *chapi.SSOResult, 1)
		go func() {
			res, _ := callbacks.ChannelSSO(gw, "wecom", oauth(corpA, "u-bd"))
			result <- res
		}()
		chs := <-reached
		if status, raw := unbind(web(bd, t1, "s"), string(b.ID), 1, ""); status != 200 || codeOf(raw) != "" {
			t.Fatalf("%d %s", status, raw)
		}
		close(release)
		hooked.hooks(nil, nil)
		login(<-result, "binding_inactive")
		expect(channelTok(bd, t1, chs), "GET", "/v1/channels/bindings", "", 401, "channel_session_revoked")
		if state, _, _, _ := bindingRow(string(b.ID)); state != "revoking" {
			t.Fatalf("凭据过期前须为失效中：%s", state)
		}
		advance(36 * time.Second)
		retry()
		if state, _, _, _ := bindingRow(string(b.ID)); state != "invalidated" || liveSessions(string(b.ID)) != 0 {
			t.Fatalf("%s %d", state, liveSessions(string(b.ID)))
		}
	})

	run("用例11b：撤销先到、旧版本签发后到：按水位拒绝，不创建会话", func(t *testing.T) {
		b := bindAccount(t1, bd, corpA, "u-bd")
		reached, release := make(chan struct{}), make(chan struct{})
		hooked.hooks(func(*idapi.IssueChannelSessionRequest) error { close(reached); <-release; return nil }, nil)
		result := make(chan *chapi.SSOResult, 1)
		go func() {
			res, _ := callbacks.ChannelSSO(gw, "wecom", oauth(corpA, "u-bd"))
			result <- res
		}()
		<-reached
		if status, raw := unbind(web(bd, t1, "s"), string(b.ID), 1, ""); status != 200 {
			t.Fatalf("%d %s", status, raw)
		}
		close(release)
		hooked.hooks(nil, nil)
		login(<-result, "session_rejected")
		if n := count(iamOwner, `SELECT count(*) FROM identity_channel_sessions WHERE binding_id=$1`, string(b.ID)); n != 0 {
			t.Fatalf("不得创建会话：%d", n)
		}
		advance(36 * time.Second)
		retry()
	})

	run("用例11c：签发重试跨越解绑与重新绑定：旧绑定版本被拒绝，新绑定正常", func(t *testing.T) {
		old := bindAccount(t1, bd, corpA, "u-bd")
		var captured *idapi.IssueChannelSessionRequest
		hooked.hooks(func(r *idapi.IssueChannelSessionRequest) error {
			captured = r
			return errors.New("超时（测试注入）")
		}, nil)
		login(sso(corpA, "u-bd"), "identity_unavailable")
		hooked.hooks(nil, nil)
		if status, raw := unbind(web(bd, t1, "s"), string(old.ID), 1, ""); status != 200 {
			t.Fatalf("%d %s", status, raw)
		}
		renewed := bindAccount(t1, bd, corpA, "u-bd")
		// 旧请求的重试到达 identity（凭据仍在有效期内）
		if _, err := identitySessions.IssueChannelSession(svc(t1, "svc:channels"), captured); errCode(err) != "binding_revoked" {
			t.Fatalf("%v", err)
		}
		chs := session(corpA, "u-bd", bd)
		expect(channelTok(bd, t1, chs), "GET", "/v1/channels/bindings", "", 200, "")
		if n := count(iamOwner, `SELECT count(*) FROM identity_channel_sessions WHERE binding_id=$1`, string(old.ID)); n != 0 {
			t.Fatalf("%d", n)
		}
		bdBinding = renewed
	})

	run("用例11d：签发在 channels 侧超时而 identity 继续执行，同时同步撤销失败", func(t *testing.T) {
		b := bdBinding
		// 第一次：签发请求滞留，凭据过期后才到达 identity → 拒绝
		var late *idapi.IssueChannelSessionRequest
		hooked.hooks(func(r *idapi.IssueChannelSessionRequest) error {
			late = r
			return errors.New("超时（测试注入）")
		}, nil)
		login(sso(corpA, "u-bd"), "identity_unavailable")
		// 第二次：滞留的请求在凭据过期前到达并写入会话
		var early *idapi.IssueChannelSessionRequest
		hooked.hooks(func(r *idapi.IssueChannelSessionRequest) error {
			early = r
			return errors.New("超时（测试注入）")
		}, nil)
		login(sso(corpA, "u-bd"), "identity_unavailable")
		hooked.hooks(nil, nil)
		hooked.failRevoke.Store(true)
		if status, raw := unbind(web(bd, t1, "s"), string(b.ID), 1, ""); status != 200 {
			t.Fatalf("%d %s", status, raw)
		}
		written, err := identitySessions.IssueChannelSession(svc(t1, "svc:channels"), early)
		if err != nil || written.Outcome != "issued" {
			t.Fatalf("撤销未送达时凭据有效期内的签发写入会话：%+v %v", written, err)
		}
		advance(26 * time.Second)
		if _, err := identitySessions.IssueChannelSession(svc(t1, "svc:channels"), late); errCode(err) != "credential_expired" {
			t.Fatalf("%v", err)
		}
		// 撤销确认且凭据过期前，失效不显示为已完成
		advance(10 * time.Second)
		retry()
		if state, _, _, _ := bindingRow(string(b.ID)); state != "revoking" {
			t.Fatal(state)
		}
		var listed chn.BindingList
		decode(expect(web(bd, t1, "s"), "GET", "/v1/channels/bindings?state=revoking", "", 200, ""), &listed)
		if len(listed.Bindings) != 1 || listed.Bindings[0].ID != b.ID {
			t.Fatalf("%+v", listed)
		}
		// 撤销重试送达：作废已写入的会话并完成失效
		hooked.failRevoke.Store(false)
		retry()
		expect(channelTok(bd, t1, string(written.ID)), "GET", "/v1/channels/bindings", "", 401, "channel_session_revoked")
		if state, _, _, _ := bindingRow(string(b.ID)); state != "invalidated" || liveSessions(string(b.ID)) != 0 {
			t.Fatalf("%s", state)
		}
	})

	run("用例11e：签发事务核验水位后、写入会话前发起撤销：撤销等待并作废该会话", func(t *testing.T) {
		b := bindAccount(t1, bd, corpA, "u-bd")
		session(corpA, "u-bd", bd) // 建立水位行
		// 屏障：另一事务以同一凭据标识写入未提交的会话（挂在另一绑定上），签发事务在写入会话时等待它
		barrier, err := pgx.Connect(ctx, iam.OwnerDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer barrier.Close(ctx)
		other := ids.New("chb")
		if _, err := barrier.Exec(ctx, `INSERT INTO identity_channel_revocations(binding_id,tenant_id,watermark) VALUES($1,$2,0)`, other, t1); err != nil {
			t.Fatal(err)
		}
		tx, err := barrier.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		credential := "barrier-credential-" + suffix
		if _, err := tx.Exec(ctx, `INSERT INTO identity_channel_sessions(id,tenant_id,binding_id,binding_version,subject,member_version,credential_id,issued_at,expires_at)
 VALUES($1,$2,$3,1,$4,1,$5,now(),now()+interval '1 hour')`, ids.New("chs"), t1, other, bd, credential); err != nil {
			t.Fatal(err)
		}
		issued := make(chan error, 1)
		var out *idapi.ChannelSession
		go func() {
			var err error
			out, err = identitySessions.IssueChannelSession(svc(t1, "svc:channels"), &idapi.IssueChannelSessionRequest{CredentialID: credential,
				BindingID: identityapi.ChannelBindingID(b.ID), BindingVersion: 1, Subject: bd, MemberVersion: 1, CredentialExpiresAt: clock.Now().Add(30 * time.Second)})
			issued <- err
		}()
		waitForLockWaiters(t, ctx, iam, 1)
		unbound := make(chan int, 1)
		go func() {
			status, _ := unbind(web(bd, t1, "s"), string(b.ID), 1, "")
			unbound <- status
		}()
		// 撤销在同一水位行上等待签发事务
		waitForLockWaiters(t, ctx, iam, 2)
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-issued; err != nil || out.Outcome != "issued" {
			t.Fatalf("%+v %v", out, err)
		}
		if status := <-unbound; status != 200 {
			t.Fatal(status)
		}
		expect(channelTok(bd, t1, string(out.ID)), "GET", "/v1/channels/bindings", "", 401, "channel_session_revoked")
		if liveSessions(string(b.ID)) != 0 {
			t.Fatal("撤销后不得留有有效会话")
		}
		advance(36 * time.Second)
		retry()
	})

	run("identity 免登会话接口：服务授权、凭据规则与远程绑定一致", func(t *testing.T) {
		b := bindAccount(t1, bd, corpA, "u-bd")
		req := func(credential string, expires time.Time) *idapi.IssueChannelSessionRequest {
			return &idapi.IssueChannelSessionRequest{CredentialID: credential, BindingID: identityapi.ChannelBindingID(b.ID), BindingVersion: 1, Subject: bd, MemberVersion: 1, CredentialExpiresAt: expires}
		}
		session(corpA, "u-admin", admin) // 另一条有效会话，用于翻页
		type named struct {
			name string
			s    idapi.ChannelSessions
		}
		both := []named{{"本地", identitySessions}, {"远程", remoteSessions}}
		for _, n := range both {
			name, s := n.name, n.s
			cred := "credential-" + name + "-" + suffix
			cred = fmt.Sprintf("cred%x", sha256.Sum256([]byte(cred)))[:40]
			for _, who := range []string{"svc:detection", "svc:customer-context"} {
				if _, err := s.IssueChannelSession(svc(t1, who), req(cred, clock.Now().Add(30*time.Second))); errCode(err) != "forbidden" {
					t.Fatalf("%s：%s：%v", name, who, err)
				}
				if _, err := s.ListLiveChannelSessions(svc(t1, who), "", 10); errCode(err) != "forbidden" {
					t.Fatalf("%s：%v", name, err)
				}
				if _, err := s.MemberLifecycle(svc(t1, who), bd); errCode(err) != "forbidden" {
					t.Fatalf("%s：%v", name, err)
				}
			}
			if m, err := s.MemberLifecycle(svc(t1, "svc:channels"), bd); err != nil || m.State != "active" || m.MemberVersion != 1 || m.LeftVersion != 0 {
				t.Fatalf("%s：%+v %v", name, m, err)
			}
			if m, err := s.MemberLifecycle(svc(t1, "svc:channels"), admin2); err != nil || m.State != "absent" {
				t.Fatalf("%s：其他租户的成员在本租户为 absent：%+v %v", name, m, err)
			}
			if _, err := s.IssueChannelSession(svc(t1, "svc:channels"), req(cred, clock.Now().Add(5*time.Minute))); errCode(err) != "invalid_credential" {
				t.Fatalf("%s：有效期过长的凭据：%v", name, err)
			}
			future := req(cred, clock.Now().Add(30*time.Second))
			future.MemberVersion = 9
			if _, err := s.IssueChannelSession(svc(t1, "svc:channels"), future); errCode(err) != "invalid_credential" {
				t.Fatalf("%s：高于当前成员版本的绑定：%v", name, err)
			}
			first, err := s.IssueChannelSession(svc(t1, "svc:channels"), req(cred, clock.Now().Add(30*time.Second)))
			if err != nil || first.Outcome != "issued" {
				t.Fatalf("%s：%+v %v", name, first, err)
			}
			again, err := s.IssueChannelSession(svc(t1, "svc:channels"), req(cred, clock.Now().Add(30*time.Second)))
			if err != nil || again.Outcome != "replayed" || again.ID != first.ID {
				t.Fatalf("%s：%+v %v", name, again, err)
			}
			changed := req(cred, clock.Now().Add(30*time.Second))
			changed.Subject = dm
			if _, err := s.IssueChannelSession(svc(t1, "svc:channels"), changed); errCode(err) != "credential_conflict" {
				t.Fatalf("%s：%v", name, err)
			}
			// 其他租户的同一绑定标识
			if _, err := s.IssueChannelSession(svc(t2, "svc:channels"), req(cred+"x", clock.Now().Add(30*time.Second))); errCode(err) != "binding_conflict" {
				t.Fatalf("%s：%v", name, err)
			}
			page, err := s.ListLiveChannelSessions(svc(t1, "svc:channels"), "", 1)
			if err != nil || len(page.Bindings) != 1 || !page.Truncated {
				t.Fatalf("%s：%+v %v", name, page, err)
			}
			next, _ := page.NextCursor.Get()
			if _, err := s.ListLiveChannelSessions(svc(t2, "svc:channels"), string(next), 1); errCode(err) != "invalid_cursor" {
				t.Fatalf("%s：其他租户不能使用游标：%v", name, err)
			}
		}
		// 撤销：按版本幂等，水位只升不降
		for _, n := range both {
			name, s := n.name, n.s
			out, err := s.RevokeChannelSessions(svc(t1, "svc:channels"), &idapi.RevokeChannelSessionsRequest{BindingID: identityapi.ChannelBindingID(b.ID), BindingVersion: 2})
			want := "applied"
			if name == "远程" {
				want = "already_applied"
			}
			if err != nil || string(out.Outcome) != want || out.Watermark != 2 {
				t.Fatalf("%s：%+v %v", name, out, err)
			}
		}
		if liveSessions(string(b.ID)) != 0 {
			t.Fatal("撤销后不得留有有效会话")
		}
		// 收尾：channels 中的绑定以对账撤销到 identity 的水位后解绑
		if status, raw := unbind(web(bd, t1, "s"), string(b.ID), 1, ""); status != 200 {
			t.Fatalf("%d %s", status, raw)
		}
		advance(36 * time.Second)
		retry()
	})

	run("用例12：成员退出后免登被拒绝、旧会话下一次请求被拒绝、绑定失效", func(t *testing.T) {
		b := bindAccount(t1, lv, corpA, "u-lv")
		chs := session(corpA, "u-lv", lv)
		// 固定到该成员的未完成绑定请求
		link := guide(corpA, "u-lv2")
		preview(web(lv, t1, "web-lv"), link, corpA, "u-lv2")
		var s identityapi.Session
		decode(expect(web(admin, t1, "s"), "GET", "/v1/identity/members/"+lv, "", 200, ""), &s)
		expect(web(admin, t1, "s"), "PUT", "/v1/identity/members/"+lv, fmt.Sprintf(`{"state":"removed","roles":[],"expected_version":%d}`, s.MemberVersion), 200, "")
		// 事件送达前：identity 已拒绝——免登会话来自这次退出之前（401），Web 会话按成员状态（403）
		expect(channelTok(lv, t1, chs), "GET", "/v1/channels/bindings", "", 401, "channel_session_revoked")
		expect(web(lv, t1, "web-lv"), "GET", "/v1/channels/bindings", "", 403, "invalid_tenant_context")
		login(sso(corpA, "u-lv"), "member_inactive")
		// identity.member.changed.v1 送达 channels：绑定以 member_left 失效，固定到该成员的请求作废
		envelopes := outboxEnvelopes(iamOwner, "identity.member.changed.v1", t1)
		e := envelopes[len(envelopes)-1]
		if err := consume(module, env, "channels.member-changes", "identity.member.changed.v1", e); err != nil {
			t.Fatal(err)
		}
		if state, reason, version, _ := bindingRow(string(b.ID)); state != "revoking" || reason != "member_left" || version != 2 {
			t.Fatalf("%s %s %d", state, reason, version)
		}
		if n := count(chnOwner, `SELECT count(*) FROM chn_binding_requests WHERE channel_user_id='u-lv2' AND state='voided' AND void_reason='member_left'`); n != 1 {
			t.Fatalf("%d", n)
		}
		login(sso(corpA, "u-lv"), "binding_inactive")
		// 重复送达不改变结果；缺少租户上下文的事件不静默跳过
		if err := consume(module, env, "channels.member-changes", "identity.member.changed.v1", e); err != nil {
			t.Fatal(err)
		}
		bad := e
		bad.Tenant, bad.Cell = "", ""
		if err := consume(module, env, "channels.member-changes", "identity.member.changed.v1", bad); err == nil {
			t.Fatal("缺少租户上下文的成员事件须返回错误")
		}
		advance(36 * time.Second)
		retry()
		if state, _, _, _ := bindingRow(string(b.ID)); state != "invalidated" {
			t.Fatal(state)
		}
	})

	run("用例12：退出事件延迟送达时重新加入，旧绑定与旧会话不恢复；重新绑定后迟到的退出事件不影响新绑定", func(t *testing.T) {
		old := bindAccount(t1, rj, corpA, "u-rj")
		oldSession := session(corpA, "u-rj", rj)
		var oldVersion int64
		if err := chnOwner.QueryRow(ctx, `SELECT member_version FROM chn_bindings WHERE id=$1`, string(old.ID)).Scan(&oldVersion); err != nil || oldVersion != 1 {
			t.Fatalf("%d %v", oldVersion, err)
		}
		member := func() identityapi.Session {
			var s identityapi.Session
			decode(expect(web(admin, t1, "s"), "GET", "/v1/identity/members/"+rj, "", 200, ""), &s)
			return s
		}
		// 退出；identity.member.changed.v1 不送达 channels（模拟中继或消费延迟）
		m := member()
		expect(web(admin, t1, "s"), "PUT", "/v1/identity/members/"+rj, fmt.Sprintf(`{"state":"removed","roles":[],"expected_version":%d}`, m.MemberVersion), 200, "")
		envelopes := outboxEnvelopes(iamOwner, "identity.member.changed.v1", t1)
		leave := envelopes[len(envelopes)-1]
		expect(channelTok(rj, t1, oldSession), "GET", "/v1/channels/bindings", "", 401, "channel_session_revoked")
		expect(web(rj, t1, "web-rj"), "GET", "/v1/channels/bindings", "", 403, "invalid_tenant_context")
		// 同一主体重新加入
		expect(web(admin, t1, "s"), "PUT", "/v1/identity/members/"+rj, fmt.Sprintf(`{"state":"active","roles":["business_developer"],"expected_version":%d}`, m.MemberVersion+1), 200, "")
		if state, _, _, _ := bindingRow(string(old.ID)); state != "bound" {
			t.Fatalf("前提：退出事件尚未送达，channels 中旧绑定仍为有效：%s", state)
		}
		// 退出前的会话不随重新加入恢复
		expect(channelTok(rj, t1, oldSession), "GET", "/v1/channels/bindings", "", 401, "channel_session_revoked")
		// 旧绑定不能再签发：identity 直接拒绝（不依赖 channels 的事件）
		if _, err := identitySessions.IssueChannelSession(svc(t1, "svc:channels"), &idapi.IssueChannelSessionRequest{CredentialID: "rejoin-credential-" + suffix,
			BindingID: identityapi.ChannelBindingID(old.ID), BindingVersion: 1, Subject: rj, MemberVersion: oldVersion, CredentialExpiresAt: clock.Now().Add(30 * time.Second)}); errCode(err) != "member_rejoined" {
			t.Fatalf("%v", err)
		}
		// 经 channels 免登：转登录页，旧绑定随即以 member_left 失效，不产生会话
		login(sso(corpA, "u-rj"), "binding_inactive")
		if state, reason, _, _ := bindingRow(string(old.ID)); state != "revoking" || reason != "member_left" {
			t.Fatalf("%s %s", state, reason)
		}
		if n := count(iamOwner, `SELECT count(*) FROM identity_channel_sessions WHERE binding_id=$1`, string(old.ID)); n != 1 || liveSessions(string(old.ID)) != 0 {
			t.Fatalf("旧绑定不得签发新会话：%d", n)
		}
		// 重新走 F5：新绑定正常
		renewed := bindAccount(t1, rj, corpA, "u-rj")
		newSession := session(corpA, "u-rj", rj)
		expect(channelTok(rj, t1, newSession), "GET", "/v1/channels/bindings", "", 200, "")
		// 迟到的退出事件：只影响退出前建立的绑定与请求，新绑定与新会话不受影响
		if err := consume(module, env, "channels.member-changes", "identity.member.changed.v1", leave); err != nil {
			t.Fatal(err)
		}
		if state, _, _, _ := bindingRow(string(renewed.ID)); state != "bound" {
			t.Fatalf("迟到的退出事件不得使重新加入后的绑定失效：%s", state)
		}
		expect(channelTok(rj, t1, newSession), "GET", "/v1/channels/bindings", "", 200, "")
		session(corpA, "u-rj", rj)

		// 另一顺序：再次退出并重新加入，期间没有人尝试免登；迟到的退出事件仍使这次退出之前的绑定失效
		m = member()
		expect(web(admin, t1, "s"), "PUT", "/v1/identity/members/"+rj, fmt.Sprintf(`{"state":"removed","roles":[],"expected_version":%d}`, m.MemberVersion), 200, "")
		envelopes = outboxEnvelopes(iamOwner, "identity.member.changed.v1", t1)
		leaveAgain := envelopes[len(envelopes)-1]
		expect(web(admin, t1, "s"), "PUT", "/v1/identity/members/"+rj, fmt.Sprintf(`{"state":"active","roles":["business_developer"],"expected_version":%d}`, m.MemberVersion+1), 200, "")
		expect(channelTok(rj, t1, newSession), "GET", "/v1/channels/bindings", "", 401, "channel_session_revoked")
		if err := consume(module, env, "channels.member-changes", "identity.member.changed.v1", leaveAgain); err != nil {
			t.Fatal(err)
		}
		if state, reason, _, _ := bindingRow(string(renewed.ID)); state != "revoking" || reason != "member_left" {
			t.Fatalf("%s %s", state, reason)
		}
		third := bindAccount(t1, rj, corpA, "u-rj")
		if err := consume(module, env, "channels.member-changes", "identity.member.changed.v1", leave); err != nil {
			t.Fatal(err)
		}
		if state, _, _, _ := bindingRow(string(third.ID)); state != "bound" {
			t.Fatal(state)
		}
		session(corpA, "u-rj", rj)
		advance(36 * time.Second)
		retry()
	})

	run("用例12：退出事件延迟送达、重新加入后未尝试免登：消息回调发出绑定引导、投递改投站内；角色变化不影响；identity 不可用时失败关闭", func(t *testing.T) {
		b := bindAccount(t1, rk, corpA, "u-rk")
		member := func() identityapi.Session {
			var s identityapi.Session
			decode(expect(web(admin, t1, "s"), "GET", "/v1/identity/members/"+rk, "", 200, ""), &s)
			return s
		}
		putMember := func(state, roles string) {
			m := member()
			expect(web(admin, t1, "s"), "PUT", "/v1/identity/members/"+rk, fmt.Sprintf(`{"state":%q,"roles":%s,"expected_version":%d}`, state, roles, m.MemberVersion), 200, "")
		}
		target := func() (*chapi.DeliveryTarget, error) { return messaging.DeliveryTarget(svc(t1, "svc:briefing"), rk) }
		requests := func() int {
			return count(chnOwner, `SELECT count(*) FROM chn_binding_requests WHERE channel_user_id='u-rk'`)
		}
		// 普通的角色变化：成员版本增加，绑定仍有效（两条路径）
		putMember("active", `["decision_maker"]`)
		if res, err := inbound(corpA, "u-rk", clock.Now()); err != nil || res.Outcome != chapi.InboundBound {
			t.Fatalf("%+v %v", res, err)
		}
		if got, err := target(); err != nil || got.Channel != "wecom" || got.BindingID != string(b.ID) {
			t.Fatalf("%+v %v", got, err)
		}
		session(corpA, "u-rk", rk)
		// identity 不可用：失败关闭，不写入任何状态（含去重记录），同一回调恢复后照常处理
		before := requests()
		cb := wc.Sign(&chapi.Callback{CorpID: corpA, EventID: "ev-rk-" + suffix, Timestamp: clock.Now(), Nonce: "n", Body: "message:u-rk"})
		hooked.failLifecycle.Store(true)
		if _, err := callbacks.ReceiveInbound(gw, "wecom", cb); errCode(err) != "identity_unavailable" {
			t.Fatalf("%v", err)
		}
		if _, err := target(); errCode(err) != "identity_unavailable" {
			t.Fatalf("投递不得选择渠道，也不得声称未绑定：%v", err)
		}
		hooked.failLifecycle.Store(false)
		if res, err := callbacks.ReceiveInbound(gw, "wecom", cb); err != nil || res.Outcome != chapi.InboundBound {
			t.Fatalf("失败时不得记下去重：%+v %v", res, err)
		}
		if state, _, _, _ := bindingRow(string(b.ID)); state != "bound" || requests() != before {
			t.Fatalf("%s %d/%d", state, requests(), before)
		}

		// 第一次：退出并重新加入，退出事件不送达、不尝试免登
		putMember("removed", `[]`)
		putMember("active", `["business_developer"]`)
		guides := len(wc.Messages(corpA, "u-rk"))
		hooked.failLifecycle.Store(true)
		if _, err := inbound(corpA, "u-rk", clock.Now()); errCode(err) != "identity_unavailable" {
			t.Fatalf("%v", err)
		}
		if _, err := target(); errCode(err) != "identity_unavailable" {
			t.Fatalf("%v", err)
		}
		hooked.failLifecycle.Store(false)
		if state, _, _, _ := bindingRow(string(b.ID)); state != "bound" || requests() != before || len(wc.Messages(corpA, "u-rk")) != guides {
			t.Fatal("identity 不可用时不得改变状态或发送引导")
		}
		// 消息回调：旧绑定以 member_left 失效，发出绑定引导；重新走 F5
		link := guide(corpA, "u-rk")
		if state, reason, _, _ := bindingRow(string(b.ID)); state != "revoking" || reason != "member_left" {
			t.Fatalf("%s %s", state, reason)
		}
		token := web(rk, t1, "web-rk")
		p := preview(token, link, corpA, "u-rk")
		var renewed chn.BindingConfirmation
		decode(expect(token, "POST", "/v1/channels/bindings:confirm", linkBody(link, oauth(corpA, "u-rk"), string(p.Confirmation)), 200, ""), &renewed)
		if got, err := target(); err != nil || got.Channel != "wecom" || got.BindingID != string(renewed.Binding.ID) {
			t.Fatalf("重新绑定后的新绑定是投递目标：%+v %v", got, err)
		}
		if res, err := inbound(corpA, "u-rk", clock.Now()); err != nil || res.Outcome != chapi.InboundBound {
			t.Fatalf("%+v %v", res, err)
		}
		// 第二次：再次退出并重新加入；投递先发现旧绑定：改投站内，旧绑定失效
		putMember("removed", `[]`)
		putMember("active", `["business_developer"]`)
		if got, err := target(); err != nil || got.Channel != "in_app" || got.Reason != "not_bound" {
			t.Fatalf("投递不得选择退出前的绑定：%+v %v", got, err)
		}
		// 新绑定从未签发凭据，撤销确认后可立即完成：失效中或已失效
		if state, reason, _, _ := bindingRow(string(renewed.Binding.ID)); state == "bound" || reason != "member_left" {
			t.Fatalf("%s %s", state, reason)
		}
		guide(corpA, "u-rk")
		advance(36 * time.Second)
		retry()
	})

	run("用例14：卡片链接不切换账号；无权限、不存在与其他租户返回同一通用结果", func(t *testing.T) {
		var unit identityapi.OrgUnit
		decode(expect(web(admin, t1, "s"), "POST", "/v1/identity/org-units", `{"name":"燃气事业部","parent_id":"root"}`, 200, ""), &unit)
		objective := "objective:" + ids.New("obj")
		if res, err := directory.SyncResourceParticipants(svc(t1, "svc:customer-context"), &idapi.SyncParticipantsRequest{Resource: objective, OrgUnitID: unit.ID,
			Version: 1, Participants: []idapi.ParticipantRelation{{Subject: bd, Relation: "participant"}}}); err != nil || len(res.Rejected) != 0 {
			t.Fatalf("%+v %v", res, err)
		}
		card := func(recipient, object string) string {
			t.Helper()
			l, err := messaging.CreateCardLink(svc(t1, "svc:briefing"), recipient, object)
			if err != nil {
				t.Fatal(err)
			}
			return l.Token
		}
		open := func(token, link string) (int, []byte) {
			return do(token, "POST", "/v1/channels/card-links:open", fmt.Sprintf(`{"token":%q}`, link))
		}
		bdLink := card(bd, objective)
		var target chn.CardTarget
		decode(expect(web(bd, t1, "s"), "POST", "/v1/channels/card-links:open", fmt.Sprintf(`{"token":%q}`, bdLink), 200, ""), &target)
		if target.Object != objective {
			t.Fatalf("%+v", target)
		}
		// 在另一成员的会话中打开：不切换账号，提示链接属于谁
		status, raw := open(web(dm, t1, "s"), bdLink)
		var mismatch chn.Problem
		decode(raw, &mismatch)
		if status != 409 || mismatch.Code.Value != "account_mismatch" || mismatch.Recipient.Value != bd {
			t.Fatalf("%d %s", status, raw)
		}
		// 无权限、对象不存在、链接不存在、其他租户：同一个 404，不含对象信息
		generic := map[string]bool{}
		for _, c := range []struct{ token, link string }{
			{web(ra, t1, "s"), card(ra, "objective:"+ids.New("obj"))},
			{web(ex, t1, "s"), card(ex, objective)},
			{web(bd, t1, "s"), strings.Repeat("A", 43)},
			{web(admin2, t2, "s"), bdLink},
		} {
			status, raw := open(c.token, c.link)
			if status != 404 || codeOf(raw) != "content_unavailable" || strings.Contains(string(raw), "obj_") {
				t.Fatalf("%d %s", status, raw)
			}
			generic[string(raw)] = true
		}
		if len(generic) != 1 {
			t.Fatalf("通用结果必须相同：%v", generic)
		}
		if _, err := messaging.CreateCardLink(svc(t1, "svc:briefing"), bd, "objective:"+objective); errCode(err) != "invalid_request" {
			t.Fatalf("%v", err)
		}
		advance(72 * time.Hour)
		if status, raw := open(web(bd, t1, "s"), bdLink); status != 410 || codeOf(raw) != "card_link_expired" {
			t.Fatalf("%d %s", status, raw)
		}
	})

	run("用例15：渠道网页授权或 IdP 不可用时，免登转登录页并说明原因", func(t *testing.T) {
		b := bindAccount(t1, bd, corpA, "u-bd")
		wc.SetOAuthFailure(true)
		login(sso(corpA, "u-bd"), "channel_oauth_unavailable")
		link := guide(corpA, "u-z")
		if status, raw := prepare(web(ex, t1, "s"), link, "any-code"); status != 503 || codeOf(raw) != "channel_oauth_unavailable" {
			t.Fatalf("%d %s", status, raw)
		}
		wc.SetOAuthFailure(false)
		res, err := callbacks.ChannelSSO(gw, "wecom", "forged-code")
		if err != nil {
			t.Fatal(err)
		}
		login(res, "channel_oauth_invalid")
		hooked.failIssue.Store(true)
		login(sso(corpA, "u-bd"), "identity_unavailable")
		hooked.failIssue.Store(false)
		// 未安装企业的账号
		login(sso("corp-unknown", "u-1"), "channel_not_installed")
		// 恢复后正常；不可用期间没有改变绑定
		session(corpA, "u-bd", bd)
		if state, _, _, _ := bindingRow(string(b.ID)); state != "bound" {
			t.Fatal(state)
		}
		bdBinding = b
	})

	run("用例16：回调重放、验签失败、超出时间窗不改变状态", func(t *testing.T) {
		requests := func() int { return count(chnOwner, `SELECT count(*) FROM chn_binding_requests`) }
		rejected := func() int {
			return installations(web(admin, t1, "s"))[0].RejectedCallbacks.Value
		}
		before, rejectedBefore, msgs := requests(), rejected(), len(wc.Messages(corpA, "u-new"))
		forged := &chapi.Callback{CorpID: corpA, EventID: "ev-forged", Timestamp: clock.Now(), Nonce: "n", Body: "message:u-new", Signature: strings.Repeat("0", 64)}
		if _, err := callbacks.ReceiveInbound(gw, "wecom", forged); errCode(err) != "callback_signature_invalid" {
			t.Fatalf("%v", err)
		}
		// 签名后篡改内容
		tampered := wc.Sign(&chapi.Callback{CorpID: corpA, EventID: "ev-tampered", Timestamp: clock.Now(), Nonce: "n", Body: "message:u-new"})
		tampered.Body = "message:u-admin"
		if _, err := callbacks.ReceiveInbound(gw, "wecom", tampered); errCode(err) != "callback_signature_invalid" {
			t.Fatalf("%v", err)
		}
		// 超出时间窗
		if _, err := callbacks.ReceiveInbound(gw, "wecom", wc.Sign(&chapi.Callback{CorpID: corpA, EventID: "ev-old", Timestamp: clock.Now().Add(-6 * time.Minute), Nonce: "n", Body: "message:u-new"})); errCode(err) != "callback_replayed" {
			t.Fatalf("%v", err)
		}
		if requests() != before || len(wc.Messages(corpA, "u-new")) != msgs {
			t.Fatal("被拒绝的回调不得改变状态")
		}
		// 重放：同一事件第二次到达
		cb := wc.Sign(&chapi.Callback{CorpID: corpA, EventID: "ev-once", Timestamp: clock.Now(), Nonce: "n", Body: "message:u-new"})
		if res, err := callbacks.ReceiveInbound(gw, "wecom", cb); err != nil || res.Outcome != chapi.InboundGuideSent {
			t.Fatalf("%+v %v", res, err)
		}
		state := requests()
		if res, err := callbacks.ReceiveInbound(gw, "wecom", cb); err != nil || res.Outcome != chapi.InboundDuplicate {
			t.Fatalf("%+v %v", res, err)
		}
		if requests() != state || len(wc.Messages(corpA, "u-new")) != msgs+1 {
			t.Fatal("重放不得改变状态或再次发送")
		}
		if got := rejected(); got != rejectedBefore+4 {
			t.Fatalf("渠道健康计数：%d → %d", rejectedBefore, got)
		}
	})

	run("用例13：渠道停用后全部绑定失效，消息改投站内", func(t *testing.T) {
		target, err := messaging.DeliveryTarget(svc(t1, "svc:briefing"), bd)
		if err != nil || target.Channel != "wecom" || target.BindingID != string(bdBinding.ID) {
			t.Fatalf("%+v %v", target, err)
		}
		if target, err := messaging.DeliveryTarget(svc(t1, "svc:briefing"), ra); err != nil || target.Channel != "in_app" || target.Reason != "not_bound" {
			t.Fatalf("%+v %v", target, err)
		}
		chs := session(corpA, "u-bd", bd)
		current := installations(web(admin, t1, "s"))[0]
		body := fmt.Sprintf(`{"expected_version":%d}`, current.Version)
		path := "/v1/channels/installations/" + string(current.ID) + "/disable"
		expect(web(bd, t1, "s"), "POST", path, body, 403, "forbidden")
		expect(stale(admin, t1), "POST", path, body, 403, "mfa_required")
		expect(web(admin, t1, "s"), "POST", path, `{"expected_version":99}`, 409, "version_conflict")
		var disabled chn.Installation
		decode(expect(web(admin, t1, "s"), "POST", path, body, 200, ""), &disabled)
		if disabled.State != "disabled" || disabled.BoundCount != 0 {
			t.Fatalf("%+v", disabled)
		}
		if n := count(chnOwner, `SELECT count(*) FROM chn_bindings WHERE tenant_id=$1 AND state='bound'`, t1); n != 0 {
			t.Fatalf("%d", n)
		}
		if n := count(chnOwner, `SELECT count(*) FROM chn_bindings WHERE tenant_id=$1 AND reason='channel_disabled'`, t1); n < 3 {
			t.Fatalf("%d", n)
		}
		expect(channelTok(bd, t1, chs), "GET", "/v1/channels/bindings", "", 401, "channel_session_revoked")
		for _, who := range []string{bd, dm, admin} {
			if target, err := messaging.DeliveryTarget(svc(t1, "svc:briefing"), who); err != nil || target.Channel != "in_app" || target.Reason != "channel_not_enabled" {
				t.Fatalf("%s：%+v %v", who, target, err)
			}
		}
		login(sso(corpA, "u-bd"), "channel_not_installed")
		if res, err := inbound(corpA, "u-bd", clock.Now()); err != nil || res.Outcome != chapi.InboundIgnored {
			t.Fatalf("%+v %v", res, err)
		}
		// 重复停用（同一幂等键之外）：版本已变
		expect(web(admin, t1, "s"), "POST", path, body, 409, "version_conflict")
		advance(36 * time.Second)
		retry()
		if n := count(chnOwner, `SELECT count(*) FROM chn_bindings WHERE tenant_id=$1 AND state='revoking'`, t1); n != 0 {
			t.Fatalf("%d", n)
		}
		// 乙租户不受影响
		if list := installations(web(admin2, t2, "s")); len(list) != 1 || list[0].State != "pending_verification" {
			t.Fatalf("%+v", list)
		}
	})

	run("持久化的 channels 事件符合契约", func(t *testing.T) {
		rows, err := chnOwner.Query(ctx, `SELECT envelope FROM kernel_outbox WHERE envelope->>'type' LIKE 'channels.%'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var raw []byte
			var envelope outbox.Envelope
			if err := rows.Scan(&raw); err != nil || json.Unmarshal(raw, &envelope) != nil {
				t.Fatal(err)
			}
			if err := envelope.CheckTopic("cell-cell-01.channels"); err != nil {
				t.Fatalf("%v：%s", err, raw)
			}
			schema, err := events.Schema(envelope.Type)
			if err != nil {
				t.Fatal(err)
			}
			if err := events.Validate(schema, envelope.Data); err != nil {
				t.Fatalf("%s：%v：%s", envelope.Type, err, envelope.Data)
			}
			n++
		}
		if rows.Err() != nil || n < 20 {
			t.Fatalf("事件 %d：%v", n, rows.Err())
		}
	})
}

// unavailableBriefing 与 unavailableContext 是失败关闭的替身：一律返回暂时性错误。
type unavailableBriefing struct{}

func (unavailableBriefing) BriefingSignals(context.Context, string, time.Time, time.Time) (*detectionapi.BriefingSignalPage, error) {
	return nil, binding.ErrTransient
}
func (unavailableBriefing) SignalCards(context.Context, []string) (*detectionapi.SignalCardList, error) {
	return nil, binding.ErrTransient
}
func (unavailableBriefing) Triage(context.Context, string, string, *detectionapi.TriageRequest) (*detectionapi.TriageResult, error) {
	return nil, binding.ErrTransient
}
func (unavailableBriefing) Undo(context.Context, string, *detectionapi.UndoRequest) (*detectionapi.UndoResult, error) {
	return nil, binding.ErrTransient
}

type unavailableContext struct{}

func (unavailableContext) Access(context.Context, string, []string) ([]string, error) {
	return nil, binding.ErrTransient
}
func (unavailableContext) ActivePolicies(context.Context, int, string) (*ccapi.ActivePolicyList, error) {
	return nil, binding.ErrTransient
}
