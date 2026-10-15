package gateway_test

import (
	"context"
	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/internal/testissuer"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"net/http"
	"net/http/httptest"
	"testing"
)

type resolver func(context.Context) (*api.Session, error)

func (r resolver) Resolve(ctx context.Context) (*api.Session, error) { return r(ctx) }

func TestDirectoryResponseCannotChangeIdentity(t *testing.T) {
	t.Setenv("MIAOQIA_ENV", "production")
	issuer := testissuer.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tenant := "tnt_01JA8ZXXXXXXXXXXXXXXXXXXX1"
	for _, c := range []struct {
		name, tenant, cell, subject, state string
		nilValue                           bool
		want                               int
	}{
		{"valid", tenant, "cell-01", "member", "active", false, 204},
		{"different tenant", "other", "cell-01", "member", "active", false, 403},
		{"different cell", tenant, "cell-02", "member", "active", false, 403},
		{"different subject", tenant, "cell-01", "other", "active", false, 403},
		{"removed", tenant, "cell-01", "member", "removed", false, 403},
		{"empty response", "", "", "", "", true, 403},
	} {
		t.Run(c.name, func(t *testing.T) {
			hit := false
			target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hit = true
				if r.Header.Get(reqctx.Header) != "" {
					t.Error("client context was forwarded")
				}
				w.WriteHeader(204)
			})
			directory := resolver(func(context.Context) (*api.Session, error) {
				if c.nilValue {
					return nil, nil
				}
				return &api.Session{Tenant: c.tenant, Cell: identityapi.SessionCell(c.cell), Subject: c.subject, State: identityapi.SessionState(c.state), Roles: []identityapi.SessionRolesItem{}, TenantVersion: 1, MemberVersion: 1}, nil
			})
			h, err := gateway.Public(ctx, issuer.Server.URL, "miaoqia-api", target, directory)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("Authorization", "Bearer "+issuer.Token(t, "member", tenant, "cell-01", false))
			r.Header.Set(reqctx.Header, "injected")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.want || hit != (c.want == 204) {
				t.Fatalf("status %d hit %v", w.Code, hit)
			}
		})
	}
}
