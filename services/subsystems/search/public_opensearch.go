package search

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/superailabs/miaoqia/services/subsystems/search/api"
)

const (
	maxPublicIndexResponse = 1 << 20
	maxPublicCandidates    = 256
	publicPITLifetime      = "60s"
)

var (
	publicNamespace = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	generationID    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	errPublicPIT    = errors.New("search: public search snapshot expired")
)

// ErrPublicIndexUnpublished indicates that the configured alias has no target.
var ErrPublicIndexUnpublished = errors.New("search: public index has no active alias")

// PublicOpenSearch is scoped to one dedicated public index namespace. The caller
// supplies the role-specific authenticated transport and its OpenSearch endpoint.
type PublicOpenSearch struct {
	base   *url.URL
	client http.Client
	prefix string
	alias  string
}

func NewPublicOpenSearch(endpoint, namespace string, client *http.Client) (*PublicOpenSearch, error) {
	base, err := url.Parse(endpoint)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		(base.Scheme != "https" && base.Scheme != "http") || client == nil || !publicNamespace.MatchString(namespace) {
		return nil, errors.New("search: invalid OpenSearch configuration")
	}
	prefix := "mq-public-" + namespace + "-"
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &PublicOpenSearch{base: base, client: copy, prefix: prefix, alias: prefix + "current"}, nil
}

// PublicMutation uses an owner-supplied strictly monotone revision per kind,
// domain and stable ID. Version identifies the exact owner record; Revision is
// its ordering token for the projection, including withdrawals and merges.
type PublicMutation struct {
	Ref         api.PublicRef
	Revision    int64
	Withdrawn   bool
	Identifiers []string
	Region      string
	NoticeType  string
	PublishedAt *time.Time
	Title       string
	Text        string
}

type publicDocument struct {
	Ref         api.PublicRef `json:"ref"`
	Key         string        `json:"key"`
	Withdrawn   bool          `json:"withdrawn"`
	Identifiers []string      `json:"identifiers,omitempty"`
	Region      string        `json:"region,omitempty"`
	NoticeType  string        `json:"notice_type,omitempty"`
	PublishedAt *time.Time    `json:"published_at,omitempty"`
	Title       string        `json:"title,omitempty"`
	Text        string        `json:"text,omitempty"`
	Digest      string        `json:"digest,omitempty"`
}

func validPublicKind(kind api.PublicKind) bool {
	switch kind {
	case api.PublicNotice, api.PublicChunk, api.PublicFact, api.PublicEntity:
		return true
	default:
		return false
	}
}

func validPublicRef(ref api.PublicRef) bool {
	return validPublicKind(ref.Kind) && ref.Domain != "" && len(ref.Domain) <= 128 &&
		ref.ID != "" && len(ref.ID) <= 512 && ref.Version != "" && len(ref.Version) <= 256 &&
		utf8.ValidString(ref.Domain) && utf8.ValidString(ref.ID) && utf8.ValidString(ref.Version)
}

func publicKey(ref api.PublicRef) string {
	basis, _ := json.Marshal([]string{string(ref.Kind), ref.Domain, ref.ID})
	sum := sha256.Sum256(basis)
	return hex.EncodeToString(sum[:])
}

func (m PublicMutation) document() (publicDocument, error) {
	if !validPublicRef(m.Ref) || m.Revision < 1 {
		return publicDocument{}, errors.New("search: projection requires a fixed reference and positive revision")
	}
	doc := publicDocument{Ref: m.Ref, Key: publicKey(m.Ref), Withdrawn: m.Withdrawn}
	if !m.Withdrawn {
		doc.Identifiers = m.Identifiers
		doc.Region = m.Region
		doc.NoticeType = m.NoticeType
		doc.PublishedAt = m.PublishedAt
		doc.Title = m.Title
		doc.Text = m.Text
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return publicDocument{}, err
	}
	sum := sha256.Sum256(body)
	doc.Digest = hex.EncodeToString(sum[:])
	return doc, nil
}

// Use OpenSearch's built-in CJK analyzer so installation needs no extra plugin.
// Only projection fields are accepted. Text is indexed without storing its original value.
func (s *PublicOpenSearch) create(ctx context.Context, generation string) error {
	mapping := json.RawMessage(`{
	    "dynamic": "strict",
	    "_source": {"includes": ["ref", "digest"]},
	    "properties": {
	      "ref": {"properties": {
	        "kind": {"type": "keyword"}, "domain": {"type": "keyword"},
	        "id": {"type": "keyword", "index": false},
	        "version": {"type": "keyword", "index": false}
	      }},
	      "key": {"type": "keyword"},
	      "withdrawn": {"type": "boolean"},
	      "identifiers": {"type": "keyword"},
	      "region": {"type": "keyword"},
	      "notice_type": {"type": "keyword"},
	      "published_at": {"type": "date"},
	      "title": {"type": "text", "analyzer": "cjk"},
	      "text": {"type": "text", "analyzer": "cjk"},
	      "digest": {"type": "keyword", "index": false, "doc_values": false}
	    }
	}`)
	return s.acknowledged(ctx, http.MethodPut, "/"+s.prefix+generation, map[string]any{
		"mappings": mapping,
		"aliases":  map[string]any{s.writeAlias(generation): map[string]any{"is_write_index": true}},
	})
}

func (s *PublicOpenSearch) writeAlias(generation string) string {
	return s.prefix + generation + "-write"
}

