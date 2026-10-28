package integration_test

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/internal/testissuer"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/commerce"
	commerceapi "github.com/superailabs/miaoqia/services/subsystems/commerce/api"
	commerceremote "github.com/superailabs/miaoqia/services/subsystems/commerce/api/remote"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext"
	"github.com/superailabs/miaoqia/services/subsystems/detection"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api/facttest"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	gwapi "github.com/superailabs/miaoqia/services/subsystems/gateway/api"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/api/publictest"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	idapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// flakyCoverage 在 gateway 与 commerce 覆盖判断之间注入故障。
type flakyCoverage struct {
	next  commerceapi.Coverage
	fail  atomic.Bool
	calls atomic.Int64
}

func (f *flakyCoverage) CheckCoverage(ctx context.Context, domain, region string) (*commerceapi.CoverageDecision, error) {
	f.calls.Add(1)
	if f.fail.Load() {
		return nil, errors.New("注入：commerce 不可用")
	}
	return f.next.CheckCoverage(ctx, domain, region)
}

// 验收线 R-05：对象页的覆盖判断经 commerce 的 checkCoverage（gateway 未显式给出 Entitlements 时的生产装配，mTLS 内部端口的远程绑定，
// 以用户主体转交）。覆盖包为部署配置 MIAOQIA_COMMERCE_COVERAGE；数据平面与证据仍为夹具（publictest）。
// 覆盖内成功；区域或信息领域在覆盖外为 403 entitlement.coverage_insufficient，只带对象名称、不计量；commerce 故障或对象缺少信息领域为 503。
func TestObjectCoverageThroughCommerce(t *testing.T) {
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

	iam := testenv.NewDatabase(t)
	idModule := identity.Module()
	if err := db.Migrate(ctx, iam.OwnerDSN, iam.Name+"_app", idModule.Migrations); err != nil {
		t.Fatal(err)
	}
	t1, t2 := ids.New("tnt"), ids.New("tnt")
	suffix := strings.ToLower(iam.Name)
	admin, admin2 := "admin-"+suffix, "admin2-"+suffix
	iamOwner, err := pgx.Connect(ctx, iam.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer iamOwner.Close(ctx)
	if _, err := iamOwner.Exec(ctx, `INSERT INTO identity_tenants VALUES ($1,'甲','cell-01','active',1), ($2,'乙','cell-01','active',1)`, t1, t2); err != nil {
		t.Fatal(err)
	}
	if _, err := iamOwner.Exec(ctx, `INSERT INTO identity_members VALUES ($1,$2,'active',ARRAY['admin'],1), ($3,$4,'active',ARRAY['admin'],1)`, t1, admin, t2, admin2); err != nil {
		t.Fatal(err)
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

	packs, _ := filepath.Abs(filepath.Join("..", "..", "..", "packs"))
	config := func(k string) string {
		switch k {
		case "PACKS_DIR":
			return packs
		case "CURSOR_KEY":
			return base64.StdEncoding.EncodeToString([]byte("coverage-test-cursor-key-0123456789abcd"))
		case "COVERAGE": // commerce 的覆盖包：采购只覆盖浙江，组织覆盖全部区域
			return "INF-PROC=330000;INF-ORG=*"
		}
		return ""
	}
	start := func(m runtime.Module) (*runtime.Env, *pgx.Conn) {
		t.Helper()
		database := testenv.NewDatabase(t)
		if err := db.Migrate(ctx, database.OwnerDSN, database.Name+"_app", m.Migrations); err != nil {
			t.Fatal(err)
		}
		app, err := db.Open(ctx, database.AppDSN)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(app.Close)
		owner, err := pgx.Connect(ctx, database.OwnerDSN)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { owner.Close(context.Background()) })
		env := &runtime.Env{DB: app, Bindings: bindings, Log: quiet, Cell: "cell-01", Config: config}
		if err := m.Start(ctx, env); err != nil {
			t.Fatal(err)
		}
		return env, owner
	}
	public, internal := http.NewServeMux(), http.NewServeMux()
	idModule.Routes(idEnv, public, internal)
	ccModule := customercontext.Module()
	ccEnv, _ := start(ccModule)
	ccModule.Routes(ccEnv, public, internal)
	facts := facttest.New(t1, t2)
	detModule := detection.ModuleWith(detection.Adapters{Facts: facts, Candidates: facts, ProjectLinks: facts})
	detEnv, _ := start(detModule)
	detModule.Routes(detEnv, public, internal)
	comModule := commerce.Module()
	comEnv, comOwner := start(comModule)
	comModule.Routes(comEnv, public, internal)

	certs := testenv.NewTLS(t)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CERT", certs.CertPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_KEY", certs.KeyPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CA", certs.CA)
	t.Setenv("MIAOQIA_INTERNAL_ALLOWED_PEERS", "spiffe://miaoqia/service/platform-api")
	intern := httptest.NewUnstartedServer(internal)
	intern.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: certs.Pool, Certificates: []tls.Certificate{certs.Cert}, MinVersion: tls.VersionTLS13}
	intern.StartTLS()
	defer intern.Close()
	remoteCoverage, err := commerceremote.NewCoverage(intern.URL + "/internal")
	if err != nil {
		t.Fatal(err)
	}
	coverage := &flakyCoverage{next: remoteCoverage}
	bindings.Provide(commerceapi.CoverageBinding, commerceapi.Coverage(coverage))

	pub := publictest.New()
	long := time.Now().Add(-365 * 24 * time.Hour)
	inZJ, inGD, org, noDomain := ids.New("pp"), ids.New("pp"), ids.New("org"), ids.New("pp")
	pub.PutObject(long, gwapi.Object{ID: inZJ, Type: "procurement_project", Name: "夹具：浙江某管网工程", Region: "330106", InfoDomain: "INF-PROC"})
	pub.PutObject(long, gwapi.Object{ID: inGD, Type: "procurement_project", Name: "夹具：广东某管廊工程", Region: "440305", InfoDomain: "INF-PROC"})
	pub.PutObject(long, gwapi.Object{ID: org, Type: "organization", Name: "夹具：区划未知的总包", InfoDomain: "INF-ORG"})
	pub.PutObject(long, gwapi.Object{ID: noDomain, Type: "procurement_project", Name: "夹具：缺少信息领域", Region: "330106"})
	// 未显式给出 Entitlements：生产装配经 commerce 覆盖判断绑定
	gwModule := gateway.ModuleWith(gateway.Adapters{Public: pub, Evidence: publictest.Resolver{Store: pub}})
	gwEnv, _ := start(gwModule)
	gwModule.Routes(gwEnv, public, internal)

	issuer := testissuer.New(t)
	resolver, _ := runtime.Lookup[idapi.SessionResolver](bindings, idapi.SessionBinding)
	secured, err := gateway.Public(ctx, issuer.Server.URL, "miaoqia-api", public, resolver)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(secured)
	defer server.Close()
	get := func(t *testing.T, subject, tenant, object string, status int, code string) map[string]any {
		t.Helper()
		r, _ := http.NewRequest("GET", server.URL+"/v1/objects/"+object, nil)
		r.Header.Set("Authorization", "Bearer "+issuer.Token(t, subject, tenant, "cell-01", false))
		r.Header.Set("Idempotency-Key", ids.New("req")[4:])
		resp, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		if resp.StatusCode != status || (code != "" && out["code"] != code) {
			t.Fatalf("%s：得到 %d，期望 %d %s：%s", object, resp.StatusCode, status, code, raw)
		}
		return out
	}
	meters := func(target string) int {
		var n int
		if err := comOwner.QueryRow(ctx, `SELECT count(*) FROM com_view_meters WHERE target = $1`, target).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("覆盖内：对象所在区划在覆盖包内（省级包含县级）；全部区域覆盖时区划未知也可打开", func(t *testing.T) {
		if p := get(t, admin, t1, inZJ, 200, ""); p["object"].(map[string]any)["id"] != inZJ {
			t.Fatalf("%+v", p)
		}
		get(t, admin, t1, org, 200, "")
		if meters(inZJ) != 1 {
			t.Fatal("覆盖内的查看应计量")
		}
	})

	t.Run("覆盖外：区划不在覆盖包内为 403 coverage_insufficient，只带对象名称，不返回内容、不计量；两个租户相同", func(t *testing.T) {
		for _, who := range [][2]string{{admin, t1}, {admin2, t2}} {
			p := get(t, who[0], who[1], inGD, 403, "entitlement.coverage_insufficient")
			if o := p["object"].(map[string]any); o["id"] != inGD || o["name"] != "夹具：广东某管廊工程" || p["sections"] != nil || p["meter"] != nil {
				t.Fatalf("%+v", p)
			}
		}
		if meters(inGD) != 0 {
			t.Fatal("覆盖外的请求被计量")
		}
	})

	t.Run("失败关闭：commerce 不可用、对象缺少信息领域为 503 entitlement_unavailable，不当作覆盖内或覆盖外", func(t *testing.T) {
		coverage.fail.Store(true)
		get(t, admin, t1, inZJ, 503, "entitlement_unavailable")
		coverage.fail.Store(false)
		before := coverage.calls.Load()
		get(t, admin, t1, noDomain, 503, "entitlement_unavailable")
		if coverage.calls.Load() != before {
			t.Fatal("缺少信息领域时不应调用 commerce")
		}
		get(t, admin, t1, inZJ, 200, "")
	})

	t.Run("越权：不经调用管线转交请求上下文的内部调用被拒绝，不给出判断（服务主体与无租户的拒绝见 commerce 单元测试）", func(t *testing.T) {
		// 只有 mTLS 服务身份、没有转交的用户主体与租户
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: certs.Pool, Certificates: []tls.Certificate{certs.Cert}, MinVersion: tls.VersionTLS13}}}
		resp, err := client.Get(intern.URL + "/internal/v1/commerce/coverage?info_domain=INF-PROC&region=330106")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 401 || !strings.Contains(string(raw), `"missing_context"`) || strings.Contains(string(raw), `"covered"`) {
			t.Fatalf("%d %s", resp.StatusCode, raw)
		}
	})
}
