package search

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/search/api"
)

const (
	publicQueryTimeout = 8 * time.Second
	publicCursorTTL    = 55 * time.Second
	maxPublicCursor    = 16384
)

type publicCandidate struct {
	Ref  api.PublicRef
	Sort []json.RawMessage
}

// The OpenSearch hit allows only ref to be read from _source.
func (s *PublicOpenSearch) find(ctx context.Context, pit string, q api.PublicQuery, after []json.RawMessage) ([]publicCandidate, error) {
	filters := []any{term("withdrawn", false)}
	for _, field := range []struct{ name, value string }{
		{"ref.kind", string(q.Kind)}, {"ref.domain", q.Domain}, {"region", q.Region},
		{"notice_type", q.NoticeType}, {"identifiers", q.Identifier},
	} {
		if field.value != "" {
			filters = append(filters, term(field.name, field.value))
		}
	}
	dates := map[string]time.Time{}
	if q.From != nil {
		dates["gte"] = q.From.UTC()
	}
	if q.Before != nil {
		dates["lt"] = q.Before.UTC()
	}
	if len(dates) != 0 {
		filters = append(filters, map[string]any{"range": map[string]any{"published_at": dates}})
	}
	query := map[string]any{"filter": filters}
	if q.Text != "" {
		query["must"] = map[string]any{"multi_match": map[string]any{
			"query": q.Text, "fields": []string{"title^2", "text"}, "operator": "and",
		}}
	}
	request := map[string]any{
		"pit":   map[string]string{"id": pit, "keep_alive": publicPITLifetime},
		"query": map[string]any{"bool": query},
		"size":  maxPublicCandidates, "_source": []string{"ref"},
		"track_total_hits": false,
		"sort":             []any{map[string]string{"_score": "desc"}, map[string]string{"key": "asc"}},
	}
	if len(after) != 0 {
		request["search_after"] = after
	}
	var result struct {
		TimedOut bool `json:"timed_out"`
		Shards   struct {
			Failed int `json:"failed"`
		} `json:"_shards"`
		Hits *struct {
			Hits []struct {
				Source struct {
					Ref api.PublicRef `json:"ref"`
				} `json:"_source"`
				Sort []json.RawMessage `json:"sort"`
			} `json:"hits"`
		} `json:"hits"`
	}
	status, err := s.call(ctx, http.MethodPost, "/_search", url.Values{"allow_partial_search_results": {"false"}}, request, &result)
	if status == http.StatusNotFound {
		return nil, errPublicPIT
	}
	if err != nil {
		return nil, err
	}
	if result.TimedOut || result.Shards.Failed != 0 || result.Hits == nil || result.Hits.Hits == nil || len(result.Hits.Hits) > maxPublicCandidates {
		return nil, errors.New("search: incomplete public candidate response")
	}
	candidates := make([]publicCandidate, 0, len(result.Hits.Hits))
	seen := make(map[string]bool, len(result.Hits.Hits))
	for _, hit := range result.Hits.Hits {
		ref := hit.Source.Ref
		key := publicKey(ref)
		if !validPublicRef(ref) || !validPublicSort(hit.Sort, key) || seen[key] {
			return nil, errors.New("search: invalid public candidate")
		}
		seen[key] = true
		candidates = append(candidates, publicCandidate{Ref: ref, Sort: hit.Sort})
	}
	return candidates, nil
}

func validPublicSort(sort []json.RawMessage, key string) bool {
	if len(sort) != 2 {
		return false
	}
	var score *float64
	var sortedKey string
	return json.Unmarshal(sort[0], &score) == nil && score != nil &&
		json.Unmarshal(sort[1], &sortedKey) == nil && sortedKey == key
}

func term(field string, value any) map[string]any {
	return map[string]any{"term": map[string]any{field: value}}
}

func (s *PublicOpenSearch) openPIT(ctx context.Context, generation string) (string, error) {
	var result struct {
		PIT    string `json:"pit_id"`
		Shards struct {
			Failed int `json:"failed"`
		} `json:"_shards"`
	}
	_, err := s.call(ctx, http.MethodPost, "/"+s.prefix+generation+"/_search/point_in_time",
		url.Values{"keep_alive": {publicPITLifetime}, "allow_partial_pit_creation": {"false"}}, nil, &result)
	if err == nil && (result.PIT == "" || len(result.PIT) > maxPublicCursor/2 || result.Shards.Failed != 0) {
		err = errors.New("search: invalid public snapshot")
	}
	return result.PIT, err
}

type publicSearcher struct {
	index     *PublicOpenSearch
	authority api.PublicAuthority
	codec     cipher.AEAD
}

type publicPageToken struct {
	BoundTo    [32]byte
	Generation string
	Activation string
	Expires    time.Time
	PIT        string
	After      []json.RawMessage
}

func NewPublicSearcher(index *PublicOpenSearch, authority api.PublicAuthority, cursorKey []byte) (api.PublicSearcher, error) {
	if index == nil || authority == nil || len(cursorKey) != 32 {
		return nil, errors.New("search: public query requires OpenSearch, an authority, and a 32-byte secret cursor key")
	}
	block, err := aes.NewCipher(cursorKey)
	if err != nil {
		return nil, err
	}
	codec, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return &publicSearcher{index: index, authority: authority, codec: codec}, nil
}