func (s *PublicOpenSearch) apply(ctx context.Context, generation string, mutation PublicMutation) error {
	doc, err := mutation.document()
	if err != nil {
		return err
	}
	resource := "/" + s.prefix + generation + "/_doc/" + doc.Key
	params := url.Values{
		"version": {strconv.FormatInt(mutation.Revision, 10)}, "version_type": {"external"},
		"require_alias": {"true"},
	}
	status, err := s.call(ctx, http.MethodPut, "/"+s.writeAlias(generation)+"/_doc/"+doc.Key, params, doc, nil)
	if err == nil || status != http.StatusConflict {
		return err
	}

	// A retained tombstone uses the same version rules as a live document.
	// A retry can acknowledge an identical write, including one that reached
	// OpenSearch immediately before the process lost its SQL commit.
	var existing struct {
		Version int64 `json:"_version"`
		Found   bool  `json:"found"`
		Source  struct {
			Digest string `json:"digest"`
		} `json:"_source"`
	}
	if _, err := s.call(ctx, http.MethodGet, resource, nil, nil, &existing); err != nil {
		return err
	}
	if !existing.Found || existing.Version < mutation.Revision ||
		(existing.Version == mutation.Revision && existing.Source.Digest != doc.Digest) {
		return errors.New("search: conflicting public projection revision")
	}
	return nil
}

func (s *PublicOpenSearch) refresh(ctx context.Context, generation string) error {
	var result struct {
		Shards struct {
			Failed int `json:"failed"`
		} `json:"_shards"`
	}
	_, err := s.call(ctx, http.MethodPost, "/"+s.prefix+generation+"/_refresh", nil, nil, &result)
	if err == nil && result.Shards.Failed != 0 {
		err = errors.New("search: public index refresh was incomplete")
	}
	return err
}

// PublicPublication comes from the atomic alias state, including an activation
// token that changes on rollback to a previous physical generation.
type PublicPublication struct {
	Generation string    `json:"generation"`
	Activation string    `json:"activation"`
	IndexedAt  time.Time `json:"indexed_at"`
}

func (s *PublicOpenSearch) Current(ctx context.Context) (PublicPublication, error) {
	var aliases map[string]struct {
		Aliases map[string]struct {
			Filter struct {
				MatchAll struct {
					Name string `json:"_name"`
				} `json:"match_all"`
			} `json:"filter"`
		} `json:"aliases"`
	}
	status, err := s.call(ctx, http.MethodGet, "/"+s.alias+"/_alias/"+s.alias, nil, nil, &aliases)
	if status == http.StatusNotFound {
		return PublicPublication{}, ErrPublicIndexUnpublished
	}
	if err != nil {
		return PublicPublication{}, err
	}
	if len(aliases) != 1 {
		return PublicPublication{}, errors.New("search: invalid public alias target count")
	}
	for index, entry := range aliases {
		generation, scoped := strings.CutPrefix(index, s.prefix)
		activation := entry.Aliases[s.alias].Filter.MatchAll.Name
		token, millis, _ := strings.Cut(activation, ":")
		stamp, err := strconv.ParseInt(millis, 10, 64)
		if !scoped || !generationID.MatchString(generation) || !generationID.MatchString(token) || err != nil || stamp <= 0 {
			return PublicPublication{}, errors.New("search: invalid public alias metadata")
		}
		return PublicPublication{Generation: generation, Activation: activation, IndexedAt: time.UnixMilli(stamp).UTC()}, nil
	}
	return PublicPublication{}, ErrPublicIndexUnpublished
}

func (s *PublicOpenSearch) switchAlias(ctx context.Context, current, next PublicPublication) error {
	actions := make([]map[string]any, 0, 2)
	if current.Generation != "" {
		actions = append(actions, map[string]any{"remove": map[string]any{
			"index": s.prefix + current.Generation, "alias": s.alias, "must_exist": true,
		}})
	}
	actions = append(actions, map[string]any{"add": map[string]any{
		"index": s.prefix + next.Generation, "alias": s.alias,
		"filter": map[string]any{"match_all": map[string]any{"_name": next.Activation}},
	}})
	return s.acknowledged(ctx, http.MethodPost, "/_aliases", map[string]any{"actions": actions})
}

func randomPublicID() string {
	var id [16]byte
	rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

func (s *PublicOpenSearch) acknowledged(ctx context.Context, method, path string, body any) error {
	var result struct {
		Acknowledged bool `json:"acknowledged"`
		Errors       bool `json:"errors"`
	}
	_, err := s.call(ctx, method, path, nil, body, &result)
	if err == nil && (!result.Acknowledged || result.Errors) {
		err = errors.New("search: OpenSearch did not acknowledge the change")
	}
	return err
}

func (s *PublicOpenSearch) call(ctx context.Context, method, path string, query url.Values, body, result any) (int, error) {
	u := *s.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawPath = ""
	u.RawQuery = query.Encode()
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		input = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), input)
	if err != nil {
		return 0, err
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxPublicIndexResponse+1))
	if err != nil || len(raw) > maxPublicIndexResponse {
		return res.StatusCode, errors.New("search: invalid OpenSearch response")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return res.StatusCode, fmt.Errorf("search: OpenSearch status %d", res.StatusCode)
	}
	if result != nil {
		if err := json.Unmarshal(raw, result); err != nil {
			return res.StatusCode, errors.New("search: invalid OpenSearch response")
		}
	}
	return res.StatusCode, nil
}
