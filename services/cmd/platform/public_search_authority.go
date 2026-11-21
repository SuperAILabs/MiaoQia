package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/superailabs/miaoqia/services/contracts"
	dataapi "github.com/superailabs/miaoqia/services/contracts/apis/data-api"
	"github.com/superailabs/miaoqia/services/kernel/dataread"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	searchapi "github.com/superailabs/miaoqia/services/subsystems/search/api"
)

type publicSearchAuthority struct {
	reader    dataread.Reader
	discovery map[string]bool
}

var _ searchapi.PublicAuthority = publicSearchAuthority{}
var publicDiscoverySource = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,95}$`)

func publicDiscoverySources(input string) (map[string]bool, error) {
	var sources []string
	if err := json.Unmarshal([]byte(input), &sources); err != nil || sources == nil {
		return nil, errors.New(publicSearchPrefix + "DISCOVERY_SOURCES requires a JSON source allowlist")
	}
	allowed := make(map[string]bool, len(sources))
	for _, source := range sources {
		tag := "public:" + source
		if !publicDiscoverySource.MatchString(source) || allowed[tag] {
			return nil, errors.New(publicSearchPrefix + "DISCOVERY_SOURCES contains an invalid or duplicate source")
		}
		allowed[tag] = true
	}
	return allowed, nil
}

func publicSearchScope(ctx context.Context) bool {
	scope, ok := reqctx.From(ctx)
	return ok && scope.Validate() == nil && scope.Tenant != "" &&
		scope.Purpose == "interactive" && scope.Principal.Kind == "user"
}

// Public results use the owner's latest confirmed public fact view. Matching
// again there prevents an indexed-only term or filter from disclosing a ref.
func (a publicSearchAuthority) AuthorizePublicSearch(ctx context.Context, query searchapi.PublicQuery, refs []searchapi.PublicRef) ([]searchapi.PublicHit, error) {
	if !publicSearchScope(ctx) {
		return nil, searchapi.ErrPublicDenied
	}
	if a.reader == nil || a.discovery == nil {
		return nil, searchapi.ErrPublicUnavailable
	}

	work, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var workers sync.WaitGroup
	parallelism := min(8, len(refs))
	hits := make([]searchapi.PublicHit, len(refs))
	for offset := range parallelism {
		workers.Go(func() {
			for i := offset; i < len(refs) && work.Err() == nil; i += parallelism {
				hit, err := a.confirmRef(work, query, refs[i])
				if err != nil {
					cancel(err)
					return
				}
				hits[i] = hit
			}
		})
	}
	workers.Wait()
	if err := context.Cause(work); err != nil {
		return nil, err
	}
	allowed := hits[:0]
	for _, hit := range hits {
		if hit.Ref.ID != "" {
			allowed = append(allowed, hit)
		}
	}
	return allowed, nil
}

func (a publicSearchAuthority) confirmRef(ctx context.Context, q searchapi.PublicQuery, ref searchapi.PublicRef) (searchapi.PublicHit, error) {
	var empty searchapi.PublicHit
	if (q.Kind != "" && q.Kind != ref.Kind) || (q.Domain != "" && q.Domain != ref.Domain) {
		return empty, nil
	}
	if chunk, ok := publicChunkRequest(ref); ok {
		return a.confirmChunk(ctx, q, ref, chunk)
	}
	if entity, ok := publicOrganizationRequest(ref); ok {
		return a.confirmOrganization(ctx, q, ref, entity)
	}
	if fact, ok := publicFactRequest(ref); ok {
		allowed, err := a.confirmCandidate(ctx, q, ref, fact)
		if allowed && err == nil {
			return searchapi.PublicHit{Ref: ref}, nil
		}
		return empty, err
	}
	return empty, nil
}

var publicChunkID = regexp.MustCompile(`^(ntc_[0-9A-HJKMNP-TV-Z]{26})~(0[0-9]|1[01])$`)
var publicOrganizationID = regexp.MustCompile(`^org_[0-9A-HJKMNP-TV-Z]{26}$`)

func publicVersion(value string) (int64, bool) {
	version, err := strconv.ParseInt(value, 10, 64)
	return version, err == nil && version > 0 && strconv.FormatInt(version, 10) == value
}

func publicChunkRequest(ref searchapi.PublicRef) (dataapi.NoticeChunkRequest, bool) {
	if ref.Kind != searchapi.PublicChunk || ref.Domain != "INF-PROC" {
		return dataapi.NoticeChunkRequest{}, false
	}
	id := publicChunkID.FindStringSubmatch(ref.ID)
	version, valid := publicVersion(ref.Version)
	if id == nil || !valid {
		return dataapi.NoticeChunkRequest{}, false
	}
	ordinal, _ := strconv.ParseInt(id[2], 10, 64)
	input := dataapi.NoticeChunkRequest{FactID: id[1], Version: version, Ordinal: ordinal}
	return input, input.Validate() == nil
}

func publicOrganizationRequest(ref searchapi.PublicRef) (dataapi.PublicOrganizationRequest, bool) {
	version, valid := publicVersion(ref.Version)
	if ref.Kind != searchapi.PublicEntity || ref.Domain != "INF-ORG" || !valid || !publicOrganizationID.MatchString(ref.ID) {
		return dataapi.PublicOrganizationRequest{}, false
	}
	request := dataapi.PublicOrganizationRequest{OrgID: ref.ID, Version: version}
	return request, request.Validate() == nil
}

func (a publicSearchAuthority) confirmOrganization(ctx context.Context, q searchapi.PublicQuery, ref searchapi.PublicRef, request dataapi.PublicOrganizationRequest) (searchapi.PublicHit, error) {
	var hit searchapi.PublicHit
	if q.Region != "" || q.NoticeType != "" || q.From != nil || q.Before != nil {
		return hit, nil
	}
	reader, ok := a.reader.(dataread.PublicOrganizationReader)
	if !ok {
		return hit, searchapi.ErrPublicUnavailable
	}
	value, err := reader.ReadPublicOrganization(ctx, request)
	if value == nil || err != nil {
		return hit, publicReaderError(err)
	}
	if value.OrgID != request.OrgID || value.Version != request.Version {
		return hit, searchapi.ErrPublicUnavailable
	}
	if !a.discoveryAllows(value.RightsTags) ||
		(q.Identifier != "" && q.Identifier != ref.ID && q.Identifier != value.Uscc) ||
		(q.Text != "" && !strings.Contains(strings.ToLower(value.Name), strings.ToLower(q.Text))) {
		return hit, nil
	}
	return searchapi.PublicHit{Ref: ref, Title: value.Name}, nil
}

func (a publicSearchAuthority) discoveryAllows(tags []string) bool {
	for _, tag := range tags {
		if !a.discovery[tag] {
			return false
		}
	}
	return len(tags) > 0
}

func (a publicSearchAuthority) confirmChunk(ctx context.Context, q searchapi.PublicQuery, ref searchapi.PublicRef, request dataapi.NoticeChunkRequest) (searchapi.PublicHit, error) {
	var hit searchapi.PublicHit
	reader, ok := a.reader.(dataread.NoticeChunkReader)
	if !ok {
		return hit, searchapi.ErrPublicUnavailable
	}
	value, err := reader.ReadNoticeChunk(ctx, request)
	if value == nil || err != nil {
		return hit, publicReaderError(err)
	}
	if value.FactID != request.FactID || value.Version != request.Version || value.Ordinal != request.Ordinal {
		return hit, searchapi.ErrPublicUnavailable
	}
	if !a.discoveryAllows(value.RightsTags) ||
		(q.Identifier != "" && q.Identifier != ref.ID && !slices.Contains(value.Metadata.Identifiers, q.Identifier)) ||
		(q.Region != "" && q.Region != value.Metadata.Region.Or("")) ||
		(q.NoticeType != "" && q.NoticeType != value.Metadata.NoticeType.Or("")) ||
		(q.Text != "" && !strings.Contains(strings.ToLower(value.Text), strings.ToLower(q.Text))) {
		return hit, nil
	}
	date, dated := value.Metadata.PublishedAt.Get()
	if (q.From != nil && (!dated || date.Before(*q.From))) || (q.Before != nil && (!dated || !date.Before(*q.Before))) {
		return hit, nil
	}
	locator := searchapi.ChunkLocator{Block: value.Locator.Block, CharStart: value.Locator.CharStart, CharEnd: value.Locator.CharEnd}
	if row, rowSet := value.Locator.Row.Get(); rowSet {
		col, _ := value.Locator.Col.Get()
		locator.Row, locator.Col = &row, &col
	}
	return searchapi.PublicHit{Ref: ref, SnapshotRef: value.SnapshotRef, Locator: &locator, Text: value.Text}, nil
}

func publicFactRequest(ref searchapi.PublicRef) (dataapi.FactRequest, bool) {
	if ref.Domain != "INF-PROC" ||
		!((ref.Kind == searchapi.PublicNotice && strings.HasPrefix(ref.ID, "ntc_")) ||
			(ref.Kind == searchapi.PublicFact && strings.HasPrefix(ref.ID, "pp_"))) {
		return dataapi.FactRequest{}, false
	}
	version, valid := publicVersion(ref.Version)
	if !valid {
		return dataapi.FactRequest{}, false
	}
	request := dataapi.FactRequest{FactID: ref.ID, Basis: dataapi.Basis{View: dataapi.BasisViewCurrent}}
	request.Basis.RecordedCutoff.SetToNull()
	request.Version.SetTo(version)
	return request, request.Validate() == nil
}

func (a publicSearchAuthority) confirmCandidate(ctx context.Context, query searchapi.PublicQuery, ref searchapi.PublicRef, request dataapi.FactRequest) (bool, error) {
	version, _ := request.Version.Get()
	fixed, err := a.reader.ReadFact(ctx, request)
	if err != nil || fixed == nil {
		return false, publicReaderError(err)
	}
	request.Version.SetToNull()
	current, err := a.reader.ReadFact(ctx, request)
	if err != nil || current == nil {
		return false, publicReaderError(err)
	}
	if fixed.Fact.ID != ref.ID || current.Fact.ID != ref.ID {
		return false, searchapi.ErrPublicUnavailable
	}
	if fixed.Fact.Version != version {
		return false, searchapi.ErrPublicUnavailable
	}
	if current.Fact.Version != version {
		return false, nil
	}

	fixedFields, err := publicDiscoveryFields(fixed.Fact)
	if err != nil {
		return false, err
	}
	currentFields, err := publicDiscoveryFields(current.Fact)
	if err != nil {
		return false, err
	}
	return fixedFields.matches(query, ref, a.discovery) && currentFields.matches(query, ref, a.discovery), nil
}

func publicReaderError(err error) error {
	if err == nil {
		return nil // A confirmed null means there is no readable owner candidate.
	}
	var response *dataread.Error
	if errors.As(err, &response) && response.Status == http.StatusForbidden {
		// The current owner returns the same 403 for membership, delegation,
		// and source-rights denial. Withhold the whole page on that ambiguity.
		return searchapi.ErrPublicDenied
	}
	return searchapi.ErrPublicUnavailable
}

type publicFactFields struct {
	Domain         string                                 `json:"info_domain"`
	Classification contracts.PublicFactMetaClassification `json:"classification"`
	RightsTags     []string                               `json:"rights_tags"`
	Title          contracts.Text                         `json:"title"`
	NoticeCode     contracts.Text                         `json:"notice_code"`
	ProjectCode    contracts.Text                         `json:"project_code"`
	Region         contracts.Region                       `json:"location"`
	NoticeType     contracts.NoticeType                   `json:"notice_type"`
	PublishedAt    *contracts.Instant                     `json:"published_at"`
}

func publicDiscoveryFields(fact dataapi.Fact) (publicFactFields, error) {
	var fields publicFactFields
	body, err := json.Marshal(&fact)
	if err != nil || json.Unmarshal(body, &fields) != nil || fields.Domain == "" ||
		fields.Classification == "" || len(fields.RightsTags) == 0 {
		return fields, searchapi.ErrPublicUnavailable
	}
	return fields, nil
}

func (f publicFactFields) matches(q searchapi.PublicQuery, ref searchapi.PublicRef, discovery map[string]bool) bool {
	if f.Classification != contracts.PublicFactMetaClassificationPublic || f.Domain != ref.Domain ||
		(q.Kind != "" && q.Kind != ref.Kind) || (q.Domain != "" && q.Domain != ref.Domain) {
		return false
	}
	for _, tag := range f.RightsTags {
		if !discovery[tag] {
			return false
		}
	}
	if q.Identifier != "" && q.Identifier != ref.ID &&
		q.Identifier != f.NoticeCode.Value && q.Identifier != f.ProjectCode.Value {
		return false
	}
	if (q.Region != "" && q.Region != f.Region.Code) ||
		(q.NoticeType != "" && q.NoticeType != string(f.NoticeType)) {
		return false
	}
	if q.From != nil || q.Before != nil {
		if f.PublishedAt == nil || f.PublishedAt.Value.IsZero() ||
			(q.From != nil && f.PublishedAt.Value.Before(*q.From)) ||
			(q.Before != nil && !f.PublishedAt.Value.Before(*q.Before)) {
			return false
		}
	}
	return q.Text == "" || strings.Contains(strings.ToLower(f.Title.Value), strings.ToLower(q.Text))
}