func (s *publicSearcher) Search(ctx context.Context, request api.PublicSearchRequest) (api.PublicSearchPage, error) {
	ctx, cancel := context.WithTimeout(ctx, publicQueryTimeout)
	defer cancel()
	scope, ok := reqctx.From(ctx)
	if !ok || scope.Validate() != nil || scope.Tenant == "" || scope.Purpose == "" ||
		(scope.Principal.Kind != "user" && scope.Principal.Kind != "service") {
		return api.PublicSearchPage{}, api.ErrPublicDenied
	}
	request.Query.Text = strings.TrimSpace(request.Query.Text)
	if !validPublicQuery(request) {
		return api.PublicSearchPage{}, api.ErrPublicRequest
	}
	binding, err := json.Marshal(struct {
		Query api.PublicQuery
		Size  int
		Scope reqctx.Context
	}{request.Query, request.PageSize, scope})
	if err != nil {
		return api.PublicSearchPage{}, api.ErrPublicRequest
	}
	fingerprint := sha256.Sum256(binding)
	token := publicPageToken{BoundTo: fingerprint, Expires: time.Now().Add(publicCursorTTL).UTC()}
	if request.Cursor != "" {
		token, err = s.decode(request.Cursor)
		if err != nil || token.BoundTo != fingerprint || !time.Now().Before(token.Expires) {
			return api.PublicSearchPage{}, api.ErrPublicCursor
		}
	}
	current, err := s.index.Current(ctx)
	if err != nil {
		return api.PublicSearchPage{}, api.ErrPublicUnavailable
	}
	if request.Cursor != "" && (token.Generation != current.Generation || token.Activation != current.Activation) {
		return api.PublicSearchPage{}, api.ErrPublicCursor
	}

	if request.Cursor == "" {
		token.Generation, token.Activation = current.Generation, current.Activation
		token.PIT, err = s.index.openPIT(ctx, current.Generation)
		if err != nil {
			return api.PublicSearchPage{}, api.ErrPublicUnavailable
		}
	}
	candidates, err := s.index.find(ctx, token.PIT, request.Query, token.After)
	if request.Cursor != "" && errors.Is(err, errPublicPIT) {
		return api.PublicSearchPage{}, api.ErrPublicCursor
	}
	if err != nil {
		return api.PublicSearchPage{}, api.ErrPublicUnavailable
	}
	// A changed alias stales the page before the final authorization call.
	recheck, err := s.index.Current(ctx)
	if err != nil {
		return api.PublicSearchPage{}, api.ErrPublicUnavailable
	}
	if recheck != current {
		if request.Cursor != "" {
			return api.PublicSearchPage{}, api.ErrPublicCursor
		}
		return api.PublicSearchPage{}, api.ErrPublicUnavailable
	}
	return s.authorizedPage(ctx, request, token, current, candidates)
}

func validPublicQuery(request api.PublicSearchRequest) bool {
	q := request.Query
	if request.PageSize < 1 || request.PageSize > 50 || len(request.Cursor) > maxPublicCursor ||
		(q.Kind != "" && !validPublicKind(q.Kind)) ||
		(q.From != nil && q.Before != nil && !q.From.Before(*q.Before)) {
		return false
	}
	for _, value := range []string{q.Text, q.Identifier, q.Domain, q.Region, q.NoticeType} {
		if len(value) > 512 || !utf8.ValidString(value) {
			return false
		}
	}
	return true
}

func (s *publicSearcher) authorizedPage(ctx context.Context, request api.PublicSearchRequest, token publicPageToken, current PublicPublication, candidates []publicCandidate) (api.PublicSearchPage, error) {
	refs := make([]api.PublicRef, len(candidates))
	allowed := make(map[api.PublicRef]*api.PublicHit, len(refs))
	for i, candidate := range candidates {
		refs[i] = candidate.Ref
		allowed[candidate.Ref] = nil
	}
	verified, err := s.authority.AuthorizePublicSearch(ctx, request.Query, refs)
	if errors.Is(err, api.ErrPublicDenied) {
		return api.PublicSearchPage{}, api.ErrPublicDenied
	}
	if err != nil || ctx.Err() != nil {
		return api.PublicSearchPage{}, api.ErrPublicUnavailable
	}
	if !time.Now().Before(token.Expires) {
		return api.PublicSearchPage{}, api.ErrPublicCursor
	}
	for _, hit := range verified {
		if prior, inBatch := allowed[hit.Ref]; !inBatch || prior != nil {
			return api.PublicSearchPage{}, api.ErrPublicUnavailable
		}
		allowed[hit.Ref] = &hit
	}
	page := api.PublicSearchPage{Hits: make([]api.PublicHit, 0, request.PageSize), IndexedAt: current.IndexedAt}
	for _, candidate := range candidates {
		hit := allowed[candidate.Ref]
		if hit == nil {
			continue
		}
		if len(page.Hits) == request.PageSize {
			// The next request checks this lookahead again under current rights.
			page.NextCursor, err = s.encode(token)
			if err != nil {
				return api.PublicSearchPage{}, api.ErrPublicUnavailable
			}
			break
		}
		page.Hits = append(page.Hits, *hit)
		token.After = candidate.Sort
	}
	return page, nil
}

var publicCursorAAD = []byte("miaoqia/public-search/cursor/v1")

func (s *publicSearcher) encode(token publicPageToken) (string, error) {
	raw, err := json.Marshal(token)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(s.codec.Seal(nil, nil, raw, publicCursorAAD))
	if len(encoded) > maxPublicCursor {
		return "", errors.New("search: oversized public cursor")
	}
	return encoded, nil
}

func (s *publicSearcher) decode(encoded string) (publicPageToken, error) {
	var token publicPageToken
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return token, api.ErrPublicCursor
	}
	plain, err := s.codec.Open(nil, nil, raw, publicCursorAAD)
	if err != nil {
		return token, err
	}
	err = json.Unmarshal(plain, &token)
	return token, err
}
