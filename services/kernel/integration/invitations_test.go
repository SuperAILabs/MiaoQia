package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/contracts/events"
	"github.com/superailabs/miaoqia/services/internal/testissuer"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	idapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api/idptest"
)

// testClock 是 identity 的业务时钟：缺省跟随系统时钟加偏移，可冻结在某一时刻（强认证边界与邀请过期）。
type testClock struct {
	mu     sync.Mutex
	fixed  time.Time
	offset time.Duration
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fixed.IsZero() {
		return c.fixed
	}
	return time.Now().Add(c.offset)
}

func (c *testClock) set(fixed time.Time, offset time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fixed, c.offset = fixed, offset
}

// 邀请（F2）与强认证时效（F3）：PRD 第1段第12节用例 1–7 的服务端部分。真实 PostgreSQL + OpenFGA + 网关令牌校验；
// IdP 账号与邮件渠道使用内存端口（idptest），强认证时间由测试签发方的已签名 auth_time 给出，不模拟真实 TOTP 仪式。
func TestInvitationsAndStrongAuth(t *testing.T) {
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

	idp, mail, clock := idptest.NewIdP(), &idptest.Mailbox{}, &testClock{}
	iam := testenv.NewDatabase(t)
	module := identity.ModuleWith(identity.Adapters{IdP: idp, Notifier: mail, Now: clock.Now})
	if err := db.Migrate(ctx, iam.OwnerDSN, iam.Name+"_app", module.Migrations); err != nil {
		t.Fatal(err)
	}
	t1, t2 := ids.New("tnt"), ids.New("tnt")
	suffix := strings.ToLower(iam.Name)
	user := func(name string) string { return name + "-" + suffix }
	admin, bd, dm, admin2 := user("admin"), user("bd"), user("dm"), user("admin2")
	owner, err := pgx.Connect(ctx, iam.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO identity_tenants VALUES ($1,'甲','cell-01','active',1), ($2,'乙','cell-01','active',1)`, []any{t1, t2}},
		{`INSERT INTO identity_members VALUES ($1,$2,'active',ARRAY['admin'],1), ($1,$3,'active',ARRAY['business_developer'],1),
 ($1,$4,'active',ARRAY['decision_maker'],1), ($5,$6,'active',ARRAY['admin'],1)`, []any{t1, admin, bd, dm, t2, admin2}},
	} {
		if _, err := owner.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []idapi.Account{
		{Subject: admin, Email: admin + "@example.com", EmailVerified: true, TOTPEnrolled: true},
		{Subject: bd, Email: bd + "@example.com", EmailVerified: true, TOTPEnrolled: false},
		{Subject: dm, Email: dm + "@example.com", EmailVerified: true, TOTPEnrolled: true},
		{Subject: admin2, Email: admin2 + "@example.com", EmailVerified: true, TOTPEnrolled: true},
	} {
		idp.Put(a)
	}
	app, err := db.Open(ctx, iam.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	bindings := runtime.NewBindings()
	env := &runtime.Env{DB: app, Bindings: bindings, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Config: func(string) string { return fga }}
	if err := module.Start(ctx, env); err != nil {
		t.Fatal(err)
	}
	public, internal := http.NewServeMux(), http.NewServeMux()
	module.Routes(env, public, internal)
	issuer := testissuer.New(t)
	resolver, _ := runtime.Lookup[idapi.SessionResolver](bindings, idapi.SessionBinding)
	guard, ok := runtime.Lookup[idapi.Guard](bindings, idapi.GuardBinding)
	if !ok {
		t.Fatal("identity 未提供强认证门禁")
	}
	maintenance, _ := runtime.Lookup[idapi.Maintenance](bindings, idapi.MaintenanceBinding)
	secured, err := gateway.Public(ctx, issuer.Server.URL, "miaoqia-api", public, resolver)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(secured)
	defer server.Close()
	client := server.Client()

	// 辅助函数在当前子测试上报告失败（run 设置 tt）
	tt := t
	run := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			prev := tt
			tt = t
			defer func() { tt = prev }()
			f(t)
		})
	}
	// 令牌：fresh 为刚完成的带第二因素的交互登录（auth_time 取 identity 时钟）；plain 为只有密码；tok 自定声明
	cellOf := func(tenant string) string {
		if tenant == "" {
			return ""
		}
		return "cell-01"
	}
	tok := func(subject, tenant string, claims map[string]any) string {
		return issuer.TokenWith(tt, subject, tenant, cellOf(tenant), claims)
	}
	authAt := func(at time.Time) map[string]any {
		return map[string]any{"amr": []string{"pwd", "mfa"}, "auth_time": at.Unix()}
	}
	fresh := func(subject, tenant string) string { return tok(subject, tenant, authAt(clock.Now())) }
	plain := func(subject, tenant string) string { return tok(subject, tenant, nil) }
	var keys atomic.Int64
	do := func(token, method, path, body string, key ...string) (int, []byte) {
		tt.Helper()
		r, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		k := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(method, path, body, keys.Add(1)))))[:40]
		if len(key) > 0 {
			k = key[0]
		}
		r.Header.Set("Idempotency-Key", k)
		resp, err := client.Do(r)
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
	expect := func(token, method, path, body string, status int, code string, key ...string) []byte {
		tt.Helper()
		got, raw := do(token, method, path, body, key...)
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
	count := func(q string, args ...any) int {
		tt.Helper()
		var n int
		if err := owner.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			tt.Fatal(err)
		}
		return n
	}
	invitationEvents := func(id, change string) int {
		return count(`SELECT count(*) FROM kernel_outbox WHERE envelope->>'type'='identity.invitation.changed.v1' AND envelope->'data'->>'invitation_id'=$1 AND envelope->'data'->>'change'=$2`, id, change)
	}
	memberEvents := func(tenant, subject string) int {
		return count(`SELECT count(*) FROM kernel_outbox WHERE envelope->>'type'='identity.member.changed.v1' AND envelope->'data'->>'tenant_id'=$1 AND envelope->'data'->>'subject'=$2`, tenant, subject)
	}
	memberRow := func(tenant, subject string) (string, []string, int) {
		tt.Helper()
		var state string
		var roles []string
		var units int
		err := owner.QueryRow(ctx, `SELECT state, roles, (SELECT count(*) FROM identity_member_unit_roles u WHERE u.tenant_id=m.tenant_id AND u.subject=m.subject)
 FROM identity_members m WHERE tenant_id=$1 AND subject=$2`, tenant, subject).Scan(&state, &roles, &units)
		if err == pgx.ErrNoRows {
			return "", nil, 0
		}
		if err != nil {
			tt.Fatal(err)
		}
		return state, roles, units
	}
	as := func(tenant, principal string, a reqctx.Assurance) context.Context {
		return reqctx.WithAssurance(reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: "cell-01", Principal: reqctx.Principal{ID: principal, Kind: "user"}}), a)
	}
	guardCode := func(err error) string {
		if err == nil {
			return ""
		}
		if e, ok := err.(*idapi.Error); ok {
			return e.Code
		}
		return err.Error()
	}

	const invitations = "/v1/identity/invitations"
	type issued = identityapi.InvitationIssued
	invite := func(token, body string, key ...string) issued {
		tt.Helper()
		var out issued
		decode(expect(token, "POST", invitations, body, 200, "", key...), &out)
		return out
	}
	accept := func(token, link string) (int, []byte) {
		return do(token, "POST", invitations+":accept", `{"token":"`+link+`"}`)
	}
	newcomer := func(name string, enrolled bool) (string, string) {
		subject := user(name)
		email := name + "-" + suffix + "@example.com"
		idp.Put(idapi.Account{Subject: subject, Email: email, EmailVerified: true, TOTPEnrolled: enrolled})
		return subject, email
	}
	var gas identityapi.OrgUnit
	decode(expect(fresh(admin, t1), "POST", "/v1/identity/org-units", `{"name":"燃气事业部","parent_id":"root"}`, 200, ""), &gas)

	run("用例5：非管理员或没有15分钟内强认证时邀请、角色变更与渠道安装被拒绝", func(t *testing.T) {
		body := `{"email":"x5@example.com","name":"甲","roles":["business_developer"]}`
		// 非管理员：即使刚完成强认证也被拒绝（成员管理接口与邀请接口相同）
		expect(fresh(bd, t1), "POST", invitations, body, 403, "forbidden")
		expect(fresh(bd, t1), "GET", invitations, "", 403, "forbidden")
		expect(fresh(bd, t1), "PUT", "/v1/identity/members/"+bd, `{"state":"active","roles":["admin"],"expected_version":1}`, 403, "forbidden")
		// 管理员没有强认证、或强认证已超过15分钟
		stale := tok(admin, t1, authAt(clock.Now().Add(-16*time.Minute)))
		for _, token := range []string{plain(admin, t1), stale} {
			expect(token, "POST", invitations, body, 403, "mfa_required")
			expect(token, "GET", invitations, "", 403, "mfa_required")
			expect(token, "PUT", "/v1/identity/members/"+bd, `{"state":"active","roles":["decision_maker"],"expected_version":1}`, 403, "mfa_required")
			expect(token, "POST", "/v1/identity/org-units", `{"name":"水务","parent_id":"root"}`, 403, "mfa_required")
			expect(token, "PUT", "/v1/identity/org-units/"+string(gas.ID), `{"name":"燃气","parent_id":"root","expected_version":1}`, 403, "mfa_required")
		}
		if state, roles, _ := memberRow(t1, bd); state != "active" || !slices.Equal(roles, []string{"business_developer"}) {
			t.Fatalf("被拒绝的角色变更写入了目录：%s %v", state, roles)
		}
		// 登记状态未知（IdP 不可用）时只要求重新强认证，不当作“未登记”，也不放行
		idp.SetFailures(true, false)
		expect(plain(admin, t1), "POST", invitations, body, 403, "mfa_required")
		idp.SetFailures(false, false)
		// 渠道安装的占位：渠道子系统经 identity.Guard 的 RequireAdmin 判断
		now := clock.Now()
		for _, c := range []struct {
			name, who string
			a         reqctx.Assurance
			code      string
		}{
			{"非管理员", bd, reqctx.Assurance{MFA: true, AuthTime: now, Source: reqctx.SourceIdP}, "forbidden"},
			{"管理员无强认证", admin, reqctx.Assurance{Source: reqctx.SourceIdP}, "mfa_required"},
			{"管理员强认证过期", admin, reqctx.Assurance{MFA: true, AuthTime: now.Add(-16 * time.Minute), Source: reqctx.SourceIdP}, "mfa_required"},
			{"管理员免登会话", admin, reqctx.Assurance{MFA: true, AuthTime: now, Source: reqctx.SourceChannel}, "mfa_required"},
			{"其他租户的管理员", admin2, reqctx.Assurance{MFA: true, AuthTime: now, Source: reqctx.SourceIdP}, "invalid_tenant_context"},
			{"管理员15分钟内", admin, reqctx.Assurance{MFA: true, AuthTime: now.Add(-time.Minute), Source: reqctx.SourceIdP}, ""},
		} {
			if got := guardCode(guard.RequireAdmin(as(t1, c.who, c.a))); got != c.code {
				t.Fatalf("渠道安装门禁（%s）：得到 %q，期望 %q", c.name, got, c.code)
			}
		}
		// 管理员15分钟内强认证：邀请成功
		out := invite(fresh(admin, t1), body)
		if out.Outcome != "created" || out.Invitation.State != "pending" || !out.LinkToken.Set {
			t.Fatalf("%+v", out)
		}
	})

	run("用例6：强认证时间边界、刷新不延长、免登会话须重新验证", func(t *testing.T) {
		at := time.Now().Truncate(time.Second)
		clock.set(at, 0)
		defer clock.set(time.Time{}, 0)
		org := func(token, name string, status int, code string) {
			t.Helper()
			expect(token, "POST", "/v1/identity/org-units", `{"name":"`+name+`","parent_id":"root"}`, status, code)
		}
		org(tok(admin, t1, authAt(at.Add(-14*time.Minute-59*time.Second))), "边界内", 200, "")
		org(tok(admin, t1, authAt(at.Add(-15*time.Minute-time.Second))), "边界外", 403, "mfa_required")
		invite(tok(admin, t1, authAt(at.Add(-14*time.Minute-59*time.Second))), `{"email":"x6@example.com","name":"乙","roles":["decision_maker"]}`)
		expect(tok(admin, t1, authAt(at.Add(-15*time.Minute-time.Second))), "POST", invitations, `{"email":"x6b@example.com","name":"乙","roles":["decision_maker"]}`, 403, "mfa_required")
		expect(tok(admin, t1, authAt(at.Add(-15*time.Minute-time.Second))), "PUT", "/v1/identity/members/"+dm, `{"state":"active","roles":["research_analyst"],"expected_version":1}`, 403, "mfa_required")
		// 刷新得到的令牌：签发时间是现在，auth_time 仍是原登录时间，不延长窗口
		org(tok(admin, t1, authAt(at.Add(-20*time.Minute))), "刷新", 403, "mfa_required")
		// 只有 amr=mfa 不能证明新鲜度
		org(tok(admin, t1, map[string]any{"amr": []string{"mfa"}}), "无认证时间", 403, "mfa_required")
		// 渠道免登会话：即使声明了 amr 与新的 auth_time 也须重新完成 TOTP；未知会话来源同样不满足
		for _, source := range []string{"channel", "password"} {
			claims := authAt(at)
			claims["miaoqia_session_source"] = source
			org(tok(admin, t1, claims), "免登"+source, 403, "mfa_required")
		}
		claims := map[string]any{"miaoqia_session_source": "channel", "auth_time": at.Unix()}
		org(tok(admin, t1, claims), "免登无MFA", 403, "mfa_required")
		// auth_time 晚于 identity 时钟超过容许偏差：不可信
		org(tok(admin, t1, authAt(at.Add(time.Minute))), "未来", 403, "mfa_required")
	})

	run("用例7：首次强认证动作前须登记TOTP，取消登记则原动作不执行", func(t *testing.T) {
		// 业务开拓者提交策略激活申请（pursuit 随后实现）：经 identity.Guard 的 RequireStrongAuth 判断，不满足时调用方不执行原动作
		executed := 0
		submit := func(a reqctx.Assurance) string {
			err := guard.RequireStrongAuth(as(t1, bd, a))
			if err == nil {
				executed++
			}
			return guardCode(err)
		}
		if got := submit(reqctx.Assurance{Source: reqctx.SourceIdP}); got != "totp_enrollment_required" || executed != 0 {
			t.Fatalf("未登记 TOTP：%q，执行 %d 次", got, executed)
		}
		// 取消登记后再次提交：仍要求登记，原动作仍未执行
		if got := submit(reqctx.Assurance{Source: reqctx.SourceIdP}); got != "totp_enrollment_required" || executed != 0 {
			t.Fatalf("取消登记后：%q，执行 %d 次", got, executed)
		}
		// 免登会话中的未登记成员同样先登记
		if got := submit(reqctx.Assurance{Source: reqctx.SourceChannel}); got != "totp_enrollment_required" || executed != 0 {
			t.Fatalf("免登会话：%q", got)
		}
		// 登记完成即视为本次强认证：新令牌带 amr=mfa 与刚才的 auth_time
		idp.Put(idapi.Account{Subject: bd, Email: bd + "@example.com", EmailVerified: true, TOTPEnrolled: true})
		if got := submit(reqctx.Assurance{Source: reqctx.SourceIdP}); got != "mfa_required" || executed != 0 {
			t.Fatalf("已登记但未验证：%q", got)
		}
		if got := submit(reqctx.Assurance{MFA: true, AuthTime: clock.Now(), Source: reqctx.SourceIdP}); got != "" || executed != 1 {
			t.Fatalf("登记并验证后：%q，执行 %d 次", got, executed)
		}
		// 非本租户有效成员不能借门禁执行
		if got := guardCode(guard.RequireStrongAuth(as(t2, bd, reqctx.Assurance{MFA: true, AuthTime: clock.Now(), Source: reqctx.SourceIdP}))); got != "invalid_tenant_context" {
			t.Fatalf("跨租户：%q", got)
		}
		// 管理员同样：未登记 TOTP 时成员管理接口返回登记要求，且不写入
		idp.Put(idapi.Account{Subject: dm, Email: dm + "@example.com", EmailVerified: true, TOTPEnrolled: false})
		expect(fresh(admin, t1), "PUT", "/v1/identity/members/"+dm, `{"state":"active","roles":["admin","decision_maker"],"expected_version":1}`, 200, "")
		expect(plain(dm, t1), "POST", invitations, `{"email":"x7@example.com","name":"丙","roles":["business_developer"]}`, 403, "totp_enrollment_required")
		if n := count(`SELECT count(*) FROM identity_invitations WHERE email='x7@example.com'`); n != 0 {
			t.Fatal("未登记 TOTP 的管理员创建了邀请")
		}
	})

	run("用例1：重复接受只写入一次、通知一次；响应丢失后重试返回已接受", func(t *testing.T) {
		subject, email := newcomer("new1", false)
		out := invite(fresh(admin, t1), `{"email":"`+strings.ToUpper(email)+`","name":"新销售","roles":[],"unit_roles":[{"org_unit_id":"`+string(gas.ID)+`","roles":["business_developer"]}]}`)
		link := out.LinkToken.Value
		if out.Invitation.Email != email || out.Invitation.Delivery != "sent" {
			t.Fatalf("%+v", out.Invitation)
		}
		if msgs := mail.Sent(email); len(msgs) != 1 || msgs[0].Token != link || msgs[0].TenantName != "甲" {
			t.Fatalf("邀请邮件：%+v", msgs)
		}
		// 接受前令牌不带租户；带上租户也不能访问（尚不是成员）
		expect(plain(subject, t1), "GET", "/v1/identity/session", "", 403, "invalid_tenant_context")
		code, raw := accept(plain(subject, ""), link)
		var first identityapi.InvitationAcceptance
		decode(raw, &first)
		if code != 200 || first.Outcome != "accepted" || first.Access != "ready" || first.Tenant != t1 || len(first.Roles) != 0 ||
			len(first.UnitRoles) != 1 || first.UnitRoles[0].OrgUnitID != gas.ID {
			t.Fatalf("%d %s", code, raw)
		}
		// 首次响应丢失后的重试与并发重复：都返回首次结果，不再写入或发布
		var wg sync.WaitGroup
		results := make(chan identityapi.InvitationAcceptance, 3)
		for range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, raw := accept(plain(subject, ""), link)
				var again identityapi.InvitationAcceptance
				if code != 200 || json.Unmarshal(raw, &again) != nil {
					t.Errorf("重复接受：%d %s", code, raw)
					return
				}
				results <- again
			}()
		}
		wg.Wait()
		close(results)
		n := 0
		for again := range results {
			n++
			if again.Outcome != "already_accepted" || again.MemberVersion != first.MemberVersion || !again.AcceptedAt.Equal(first.AcceptedAt) || again.Access != "ready" {
				t.Fatalf("重复接受未返回首次结果：%+v", again)
			}
		}
		if n != 3 || memberEvents(t1, subject) != 1 || invitationEvents(string(first.InvitationID), "accepted") != 1 {
			t.Fatalf("重复接受产生了多次写入或通知：成员事件 %d，接受事件 %d", memberEvents(t1, subject), invitationEvents(string(first.InvitationID), "accepted"))
		}
		bound := 0
		for _, b := range idp.Bindings() {
			if b.Subject == subject {
				bound++
				if b.Tenant != t1 || b.Cell != "cell-01" {
					t.Fatalf("%+v", b)
				}
			}
		}
		if bound != 1 {
			t.Fatalf("IdP 租户归属写入 %d 次", bound)
		}
		// 接受后令牌带租户：会话按邀请限定的单元角色
		var s idapi.Session
		decode(expect(plain(subject, t1), "GET", "/v1/identity/session", "", 200, ""), &s)
		if len(s.Roles) != 0 || len(s.UnitRoles) != 1 || s.UnitRoles[0].OrgUnitID != gas.ID {
			t.Fatalf("%+v", s)
		}
		// 已接受的邀请对其他账号：拒绝
		other, _ := newcomer("new1b", false)
		code, raw = accept(plain(other, ""), link)
		if code != 409 || codeOf(raw) != "invitation_already_accepted" {
			t.Fatalf("%d %s", code, raw)
		}
	})

	run("用例1：首次接受并发且IdP归属写入失败时，只写入一次并可重试", func(t *testing.T) {
		subject, email := newcomer("new1c", false)
		out := invite(fresh(admin, t1), `{"email":"`+email+`","name":"并发","roles":["business_developer"]}`)
		link := out.LinkToken.Value
		idp.SetFailures(false, true)
		release := lockInvitationRow(t, ctx, iam, string(out.Invitation.ID))
		type answer struct {
			code int
			raw  []byte
		}
		answers := make(chan answer, 2)
		for range 2 {
			go func() {
				code, raw := accept(plain(subject, ""), link)
				answers <- answer{code, raw}
			}()
		}
		waitForLockWaiters(t, ctx, iam, 2)
		release()
		outcomes := map[string]int{}
		for range 2 {
			a := <-answers
			var r identityapi.InvitationAcceptance
			if a.code != 200 || json.Unmarshal(a.raw, &r) != nil {
				t.Fatalf("%d %s", a.code, a.raw)
			}
			outcomes[string(r.Outcome)]++
			if r.Access != "provisioning" {
				t.Fatalf("IdP 写入失败时不能显示已开通：%+v", r)
			}
		}
		if outcomes["accepted"] != 1 || outcomes["already_accepted"] != 1 || memberEvents(t1, subject) != 1 || invitationEvents(string(out.Invitation.ID), "accepted") != 1 {
			t.Fatalf("并发接受：%v，成员事件 %d", outcomes, memberEvents(t1, subject))
		}
		// IdP 恢复后：定时重试写入归属；之后重复接受返回 ready
		idp.SetFailures(false, false)
		svc := reqctx.With(ctx, reqctx.Context{Principal: reqctx.Principal{ID: "svc:identity", Kind: "service"}})
		if n, err := maintenance.RetryTenantBindings(svc, 100); err != nil || n != 1 {
			t.Fatalf("重试写入归属：%d %v", n, err)
		}
		code, raw := accept(plain(subject, ""), link)
		var again identityapi.InvitationAcceptance
		decode(raw, &again)
		if code != 200 || again.Outcome != "already_accepted" || again.Access != "ready" {
			t.Fatalf("%d %s", code, raw)
		}
		if n, _ := maintenance.RetryTenantBindings(svc, 100); n != 0 {
			t.Fatal("已写入的归属被重复重试")
		}
		// 归属写入失败后成员已退出：重试取消该写入，不把已退出成员的租户写入 IdP
		gone, goneEmail := newcomer("gone1", false)
		out = invite(fresh(admin, t1), `{"email":"`+goneEmail+`","name":"退出","roles":["business_developer"]}`)
		idp.SetFailures(false, true)
		code, raw = accept(plain(gone, ""), out.LinkToken.Value)
		var joined identityapi.InvitationAcceptance
		decode(raw, &joined)
		idp.SetFailures(false, false)
		if code != 200 || joined.Access != "provisioning" {
			t.Fatalf("%d %s", code, raw)
		}
		expect(fresh(admin, t1), "PUT", "/v1/identity/members/"+gone, fmt.Sprintf(`{"state":"removed","roles":[],"expected_version":%d}`, joined.MemberVersion), 200, "")
		if n, err := maintenance.RetryTenantBindings(svc, 100); err != nil || n != 0 {
			t.Fatalf("%d %v", n, err)
		}
		for _, b := range idp.Bindings() {
			if b.Subject == gone {
				t.Fatal("已退出成员的租户归属被写入 IdP")
			}
		}
		if count(`SELECT count(*) FROM identity_tenant_bindings WHERE subject=$1 AND state='cancelled'`, gone) != 1 {
			t.Fatal("归属写入未取消")
		}
		code, raw = accept(plain(gone, ""), out.LinkToken.Value)
		decode(raw, &joined)
		if code != 200 || joined.Outcome != "already_accepted" || joined.Access != "provisioning" {
			t.Fatalf("已退出成员的重复接受不能显示已开通：%d %s", code, raw)
		}
	})

	run("用例2：撤回、过期、重发后的旧链接不可用", func(t *testing.T) {
		// 撤回
		subject, email := newcomer("new2", false)
		out := invite(fresh(admin, t1), `{"email":"`+email+`","name":"撤回","roles":["business_developer"]}`)
		id := string(out.Invitation.ID)
		var revoked identityapi.Invitation
		decode(expect(fresh(admin, t1), "POST", invitations+"/"+id+"/revoke", `{"expected_version":1}`, 200, ""), &revoked)
		if revoked.State != "revoked" || revoked.Version != 2 {
			t.Fatalf("%+v", revoked)
		}
		code, raw := accept(plain(subject, ""), out.LinkToken.Value)
		if code != 410 || codeOf(raw) != "invitation_revoked" {
			t.Fatalf("%d %s", code, raw)
		}
		expect(fresh(admin, t1), "POST", invitations+"/"+id+"/revoke", `{"expected_version":2}`, 409, "invitation_not_pending")
		expect(fresh(admin, t1), "POST", invitations+"/"+id+"/resend", `{"expected_version":2}`, 409, "invitation_not_pending")
		if s, _, _ := memberRow(t1, subject); s != "" || invitationEvents(id, "revoked") != 1 {
			t.Fatal("撤回的邀请写入了成员或事件次数不对")
		}
		// 重发：旧链接失效，新链接可用
		subject, email = newcomer("new2b", false)
		out = invite(fresh(admin, t1), `{"email":"`+email+`","name":"重发","roles":["decision_maker"]}`)
		id = string(out.Invitation.ID)
		var resent issued
		decode(expect(fresh(admin, t1), "POST", invitations+"/"+id+"/resend", `{"expected_version":1}`, 200, ""), &resent)
		if resent.Outcome != "resent" || resent.Invitation.Version != 2 || resent.LinkToken.Value == out.LinkToken.Value || len(mail.Sent(email)) != 2 {
			t.Fatalf("%+v", resent)
		}
		code, raw = accept(plain(subject, ""), out.LinkToken.Value)
		if code != 410 || codeOf(raw) != "invitation_link_superseded" {
			t.Fatalf("%d %s", code, raw)
		}
		if code, raw = accept(plain(subject, ""), resent.LinkToken.Value); code != 200 {
			t.Fatalf("%d %s", code, raw)
		}
		// 过期：业务时钟越过7天；接受返回已过期，列表与事件按过期时间记录；重发后恢复
		subject, email = newcomer("new2c", false)
		out = invite(fresh(admin, t1), `{"email":"`+email+`","name":"过期","roles":["business_developer"]}`)
		id = string(out.Invitation.ID)
		clock.set(time.Time{}, 7*24*time.Hour+time.Second)
		defer clock.set(time.Time{}, 0)
		code, raw = accept(plain(subject, ""), out.LinkToken.Value)
		if code != 410 || codeOf(raw) != "invitation_expired" {
			t.Fatalf("%d %s", code, raw)
		}
		var list identityapi.InvitationList
		decode(expect(fresh(admin, t1), "GET", invitations+"?state=expired", "", 200, ""), &list)
		var expired *identityapi.Invitation
		for i := range list.Invitations {
			if string(list.Invitations[i].ID) == id {
				expired = &list.Invitations[i]
			}
		}
		if expired == nil || expired.State != "expired" || expired.Version != 2 || invitationEvents(id, "expired") != 1 {
			t.Fatalf("过期未记录：%+v", expired)
		}
		var effective string
		if err := owner.QueryRow(ctx, `SELECT envelope->'data'->>'effective_at' FROM kernel_outbox WHERE envelope->>'type'='identity.invitation.changed.v1' AND envelope->'data'->>'invitation_id'=$1 AND envelope->'data'->>'change'='expired'`, id).Scan(&effective); err != nil {
			t.Fatal(err)
		}
		if at, err := time.Parse(time.RFC3339Nano, effective); err != nil || !at.Equal(out.Invitation.ExpiresAt) {
			t.Fatalf("过期事件的生效时间应为邀请的过期时间：%s vs %s", effective, out.Invitation.ExpiresAt)
		}
		decode(expect(fresh(admin, t1), "POST", invitations+"/"+id+"/resend", `{"expected_version":2}`, 200, ""), &resent)
		if resent.Invitation.State != "pending" || !resent.Invitation.ExpiresAt.After(out.Invitation.ExpiresAt) {
			t.Fatalf("%+v", resent.Invitation)
		}
		if code, raw = accept(plain(subject, ""), out.LinkToken.Value); code != 410 || codeOf(raw) != "invitation_link_superseded" {
			t.Fatalf("%d %s", code, raw)
		}
		if code, raw = accept(plain(subject, ""), resent.LinkToken.Value); code != 200 {
			t.Fatalf("%d %s", code, raw)
		}
		// 定时任务处理到期邀请：只处理一次
		_, email = newcomer("new2d", false)
		out = invite(fresh(admin, t1), `{"email":"`+email+`","name":"定时","roles":["business_developer"]}`)
		clock.set(time.Time{}, 2*(7*24*time.Hour+time.Second))
		svc := reqctx.With(ctx, reqctx.Context{Principal: reqctx.Principal{ID: "svc:identity", Kind: "service"}})
		if n, err := maintenance.ExpireDueInvitations(svc, 100); err != nil || n < 1 {
			t.Fatalf("定时过期：%d %v", n, err)
		}
		if n, err := maintenance.ExpireDueInvitations(svc, 100); err != nil || n != 0 || invitationEvents(string(out.Invitation.ID), "expired") != 1 {
			t.Fatalf("重复过期：%d %v", n, err)
		}
		// 接受请求不能借请求体扩大角色或单元
		code, raw = do(plain(subject, ""), "POST", invitations+":accept", `{"token":"`+resent.LinkToken.Value+`","roles":["admin"]}`)
		if code != 400 {
			t.Fatalf("%d %s", code, raw)
		}
	})

	run("用例2：接受与撤回并发只有一个生效，不写入超出邀请的角色或单元", func(t *testing.T) {
		wins := map[string]int{}
		for i := range 4 {
			subject, email := newcomer(fmt.Sprintf("race%d", i), false)
			out := invite(fresh(admin, t1), `{"email":"`+email+`","name":"并发","roles":["pursuit_lead"],"unit_roles":[{"org_unit_id":"`+string(gas.ID)+`","roles":["research_analyst"]}]}`)
			id := string(out.Invitation.ID)
			release := lockInvitationRow(t, ctx, iam, id)
			type answer struct {
				code int
				raw  []byte
			}
			acc, rev := make(chan answer, 1), make(chan answer, 1)
			startAccept := func() { go func() { c, r := accept(plain(subject, ""), out.LinkToken.Value); acc <- answer{c, r} }() }
			startRevoke := func() {
				go func() {
					c, r := do(fresh(admin, t1), "POST", invitations+"/"+id+"/revoke", `{"expected_version":1}`)
					rev <- answer{c, r}
				}()
			}
			// 两个请求都在邀请行锁上排队后再放行；交替排队顺序，使接受先生效与撤回先生效两个方向都被覆盖
			if i%2 == 0 {
				startAccept()
				waitForLockWaiters(t, ctx, iam, 1)
				startRevoke()
			} else {
				startRevoke()
				waitForLockWaiters(t, ctx, iam, 1)
				startAccept()
			}
			waitForLockWaiters(t, ctx, iam, 2)
			release()
			a, r := <-acc, <-rev
			state, roles, units := memberRow(t1, subject)
			switch {
			case a.code == 200 && r.code == 409:
				wins["accept"]++
				if state != "active" || !slices.Equal(roles, []string{"pursuit_lead"}) || units != 1 || invitationEvents(id, "revoked") != 0 {
					t.Fatalf("接受生效后的成员：%s %v %d", state, roles, units)
				}
				if c := codeOf(r.raw); c != "invitation_not_pending" && c != "version_conflict" {
					t.Fatalf("撤回失败原因：%s", r.raw)
				}
			case r.code == 200 && a.code == 410 && codeOf(a.raw) == "invitation_revoked":
				wins["revoke"]++
				if state != "" || invitationEvents(id, "accepted") != 0 || memberEvents(t1, subject) != 0 {
					t.Fatal("撤回生效后仍写入了成员")
				}
			default:
				t.Fatalf("接受 %d %s；撤回 %d %s", a.code, a.raw, r.code, r.raw)
			}
		}
		if wins["accept"] != 2 || wins["revoke"] != 2 {
			t.Fatalf("两个方向都应被覆盖：%v", wins)
		}
	})

	run("用例3：邮箱不一致、账号已属于其他租户、受邀管理员未完成TOTP时拒绝", func(t *testing.T) {
		// 邮箱不一致与未验证
		subject, email := newcomer("new3", false)
		out := invite(fresh(admin, t1), `{"email":"`+email+`","name":"邮箱","roles":["business_developer"]}`)
		link := out.LinkToken.Value
		idp.Put(idapi.Account{Subject: subject, Email: "someone-else@example.com", EmailVerified: true})
		if code, raw := accept(plain(subject, ""), link); code != 403 || codeOf(raw) != "invitation_email_mismatch" {
			t.Fatalf("%d %s", code, raw)
		}
		idp.Put(idapi.Account{Subject: subject, Email: email, EmailVerified: false})
		if code, raw := accept(plain(subject, ""), link); code != 403 || codeOf(raw) != "email_unverified" {
			t.Fatalf("%d %s", code, raw)
		}
		stranger, _ := newcomer("stranger3", false)
		if code, raw := accept(plain(stranger, ""), link); code != 403 || codeOf(raw) != "invitation_email_mismatch" {
			t.Fatalf("%d %s", code, raw)
		}
		// IdP 不可用：失败关闭，邀请不被消耗
		idp.Put(idapi.Account{Subject: subject, Email: email, EmailVerified: true})
		idp.SetFailures(true, false)
		if code, raw := accept(plain(subject, ""), link); code != 503 || codeOf(raw) != "identity_unavailable" {
			t.Fatalf("%d %s", code, raw)
		}
		idp.SetFailures(false, false)
		if s, _, _ := memberRow(t1, subject); s != "" || invitationEvents(string(out.Invitation.ID), "accepted") != 0 {
			t.Fatal("被拒绝的接受写入了成员")
		}
		if code, raw := accept(plain(subject, ""), link); code != 200 {
			t.Fatalf("核验通过后仍可接受：%d %s", code, raw)
		}
		// 账号已属于其他租户：拒绝，不迁移、不新增第二个租户资格
		idp.Put(idapi.Account{Subject: admin2, Email: admin2 + "@example.com", EmailVerified: true, TOTPEnrolled: true})
		out = invite(fresh(admin, t1), `{"email":"`+admin2+`@example.com","name":"他租户","roles":["business_developer"]}`)
		for _, token := range []string{plain(admin2, ""), plain(admin2, t2)} {
			if code, raw := accept(token, out.LinkToken.Value); code != 409 || codeOf(raw) != "account_in_other_tenant" {
				t.Fatalf("%d %s", code, raw)
			}
		}
		if s, _, _ := memberRow(t1, admin2); s != "" {
			t.Fatal("其他租户的账号加入了本租户")
		}
		// 同一新账号同时接受两个租户的邀请：只有一个成功
		both, bothEmail := newcomer("both3", false)
		one := invite(fresh(admin, t1), `{"email":"`+bothEmail+`","name":"两租户","roles":["business_developer"]}`)
		two := invite(fresh(admin2, t2), `{"email":"`+bothEmail+`","name":"两租户","roles":["business_developer"]}`)
		codes := make(chan string, 2)
		var wg sync.WaitGroup
		for _, l := range []string{one.LinkToken.Value, two.LinkToken.Value} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, raw := accept(plain(both, ""), l)
				codes <- fmt.Sprintf("%d %s", c, codeOf(raw))
			}()
		}
		wg.Wait()
		close(codes)
		got := []string{}
		for c := range codes {
			got = append(got, c)
		}
		slices.Sort(got)
		if !slices.Equal(got, []string{"200 ", "409 account_in_other_tenant"}) {
			t.Fatalf("同一账号两个租户：%v", got)
		}
		// 受邀管理员：未登记 TOTP、未在15分钟内完成强认证时不能完成接受
		newAdmin, adminEmail := newcomer("admin3", false)
		out = invite(fresh(admin, t1), `{"email":"`+adminEmail+`","name":"新管理员","roles":["admin"]}`)
		link = out.LinkToken.Value
		if code, raw := accept(plain(newAdmin, ""), link); code != 403 || codeOf(raw) != "totp_enrollment_required" {
			t.Fatalf("%d %s", code, raw)
		}
		idp.Put(idapi.Account{Subject: newAdmin, Email: adminEmail, EmailVerified: true, TOTPEnrolled: true})
		if code, raw := accept(plain(newAdmin, ""), link); code != 403 || codeOf(raw) != "mfa_required" {
			t.Fatalf("%d %s", code, raw)
		}
		if code, raw := accept(tok(newAdmin, "", authAt(clock.Now().Add(-16*time.Minute))), link); code != 403 || codeOf(raw) != "mfa_required" {
			t.Fatalf("%d %s", code, raw)
		}
		if s, _, _ := memberRow(t1, newAdmin); s != "" {
			t.Fatal("未完成强认证的受邀管理员已写入")
		}
		if code, raw := accept(fresh(newAdmin, ""), link); code != 200 {
			t.Fatalf("%d %s", code, raw)
		}
		if s, roles, _ := memberRow(t1, newAdmin); s != "active" || !slices.Equal(roles, []string{"admin"}) {
			t.Fatalf("%s %v", s, roles)
		}
	})

	run("用例4：接受不经成员管理接口；成员管理接口对非管理员仍拒绝", func(t *testing.T) {
		subject, email := newcomer("new4", false)
		out := invite(fresh(admin, t1), `{"email":"`+email+`","name":"自助","roles":["business_developer"]}`)
		// 受邀人在接受前、接受后都不能使用成员管理接口
		expect(fresh(subject, ""), "PUT", "/v1/identity/members/"+subject, `{"state":"active","roles":["business_developer"],"expected_version":0}`, 403, "invalid_tenant_context")
		if code, raw := accept(plain(subject, ""), out.LinkToken.Value); code != 200 {
			t.Fatalf("%d %s", code, raw)
		}
		expect(fresh(subject, t1), "PUT", "/v1/identity/members/"+subject, `{"state":"active","roles":["admin"],"expected_version":1}`, 403, "forbidden")
		expect(fresh(subject, t1), "POST", invitations, `{"email":"y4@example.com","name":"丁","roles":["admin"]}`, 403, "forbidden")
		expect(fresh(subject, t1), "POST", invitations+"/"+string(out.Invitation.ID)+"/revoke", `{"expected_version":2}`, 403, "forbidden")
		if _, roles, _ := memberRow(t1, subject); !slices.Equal(roles, []string{"business_developer"}) {
			t.Fatalf("%v", roles)
		}
	})

	run("邀请管理：已有邀请、幂等重放、跨租户、投递失败与上限", func(t *testing.T) {
		_, email := newcomer("new8", false)
		body := `{"email":"` + email + `","name":"管理","roles":["business_developer"]}`
		first := invite(fresh(admin, t1), body, "invite_key_000000001")
		// 同一幂等键重放：同一邀请，不再给出链接令牌，不再发送
		again := invite(fresh(admin, t1), body, "invite_key_000000001")
		if again.Invitation.ID != first.Invitation.ID || again.LinkToken.Set || len(mail.Sent(email)) != 1 {
			t.Fatalf("%+v", again)
		}
		// 同一邮箱已有待接受邀请：返回已有邀请（不发新链接）
		existing := invite(fresh(admin, t1), strings.Replace(body, "管理", "管理2", 1))
		if existing.Outcome != "existing" || existing.Invitation.ID != first.Invitation.ID || existing.LinkToken.Set || len(mail.Sent(email)) != 1 {
			t.Fatalf("%+v", existing)
		}
		// 其他租户的邀请与不存在的邀请：同一 404
		for _, id := range []string{string(first.Invitation.ID), ids.New("inv")} {
			expect(fresh(admin2, t2), "POST", invitations+"/"+id+"/revoke", `{"expected_version":1}`, 404, "invitation_not_found")
			expect(fresh(admin2, t2), "POST", invitations+"/"+id+"/resend", `{"expected_version":1}`, 404, "invitation_not_found")
		}
		if code, raw := accept(plain(admin2, ""), "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); code != 404 || codeOf(raw) != "invitation_not_found" {
			t.Fatalf("%d %s", code, raw)
		}
		// 单元不属于本租户、角色为空：拒绝
		expect(fresh(admin2, t2), "POST", invitations, `{"email":"z8@example.com","name":"乙","roles":[],"unit_roles":[{"org_unit_id":"`+string(gas.ID)+`","roles":["decision_maker"]}]}`, 422, "org_unit_not_found")
		expect(fresh(admin2, t2), "POST", invitations, `{"email":"z8@example.com","name":"乙","roles":[]}`, 400, "invalid_request")
		expect(fresh(admin2, t2), "POST", invitations, `{"email":"z8@example.com","name":"乙","roles":["admin"],"tenant":"`+t1+`"}`, 400, "")
		// 邮件发送失败：邀请仍创建，状态为发送失败；重发成功后为已发送
		mail.SetFail(true)
		_, failEmail := newcomer("mailfail8", false)
		failed := invite(fresh(admin, t1), `{"email":"`+failEmail+`","name":"失败","roles":["business_developer"]}`)
		mail.SetFail(false)
		if failed.Invitation.Delivery != "failed" || !failed.LinkToken.Set {
			t.Fatalf("%+v", failed)
		}
		var resent issued
		decode(expect(fresh(admin, t1), "POST", invitations+"/"+string(failed.Invitation.ID)+"/resend", `{"expected_version":1}`, 200, ""), &resent)
		if resent.Invitation.Delivery != "sent" || len(mail.Sent(failEmail)) != 1 {
			t.Fatalf("%+v", resent.Invitation)
		}
		// 待接受上限：写入前核对，达到上限时不创建、不发布
		if _, err := owner.Exec(ctx, `INSERT INTO identity_invitations(id,tenant_id,email,name,roles,state,version,inviter,link_generation,expires_at,delivery)
 SELECT 'inv_' || lpad(upper(to_hex(g)), 26, '0'), $1, 'bulk' || g || '@example.com', '批量', ARRAY['business_developer'], 'pending', 1, $2, 1, now() + interval '7 days', 'sent'
 FROM generate_series(1, $3) g`, t2, admin2, 1000); err != nil {
			t.Fatal(err)
		}
		before := count(`SELECT count(*) FROM kernel_outbox`)
		expect(fresh(admin2, t2), "POST", invitations, `{"email":"over8@example.com","name":"超限","roles":["business_developer"]}`, 409, "invitation_limit_reached")
		if count(`SELECT count(*) FROM identity_invitations WHERE email='over8@example.com'`) != 0 || count(`SELECT count(*) FROM kernel_outbox`) != before {
			t.Fatal("超限的邀请写入了数据或事件")
		}
		var list identityapi.InvitationList
		decode(expect(fresh(admin2, t2), "GET", invitations, "", 200, ""), &list)
		if len(list.Invitations) != 500 || !list.Truncated {
			t.Fatalf("列表：%d %v", len(list.Invitations), list.Truncated)
		}
		// 链接令牌原文不落库
		var leaked int
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM identity_commands WHERE response::text LIKE '%' || $1 || '%'`, first.LinkToken.Value).Scan(&leaked); err != nil || leaked != 0 {
			t.Fatalf("链接令牌写入了回执：%d %v", leaked, err)
		}
	})

	run("持久化的 identity 事件符合契约", func(t *testing.T) {
		rows, err := owner.Query(ctx, `SELECT envelope FROM kernel_outbox WHERE envelope->>'type' LIKE 'identity.%'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var envelope outbox.Envelope
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			schema, err := events.Schema(envelope.Type)
			if err != nil {
				t.Fatal(err)
			}
			if err := events.Validate(schema, envelope.Data); err != nil {
				t.Fatalf("%s：%v：%s", envelope.Type, err, envelope.Data)
			}
			if envelope.Type == "identity.invitation.changed.v1" && (envelope.Tenant == "" || strings.Contains(string(envelope.Data), "@")) {
				t.Fatalf("邀请事件须归属租户且不含邮箱：%s", raw)
			}
			n++
		}
		if rows.Err() != nil || n < 20 {
			t.Fatalf("事件 %d：%v", n, rows.Err())
		}
	})
}

// lockInvitationRow 在测试连接中锁住邀请行，作为并发屏障：并发的命令都在该行锁上等待，release 后再竞争。
func lockInvitationRow(t *testing.T, ctx context.Context, database testenv.Database, id string) func() {
	t.Helper()
	conn, err := pgx.Connect(ctx, database.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM identity_invitations WHERE id=$1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = tx.Rollback(ctx)
			_ = conn.Close(ctx)
		})
	}
	t.Cleanup(release)
	return release
}

// waitForLockWaiters 等待至少 n 个会话在行锁上等待（其他角色会话的等待事件只对超级用户可见）。
func waitForLockWaiters(t *testing.T, ctx context.Context, database testenv.Database, n int) {
	t.Helper()
	monitor, err := pgx.Connect(ctx, os.Getenv("MIAOQIA_TEST_PG"))
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Close(ctx)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var waiting int
		if err := monitor.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock'`, database.Name).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
	}
	t.Fatalf("没有 %d 个会话在锁上等待", n)
}
