package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/search"
	searchapi "github.com/superailabs/miaoqia/services/subsystems/search/api"
)

const publicSearchRoute = "POST /v1/search"

func registerPublicSearchRoutes(env *runtime.Env, public, _ *http.ServeMux) {
	searcher, _ := runtime.Lookup[searchapi.PublicSearcher](env.Bindings, searchapi.PublicSearchBinding)
	public.Handle(publicSearchRoute, publicSearchHandler(searcher))
}

func publicSearchHandler(searcher searchapi.PublicSearcher) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !publicSearchScope(r.Context()) {
			writePlatformProblem(w, http.StatusForbidden, "search_denied")
			return
		}
		contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || contentType != "application/json" {
			writePlatformProblem(w, http.StatusUnsupportedMediaType, "invalid_content_type")
			return
		}
		var input struct {
			Query    *searchapi.PublicQuery `json:"query"`
			PageSize int                    `json:"page_size"`
			Cursor   string                 `json:"cursor"`
		}
		body := http.MaxBytesReader(w, r.Body, 16<<10)
		if err := decodeStrictJSON(body, &input); err != nil || input.Query == nil {
			writePlatformProblem(w, http.StatusBadRequest, "invalid_search_request")
			return
		}
		if searcher == nil {
			writePlatformProblem(w, http.StatusServiceUnavailable, "search_unavailable")
			return
		}
		page, err := searcher.Search(r.Context(), searchapi.PublicSearchRequest{
			Query: *input.Query, PageSize: input.PageSize, Cursor: input.Cursor,
		})
		switch {
		case errors.Is(err, searchapi.ErrPublicRequest):
			writePlatformProblem(w, http.StatusBadRequest, "invalid_search_request")
		case errors.Is(err, searchapi.ErrPublicCursor):
			writePlatformProblem(w, http.StatusBadRequest, "invalid_search_cursor")
		case errors.Is(err, searchapi.ErrPublicDenied):
			writePlatformProblem(w, http.StatusForbidden, "search_denied")
		case err != nil:
			writePlatformProblem(w, http.StatusServiceUnavailable, "search_unavailable")
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(page)
		}
	})
}

func writePlatformProblem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "https://errors.miaoqia.com/" + code, "title": code, "status": status, "code": code,
	})
}

func decodeStrictJSON(body io.Reader, target any) error {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("expected a single JSON value")
	}
	return nil
}

type publicProjectionSource struct {
	endpoint string
	client   *http.Client
}

var _ search.PublicProjectionSource = (*publicProjectionSource)(nil)
var publicProjectionFactID = regexp.MustCompile(`^(ntc|pp)_[0-9A-HJKMNP-TV-Z]{26}$`)
var publicProjectionOrgCursor = regexp.MustCompile(`^~org:(org_[0-9A-HJKMNP-TV-Z]{26})?$`)

const publicOrgPhase = "~org:"

func validProjectionCursor(cursor string) bool {
	return cursor == "" || publicProjectionFactID.MatchString(cursor) || publicProjectionOrgCursor.MatchString(cursor)
}

func (s *publicProjectionSource) LoadPublicMutationPage(ctx context.Context, event outbox.Envelope, after string) (search.PublicMutationPage, error) {
	var page search.PublicMutationPage
	if event.Zone != reqctx.ZonePublic || event.Validate() != nil {
		return page, errors.New("public search owner export requires a public event")
	}
	var input any = event
	if after != "" {
		input = struct {
			Event outbox.Envelope `json:"event"`
			After string          `json:"after"`
		}{event, after}
	}
	body, err := json.Marshal(input)
	if err != nil {
		return page, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return page, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return page, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return page, fmt.Errorf("public search owner export returned HTTP %d", response.StatusCode)
	}
	const maxBody = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		return page, errors.New("public search owner export response exceeds its read boundary")
	}
	return decodePublicMutationPage(bytes.NewReader(raw), after)
}

func decodePublicMutationPage(body io.Reader, after string) (search.PublicMutationPage, error) {
	var page search.PublicMutationPage
	var payload struct {
		NextAfter string `json:"next_after"`
		Mutations []struct {
			Ref         searchapi.PublicRef `json:"ref"`
			Revision    int64               `json:"revision"`
			Withdrawn   *bool               `json:"withdrawn"`
			Identifiers []string            `json:"identifiers"`
			Region      string              `json:"region"`
			NoticeType  string              `json:"notice_type"`
			PublishedAt *time.Time          `json:"published_at"`
			Title       string              `json:"title"`
			Text        string              `json:"text"`
		} `json:"mutations"`
	}
	if err := decodeStrictJSON(body, &payload); err != nil || payload.Mutations == nil || len(payload.Mutations) > 32 ||
		!validProjectionCursor(after) || !validProjectionCursor(payload.NextAfter) ||
		(payload.NextAfter != "" && payload.NextAfter <= after) {
		return page, errors.New("public search owner export returned an invalid mutation batch")
	}
	orgPhase := strings.HasPrefix(after, publicOrgPhase) ||
		(strings.HasPrefix(payload.NextAfter, publicOrgPhase) && payload.NextAfter != publicOrgPhase)
	if orgPhase && (publicProjectionFactID.MatchString(after) || publicProjectionFactID.MatchString(payload.NextAfter)) {
		return page, errors.New("public search owner export returned an invalid owner continuation")
	}
	previous := strings.TrimPrefix(after, publicOrgPhase)
	bound := strings.TrimPrefix(payload.NextAfter, publicOrgPhase)
	mutations := make([]search.PublicMutation, 0, len(payload.Mutations))
	lastID := previous
	for _, row := range payload.Mutations {
		_, fact := publicFactRequest(row.Ref)
		chunk, isChunk := publicChunkRequest(row.Ref)
		_, org := publicOrganizationRequest(row.Ref)
		parent := row.Ref.ID
		if isChunk {
			parent = chunk.FactID
		}
		if (!fact && !isChunk && !org) || (orgPhase && !org) ||
			(org && !orgPhase && (after != "" || payload.NextAfter != "")) ||
			row.Revision < 1 || row.Withdrawn == nil ||
			(!isChunk && row.Text != "") ||
			(isChunk && !*row.Withdrawn && (row.Text == "" || utf8.RuneCountInString(row.Text) > 768)) ||
			(org && !*row.Withdrawn && (len(row.Identifiers) != 2 || row.Identifiers[0] != row.Ref.ID ||
				len(row.Identifiers[1]) != 18 || strings.TrimSpace(row.Title) == "" ||
				row.Region != "" || row.NoticeType != "" || row.PublishedAt != nil)) ||
			(*row.Withdrawn && (len(row.Identifiers) != 0 || row.Region != "" || row.NoticeType != "" ||
				row.PublishedAt != nil || row.Title != "" || row.Text != "")) {
			return page, errors.New("public search owner export returned an invalid mutation")
		}
		if parent <= previous || row.Ref.ID <= lastID || (bound != "" && parent > bound) {
			return page, errors.New("public search owner export returned an unordered mutation")
		}
		lastID = row.Ref.ID
		mutations = append(mutations, search.PublicMutation{
			Ref: row.Ref, Revision: row.Revision, Withdrawn: *row.Withdrawn, Identifiers: row.Identifiers,
			Region: row.Region, NoticeType: row.NoticeType, PublishedAt: row.PublishedAt, Title: row.Title, Text: row.Text,
		})
	}
	return search.PublicMutationPage{Mutations: mutations, NextAfter: payload.NextAfter}, nil
}
