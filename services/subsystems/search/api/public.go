package api

import (
	"context"
	"errors"
	"time"
)

var (
	ErrPublicRequest     = errors.New("search: invalid public search request")
	ErrPublicCursor      = errors.New("search: invalid or expired public search cursor")
	ErrPublicDenied      = errors.New("search: public search denied")
	ErrPublicUnavailable = errors.New("search: public search unavailable")
)

type PublicKind string

const (
	PublicNotice PublicKind = "notice"
	PublicChunk  PublicKind = "chunk"
	PublicFact   PublicKind = "fact"
	PublicEntity PublicKind = "entity"
)

// PublicRef identifies a fixed owner version, including a fixed chunk when Kind is PublicChunk.
type PublicRef struct {
	Kind    PublicKind `json:"kind"`
	Domain  string     `json:"domain"`
	ID      string     `json:"id"`
	Version string     `json:"version"`
}

type PublicQuery struct {
	Text       string     `json:"text,omitempty"`
	Identifier string     `json:"identifier,omitempty"`
	Kind       PublicKind `json:"kind,omitempty"`
	Domain     string     `json:"domain,omitempty"`
	Region     string     `json:"region,omitempty"`
	NoticeType string     `json:"notice_type,omitempty"`
	From       *time.Time `json:"from,omitempty"`
	Before     *time.Time `json:"before,omitempty"`
}

type PublicSearchRequest struct {
	Query    PublicQuery `json:"query"`
	PageSize int         `json:"page_size"`
	Cursor   string      `json:"cursor,omitempty"`
}

// PublicHit contains only fields produced by the authority for this request.
// Excerpt and Text must satisfy the current display rights for the exact version.
type PublicHit struct {
	Ref         PublicRef     `json:"ref"`
	Title       string        `json:"title,omitempty"`
	Excerpt     string        `json:"excerpt,omitempty"`
	Text        string        `json:"text,omitempty"`
	EvidenceRef string        `json:"evidence_ref,omitempty"`
	SnapshotRef string        `json:"snapshot_ref,omitempty"`
	Locator     *ChunkLocator `json:"locator,omitempty"`
}

type ChunkLocator struct {
	Block     int64  `json:"block"`
	Row       *int64 `json:"row,omitempty"`
	Col       *int64 `json:"col,omitempty"`
	CharStart int64  `json:"char_start"`
	CharEnd   int64  `json:"char_end"`
}

type PublicSearchPage struct {
	Hits      []PublicHit `json:"hits"`
	IndexedAt time.Time   `json:"indexed_as_of"`
	// A cursor requires an authorized lookahead within the candidate window.
	// An absent cursor does not establish exhaustive coverage of the index.
	NextCursor string `json:"next_cursor,omitempty"`
}

type PublicSearcher interface {
	Search(context.Context, PublicSearchRequest) (PublicSearchPage, error)
}

const PublicSearchBinding = "search.PublicSearcher"

// PublicAuthority checks the authenticated reqctx scope on every call, even
// when refs is empty. It verifies current discovery and display rights for the
// exact owner versions under their trusted source closure. A match can reveal
// indexed terms or structured values, so discoverability must cover the queried
// fields or be established by matching the allowed owner view. The authority
// omits inaccessible, expired, withdrawn, merged-away and superseded candidates.
// Incomplete or unavailable verification fails the batch. ErrPublicDenied is
// reserved for request-level admission failures.
type PublicAuthority interface {
	AuthorizePublicSearch(context.Context, PublicQuery, []PublicRef) ([]PublicHit, error)
}
