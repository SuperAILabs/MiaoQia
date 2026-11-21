package main

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"time"

	identitycontract "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/dataread"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	identityapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/research"
	researchapi "github.com/superailabs/miaoqia/services/subsystems/research/api"
)

const a02ResearchRoute = "POST /v1/research/a02"

func registerA02Research(mux *http.ServeMux, env *runtime.Env) error {
	configured := env.Config("DATA_API_URL")
	if configured == "" {
		return nil
	}
	if _, err := authenticatedHTTPSURL(configured); err != nil {
		return errors.New("MIAOQIA_RESEARCH_DATA_API_URL requires authenticated HTTPS")
	}
	reader, err := dataread.New(configured)
	if err != nil {
		return err
	}
	decider, ok := runtime.Lookup[authz.Decider](env.Bindings, identityapi.AuthorizerBinding)
	if !ok || decider == nil {
		return errors.New("A02 research requires the identity authorizer")
	}
	mux.Handle(a02ResearchRoute, a02ResearchHandler(reader, decider))
	return nil
}

func a02ResearchHandler(reader research.A02OwnerReads, decider authz.Decider) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		scope, ok := reqctx.From(r.Context())
		if !ok || scope.Validate() != nil || scope.Tenant == "" || scope.Purpose != "interactive" || scope.Principal.Kind != "user" {
			writePlatformProblem(w, http.StatusForbidden, "a02_research_denied")
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		var body struct {
			OrgUnitID identitycontract.OrgUnitID `json:"org_unit_id"`
			Notice    researchapi.NoticeVersion  `json:"notice"`
		}
		if err != nil || mediaType != "application/json" ||
			decodeStrictJSON(http.MaxBytesReader(w, r.Body, 4096), &body) != nil ||
			body.OrgUnitID.Validate() != nil {
			writePlatformProblem(w, http.StatusBadRequest, "invalid_a02_research_request")
			return
		}
		runner, err := research.NewA02(reader, research.A02Admission{
			Authz: decider,
			OrgUnit: func(context.Context) (string, error) {
				// Identity resolves this requested scope inside the authenticated tenant.
				return string(body.OrgUnitID), nil
			},
		}, research.A02Model{})
		if err != nil {
			writePlatformProblem(w, http.StatusServiceUnavailable, "a02_research_unavailable")
			return
		}
		report, err := runner.RunA02(r.Context(), researchapi.A02Request{
			Notice: body.Notice,
			Budget: researchapi.A02Budget{Reads: 16, Deadline: time.Now().Add(20 * time.Second)},
		})
		switch {
		case errors.Is(err, researchapi.ErrInvalidA02Request):
			writePlatformProblem(w, http.StatusBadRequest, "invalid_a02_research_request")
		case errors.Is(err, researchapi.ErrA02AccessDenied):
			writePlatformProblem(w, http.StatusForbidden, "a02_research_denied")
		case err != nil:
			writePlatformProblem(w, http.StatusServiceUnavailable, "a02_research_unavailable")
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(report)
		}
	})
}
