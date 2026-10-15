package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// 重定向后的请求会重新附加请求上下文头；内部授权接口的任何重定向都必须拒绝，且不得把目标的结果当作授权结果。
func TestCheckRefusesRedirects(t *testing.T) {
	t.Setenv("MIAOQIA_ENV", "development")
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		var reached atomic.Bool
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"allowed":true}`))
		}))
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+r.URL.Path, code)
		}))
		a, err := NewAuthorizer(origin.URL + "/internal")
		if err != nil {
			t.Fatal(err)
		}
		ctx := reqctx.With(context.Background(), reqctx.Context{Principal: reqctx.Principal{ID: "usr_1", Kind: "user"}, Tenant: "tnt_1", Cell: "c1"})
		ok, err := a.Check(ctx, authz.Request{Relation: "viewer", Object: "doc:1"})
		if err == nil || ok {
			t.Errorf("redirect %d: allowed=%v err=%v, want refusal", code, ok, err)
		}
		if reached.Load() {
			t.Errorf("redirect %d: request with context reached redirect target", code)
		}
		origin.Close()
		target.Close()
	}
}
