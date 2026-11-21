package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	dataapi "github.com/superailabs/miaoqia/services/contracts/apis/data-api"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/dataread"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/research/api"
)

const (
	maxA02Reads    = 16
	maxA02Duration = 30 * time.Second
)

var noticeID = regexp.MustCompile(`^ntc_[0-9A-HJKMNP-TV-Z]{26}$`)

// A02OwnerReads is the read-only subset of dataread.Reader used by this template.
// The implementation must check current caller and source rights on every read.
type A02OwnerReads interface {
	ReadFact(context.Context, dataapi.FactRequest) (*dataapi.FactRead, error)
	ReadBidResultUnits(context.Context, dataapi.FactRequest) (*dataapi.BidResultPage, error)
	ReadEvidence(context.Context, dataapi.EvidenceRequest) (*dataapi.EvidenceRead, error)
}

var _ A02OwnerReads = (dataread.Reader)(nil)

// A02Admission uses the existing identity decision point. OrgUnit resolves
// the study's organizational scope from trusted Cell-side context or an
// authorized target. The deployment Cell is not an organizational unit.
type A02Admission struct {
	Authz   authz.Decider
	OrgUnit func(context.Context) (string, error)
}

type a02Runner struct {
	reads     A02OwnerReads
	admission A02Admission
	model     A02Model
}

// NewA02 wires identity, the owner read port and an optional governed model.
func NewA02(reads A02OwnerReads, admission A02Admission, model A02Model) (api.A02Runner, error) {
	if reads == nil || admission.Authz == nil || admission.OrgUnit == nil {
		return nil, errors.New("research A02: owner reads and research-start admission are required")
	}
	if (model.Gateway == nil) != (model.Authorize == nil) {
		return nil, errors.New("research A02: model gateway requires model-use authorization")
	}
	return a02Runner{reads: reads, admission: admission, model: model}, nil
}

func (r a02Runner) RunA02(parent context.Context, request api.A02Request) (api.A02Report, error) {
	rc, ok := reqctx.From(parent)
	if !ok || rc.Validate() != nil || rc.Tenant == "" || rc.Principal.Kind != "user" || rc.Purpose != "interactive" {
		return api.A02Report{}, api.ErrA02AccessDenied
	}
	now := time.Now()
	budget := request.Budget
	if !noticeID.MatchString(request.Notice.ID) || request.Notice.Version < 1 ||
		budget.Reads < 0 || budget.Reads > maxA02Reads || budget.ModelCalls < 0 || budget.ModelCalls > 1 ||
		budget.Deadline.IsZero() || budget.Deadline.After(now.Add(maxA02Duration)) {
		return api.A02Report{}, api.ErrInvalidA02Request
	}
	ctx, cancel := context.WithDeadline(parent, budget.Deadline)
	defer cancel()
	if err := parent.Err(); err != nil {
		return api.A02Report{}, err
	}
	if ctx.Err() != nil {
		return api.A02Report{}, api.ErrA02AdmissionUnavailable
	}
	if err := r.admission.requireStart(ctx); err != nil {
		return api.A02Report{}, err
	}
	report := api.A02Report{
		Template: api.A02Template, Notice: request.Notice,
		Facts: []api.A02AwardFact{}, Inferences: []string{}, Recommendations: []api.A02Recommendation{},
		Unknowns: []api.A02Unknown{
			{Topic: "entity_identity", Reason: "entity_lookup_out_of_scope"},
			{Topic: "prime_contractor", Reason: "contract_role_requires_separate_evidence"},
			{Topic: "supply_window", Reason: "supply_window_requires_separate_evidence"},
			{Topic: "coverage", Reason: "fixed_notice_only"},
		},
	}
	readBudget := a02ReadBudget{max: budget.Reads, report: &report}
	if err := r.awardFacts(ctx, &readBudget); err != nil {
		return api.A02Report{}, err
	}
	if err := parent.Err(); err != nil {
		return api.A02Report{}, err
	}
	if budget.ModelCalls > 0 && len(report.Facts) > 0 {
		r.model.recommend(ctx, &report)
	}
	if err := parent.Err(); err != nil {
		return api.A02Report{}, err
	}
	return report, nil
}

