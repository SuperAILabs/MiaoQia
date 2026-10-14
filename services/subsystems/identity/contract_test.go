package identity_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api/apitest"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api/remote"
)

// 授权决策点的本地绑定与远程绑定通过同一套契约用例（真实 OpenFGA）。
func TestAuthorizerContract(t *testing.T) {
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
}
