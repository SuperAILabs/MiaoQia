package api

import (
	"context"
	"errors"
	"time"

	dataapi "github.com/superailabs/miaoqia/services/contracts/apis/data-api"
	ai "github.com/superailabs/miaoqia/services/subsystems/aiplatform/api"
)

const A02Template = "research.a02-award@1"

var (
	ErrInvalidA02Request       = errors.New("research A02: invalid notice reference or budget")
	ErrA02AccessDenied         = errors.New("research A02: access denied")
	ErrA02AdmissionUnavailable = errors.New("research A02: admission unavailable")
)

// A02Request pins an award notice, including when a newer version exists.
// The trusted caller supplies the budget after applying tenant limits.
// Identity and purpose come exclusively from the authenticated request context.
type A02Request struct {
	Notice NoticeVersion
	Budget A02Budget
}

type NoticeVersion struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

// Reads counts logical owner calls. ModelCalls is 0 or 1. Deadline is absolute.
type A02Budget struct {
	Reads      int
	ModelCalls int
	Deadline   time.Time
}

type A02Report struct {
	Template        string              `json:"template"`
	Notice          NoticeVersion       `json:"notice"`
	Facts           []A02AwardFact      `json:"facts"`
	Inferences      []string            `json:"inferences"`
	Unknowns        []A02Unknown        `json:"unknowns"`
	Recommendations []A02Recommendation `json:"recommendations"`
	Partial         bool                `json:"partial"`
	ReadsUsed       int                 `json:"reads_used"`
	ModelCallsUsed  int                 `json:"model_calls_used"`
	Model           *ai.ModelRef        `json:"model,omitempty"`
	InvocationID    string              `json:"invocation_id,omitempty"`
}

// A02AwardFact records the name the award notice disclosed at Field, a JSON pointer.
// Citation carries the owner-resolved, display-authorized excerpt and locator.
type A02AwardFact struct {
	Field      string      `json:"field"`
	WinnerName string      `json:"winner_name"`
	Citation   A02Citation `json:"citation"`
}

type A02Citation struct {
	EvidenceRef string                              `json:"evidence_ref"`
	SnapshotRef string                              `json:"snapshot_ref"`
	SourceID    string                              `json:"source_id"`
	SourceKey   string                              `json:"source_key"`
	ObservedAt  time.Time                           `json:"observed_at"`
	Locator     dataapi.EvidenceReadEvidenceLocator `json:"locator"`
	Quote       string                              `json:"quote"`
}

type A02Unknown struct {
	Topic  string `json:"topic"`
	Reason string `json:"reason"`
}

type A02Recommendation string

const (
	VerifyEntity       A02Recommendation = "verify_entity_identity"
	VerifyPrimeRole    A02Recommendation = "verify_contract_role"
	VerifySupplyWindow A02Recommendation = "verify_supply_window"
)

// A02Runner handles synchronous reads within an authenticated Cell request.
type A02Runner interface {
	RunA02(context.Context, A02Request) (A02Report, error)
}