func (a A02Admission) requireStart(ctx context.Context) error {
	unit, err := a.OrgUnit(ctx)
	if errors.Is(err, authz.ErrForbidden) || (err == nil && unit == "") {
		return api.ErrA02AccessDenied
	}
	if err != nil || ctx.Err() != nil {
		return api.ErrA02AdmissionUnavailable
	}
	err = authz.Require(ctx, a.Authz, "can_start_research", "org_unit:"+unit)
	switch {
	case errors.Is(err, authz.ErrForbidden):
		return api.ErrA02AccessDenied
	case err != nil:
		return api.ErrA02AdmissionUnavailable
	default:
		return nil
	}
}

type a02ReadBudget struct {
	max    int
	report *api.A02Report
}

func (b *a02ReadBudget) take(ctx context.Context, topic string) bool {
	if ctx.Err() != nil {
		markUnknown(b.report, topic, "deadline_exceeded")
		return false
	}
	if b.report.ReadsUsed == b.max {
		markUnknown(b.report, topic, "read_budget_exhausted")
		return false
	}
	b.report.ReadsUsed++
	return true
}

func markUnknown(report *api.A02Report, topic, reason string) {
	report.Partial = true
	report.Unknowns = append(report.Unknowns, api.A02Unknown{Topic: topic, Reason: reason})
}

// failedRead distinguishes explicit authorization failure from unavailable or unresolvable evidence.
func failedRead(ctx context.Context, report *api.A02Report, topic string, err error) error {
	var readErr *dataread.Error
	errors.As(err, &readErr)
	if errors.Is(err, api.ErrA02AccessDenied) ||
		(readErr != nil && (readErr.Status == http.StatusForbidden || readErr.Status == http.StatusUnauthorized)) {
		return api.ErrA02AccessDenied
	}
	switch {
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
		markUnknown(report, topic, "deadline_exceeded")
	case err == nil || (readErr != nil && readErr.Status == http.StatusNotFound):
		markUnknown(report, topic, "unresolved")
	default:
		markUnknown(report, topic, "owner_read_failed")
	}
	return nil
}

// noticeAwardFields is the narrow projection needed for citation verification.
// The owner bid-result view supplies the awarded subjects, and the notice supplies their field provenance.
type noticeAwardFields struct {
	snapshot string
	kind     string
	lots     []struct {
		Results []struct {
			ID     string `json:"id"`
			Stage  string `json:"stage"`
			Bidder struct {
				Name string `json:"name_raw"`
				Prov struct {
					EvidenceRef string `json:"evidence_ref"`
				} `json:"prov"`
			} `json:"bidder"`
		} `json:"bid_results"`
	}
}

type awardField struct {
	path, name, evidenceRef string
}

