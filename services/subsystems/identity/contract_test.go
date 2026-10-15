package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api/apitest"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api/remote"
)

// 授权决策点的本地绑定与远程绑定通过同一套契约用例（真实 OpenFGA）。
func TestAuthorizerContract(t *testing.T) {
	t.Setenv("MIAOQIA_ENV", "development")
	url := os.Getenv("MIAOQIA_TEST_OPENFGA")
	if url == "" {
		t.Skip("未设置 MIAOQIA_TEST_OPENFGA（例如 http://localhost:8081）")
	}
	ctx := context.Background()
	op := ids.New("usr")
	if err := identity.Grant(ctx, url, "user:"+op, "operator", "platform:miaoqia"); err != nil {
		t.Fatal(err)
	}
	fx := apitest.Fixture{Granted: authz.Request{Subject: "user:" + op, Relation: "can_trigger_probe", Object: "platform:miaoqia"},
		Stranger: ids.New("usr")}

	env := &runtime.Env{Config: func(k string) string {
		if k == "OPENFGA_URL" {
			return url
		}
		return ""
	}, Bindings: runtime.NewBindings()}
	m := identity.Module()
	if err := m.Start(ctx, env); err != nil {
		t.Fatal(err)
	}
	local, _ := runtime.Lookup[api.Authorizer](env.Bindings, api.AuthorizerBinding)
	t.Run("本地绑定", func(t *testing.T) { apitest.RunAuthorizer(t, local, fx) })

	internal := http.NewServeMux()
	m.Routes(env, http.NewServeMux(), internal)
	srv := httptest.NewServer(internal)
	defer srv.Close()
	rem, err := remote.NewAuthorizer(srv.URL + "/internal")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("远程绑定", func(t *testing.T) { apitest.RunAuthorizer(t, rem, fx) })
	t.Run("HTTP负向契约", func(t *testing.T) {
		for _, c := range []struct {
			name, body, header string
			status             int
		}{
			{"未知业务租户", `{"relation":"can_trigger_probe","object":"platform:miaoqia","tenant":"other"}`, reqctx.Encode(reqctx.Context{Principal: reqctx.Principal{ID: "svc:review", Kind: "service"}}), 400},
			{"非法关系", `{"relation":"can:read","object":"platform:miaoqia"}`, reqctx.Encode(reqctx.Context{Principal: reqctx.Principal{ID: "svc:review", Kind: "service"}}), 400},
			{"无上下文", `{"relation":"can_trigger_probe","object":"platform:miaoqia"}`, "", 401},
			{"空主体", `{"relation":"can_trigger_probe","object":"platform:miaoqia"}`, reqctx.Encode(reqctx.Context{}), 400},
		} {
			t.Run(c.name, func(t *testing.T) {
				r, _ := http.NewRequest(http.MethodPost, srv.URL+"/internal/v1/authz:check", bytes.NewBufferString(c.body))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set(reqctx.Header, c.header)
				resp, err := http.DefaultClient.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != c.status || resp.Header.Get("Content-Type") != "application/problem+json" {
					t.Fatalf("status=%d type=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
				}
				var problem map[string]any
				if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
					t.Fatal(err)
				}
				if problem["status"] != float64(c.status) || problem["code"] == nil {
					t.Fatalf("problem=%v", problem)
				}
			})
		}
	})
}
