package dataread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/ogen-go/ogen/validate"
	dataapi "github.com/superailabs/miaoqia/services/contracts/apis/data-api"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

const testPublicOrg = "org_00000000000000000000000001"

func TestReadPublicOrganizationChecksWireContract(t *testing.T) {
	const prefix = `{"org_id":"` + testPublicOrg + `","version":2,"name":"合成登记企业","uscc":"91000000000000000K",`
	for _, item := range []struct {
		name, response string
		status         int
		wantValue      bool
		wantError      bool
	}{
		{"current", prefix + `"rights_tags":["public:registry"]}`, 200, true, false},
		{"missing", `null`, 200, false, false},
		{"other version", `{"org_id":"` + testPublicOrg + `","version":3,"name":"主体","uscc":"91000000000000000K","rights_tags":["public:registry"]}`, 200, false, true},
		{"other id", `{"org_id":"org_00000000000000000000000002","version":2,"name":"主体","uscc":"91000000000000000K","rights_tags":["public:registry"]}`, 200, false, true},
		{"mixed tags", prefix + `"rights_tags":["public:registry","restricted"]}`, 200, false, true},
		{"no tags", prefix + `"rights_tags":[]}`, 200, false, true},
		{"duplicate tags", prefix + `"rights_tags":["public:registry","public:registry"]}`, 200, false, true},
		{"extra field", prefix + `"rights_tags":["public:registry"],"revision_ref":"exr_private"}`, 200, false, true},
		{"bad checksum", `{"org_id":"` + testPublicOrg + `","version":2,"name":"主体","uscc":"91000000000000000A","rights_tags":["public:registry"]}`, 200, false, true},
		{"denied", `{"title":"request rejected","status":403}`, 403, false, true},
	} {
		t.Run(item.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/data/get-public-organization" {
					t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
					!reflect.DeepEqual(body, map[string]any{"org_id": testPublicOrg, "version": float64(2)}) {
					t.Errorf("unexpected body %v: %v", body, err)
				}
				if _, err := reqctx.Decode(r.Header.Get(reqctx.Header)); err != nil {
					t.Errorf("missing caller context: %v", err)
				}
				contentType := "application/json"
				if item.status >= 400 {
					contentType = "application/problem+json"
				}
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(item.status)
				fmt.Fprint(w, item.response)
			}))
			defer server.Close()
			client, err := NewWithHTTPClient(server.URL, &http.Client{Transport: binding.Transport{}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.ReadPublicOrganization(readContext(), dataapi.PublicOrganizationRequest{OrgID: testPublicOrg, Version: 2})
			if (err != nil) != item.wantError || (got != nil) != item.wantValue {
				t.Fatalf("unexpected result %#v: %v", got, err)
			}
			if item.status == 403 {
				var problem *Error
				if !errors.As(err, &problem) || problem.Status != 403 {
					t.Fatalf("not a classified denial: %v", err)
				}
			}
		})
	}
}

func TestReadPublicOrganizationRejectsInvalidRequestsBeforeCallingOwner(t *testing.T) {
	client, err := NewWithHTTPClient("http://localhost:1", &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []dataapi.PublicOrganizationRequest{
		{OrgID: testPublicOrg, Version: 0},
		{OrgID: "ogr_00000000000000000000000001", Version: 1},
	} {
		var invalid *validate.Error
		if _, err := client.ReadPublicOrganization(readContext(), request); !errors.As(err, &invalid) {
			t.Fatalf("expected request validation for %#v: %v", request, err)
		}
	}
}

func TestPublicOrganizationAgainstPythonListener(t *testing.T) {
	base, id := os.Getenv("MIAOQIA_TEST_PUBLIC_ORG_URL"), os.Getenv("MIAOQIA_TEST_PUBLIC_ORG_ID")
	if base == "" {
		t.Skip("run via the Python PostgreSQL fixture with MIAOQIA_TEST_DATAREAD_GO=1")
	}
	client, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	ctx := reqctx.With(context.Background(), reqctx.Context{
		Tenant: "test-tenant", Cell: "test-cell", Principal: reqctx.Principal{ID: "synthetic-user", Kind: "user"}, Purpose: "interactive",
	})
	value, err := client.ReadPublicOrganization(ctx, dataapi.PublicOrganizationRequest{OrgID: id, Version: 1})
	if err != nil || value == nil || value.Uscc != "91000000000000000K" || value.Name != "合成测试登记企业甲" ||
		!reflect.DeepEqual(value.RightsTags, []string{"public:synthetic-enterprise-registry"}) {
		t.Fatalf("get-public-organization: %#v, %v", value, err)
	}
	value, err = client.ReadPublicOrganization(ctx, dataapi.PublicOrganizationRequest{OrgID: id, Version: 2})
	if err != nil || value != nil {
		t.Fatalf("noncurrent get-public-organization: %#v, %v", value, err)
	}
}