func (r a02Runner) awardFacts(ctx context.Context, b *a02ReadBudget) error {
	const awards = "award_result"
	report := b.report
	notice := report.Notice
	request := dataapi.FactRequest{FactID: notice.ID, Basis: dataapi.Basis{View: dataapi.BasisViewCurrent, RecordedCutoff: dataapi.NilDateTime{Null: true}}, Version: dataapi.NewNilInt64(notice.Version)}
	if !b.take(ctx, awards) {
		return nil
	}
	read, err := r.reads.ReadFact(ctx, request)
	if err != nil || read == nil || ctx.Err() != nil {
		return failedRead(ctx, report, awards, err)
	}
	var fields noticeAwardFields
	if read.Basis != request.Basis || read.Fact.ID != notice.ID || read.Fact.Version != notice.Version ||
		json.Unmarshal(read.Fact.AdditionalProps["notice_type"], &fields.kind) != nil ||
		json.Unmarshal(read.Fact.AdditionalProps["snapshot_ref"], &fields.snapshot) != nil ||
		json.Unmarshal(read.Fact.AdditionalProps["lots"], &fields.lots) != nil || fields.snapshot == "" {
		markUnknown(report, awards, "fact_mismatch")
		return nil
	}
	if fields.kind != "award" {
		return api.ErrInvalidA02Request
	}
	if !b.take(ctx, awards) {
		return nil
	}
	units, err := r.reads.ReadBidResultUnits(ctx, request)
	if err != nil || units == nil || ctx.Err() != nil {
		return failedRead(ctx, report, awards, err)
	}
	if units.Basis != request.Basis {
		markUnknown(report, awards, "fact_mismatch")
		return nil
	}
	if len(units.Items) == 0 {
		markUnknown(report, awards, "award_winner_unavailable")
		return nil
	}

	awardFields := make(map[string]awardField)
	for lotIndex, lot := range fields.lots {
		for resultIndex, result := range lot.Results {
			if _, duplicate := awardFields[result.ID]; duplicate {
				markUnknown(report, awards, "fact_mismatch")
				return nil
			}
			if result.Stage == "award" && result.ID != "" && strings.TrimSpace(result.Bidder.Name) != "" {
				path := fmt.Sprintf("/lots/%d/bid_results/%d/bidder/name_raw", lotIndex, resultIndex)
				awardFields[result.ID] = awardField{path, result.Bidder.Name, result.Bidder.Prov.EvidenceRef}
			}
		}
	}
	for _, unit := range units.Items {
		field, found := awardFields[unit.Subject.PartID]
		if !found || unit.Subject.ID != notice.ID || unit.Subject.Version != notice.Version ||
			unit.Fields.ResultType != dataapi.BidResultPageItemsItemFieldsResultTypeAwarded || unit.Fields.Winner.NameRaw != field.name {
			markUnknown(report, awards, "award_view_mismatch")
			continue
		}
		delete(awardFields, unit.Subject.PartID)
		if field.evidenceRef == "" {
			markUnknown(report, "winner_evidence", "unresolved")
			continue
		}
		if !b.take(ctx, "winner_evidence") {
			break
		}
		proof, err := r.reads.ReadEvidence(ctx, dataapi.EvidenceRequest{EvidenceRef: field.evidenceRef})
		if err != nil || proof == nil || ctx.Err() != nil {
			if err := failedRead(ctx, report, "winner_evidence", err); err != nil {
				return err
			}
			continue
		}
		citation, reason := verifyCitation(proof, fields.snapshot, field)
		if reason != "" {
			markUnknown(report, "winner_evidence", reason)
			continue
		}
		report.Facts = append(report.Facts, api.A02AwardFact{Field: field.path, WinnerName: field.name, Citation: citation})
	}
	return nil
}

func verifyCitation(proof *dataapi.EvidenceRead, snapshot string, field awardField) (api.A02Citation, string) {
	evidence := proof.Evidence
	if proof.Validate() != nil || proof.Kind != dataapi.EvidenceReadKindDoc ||
		evidence.ID != field.evidenceRef || evidence.SnapshotRef != snapshot || evidence.ObservedAt.IsZero() {
		return api.A02Citation{}, "evidence_mismatch"
	}
	if evidence.DisplayLevel == dataapi.EvidenceReadEvidenceDisplayLevelCitationOnly {
		return api.A02Citation{}, "excerpt_access_required"
	}
	locator, located := evidence.Locator.Get()
	quote, quoted := evidence.Snippet.Get()
	if !located || !quoted || strings.TrimSpace(quote) != strings.TrimSpace(field.name) {
		return api.A02Citation{}, "evidence_mismatch"
	}
	return api.A02Citation{
		EvidenceRef: evidence.ID, SnapshotRef: evidence.SnapshotRef, SourceID: evidence.SourceID,
		SourceKey: evidence.SourceKey, ObservedAt: evidence.ObservedAt, Locator: locator, Quote: quote,
	}, ""
}
