package dataread

import (
	"context"
	"errors"
	"strconv"
	"time"
	"unicode/utf8"

	dataapi "github.com/superailabs/miaoqia/services/contracts/apis/data-api"
)

var errMismatch = errors.New("data-api response does not match the request")

// NoticeChunkReader is the current-only evidence span port used by public search.
type NoticeChunkReader interface {
	ReadNoticeChunk(context.Context, dataapi.NoticeChunkRequest) (*dataapi.NoticeChunkRead, error)
}

func (c *Client) ReadNoticeChunk(ctx context.Context, request dataapi.NoticeChunkRequest) (*dataapi.NoticeChunkRead, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return invoke(ctx, c, "GetNoticeChunk", func(ctx context.Context) (*dataapi.NoticeChunkRead, error) {
		raw, err := c.invoker.GetNoticeChunk(ctx, &request)
		value, found := raw.Get()
		if err != nil || !found {
			return nil, err
		}
		if err := value.Validate(); err != nil {
			return nil, err
		}
		loc := value.Locator
		_, row := loc.Row.Get()
		_, col := loc.Col.Get()
		if value.FactID != request.FactID || value.Version != request.Version || value.Ordinal != request.Ordinal ||
			row != col || loc.CharEnd <= loc.CharStart || loc.CharEnd-loc.CharStart != int64(utf8.RuneCountInString(value.Text)) {
			return nil, errMismatch
		}
		return &value, nil
	})
}

func validateBasis(b dataapi.Basis) error {
	if err := b.Validate(); err != nil {
		return err
	}
	_, hasCutoff := b.RecordedCutoff.Get()
	if hasCutoff != (b.View == dataapi.BasisViewKnownAt) {
		return errors.New("data-api basis and cutoff disagree")
	}
	return nil
}

func equalBasis(a, b dataapi.Basis) bool {
	at, aCutoff := a.RecordedCutoff.Get()
	bt, bCutoff := b.RecordedCutoff.Get()
	// PostgreSQL and Python represent the returned cutoff with microsecond precision.
	return a.View == b.View && aCutoff == bCutoff &&
		(!aCutoff || at.Equal(bt) || at.Truncate(time.Microsecond).Equal(bt))
}

func matchesFact(request dataapi.FactRequest, id string, version int64) bool {
	want, fixed := request.Version.Get()
	return request.FactID == id && (!fixed || want == version)
}

func (c *Client) ReadFact(ctx context.Context, request dataapi.FactRequest) (*dataapi.FactRead, error) {
	if err := validateBasis(request.Basis); err != nil {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return invoke(ctx, c, "GetFact", func(ctx context.Context) (*dataapi.FactRead, error) {
		raw, err := c.invoker.GetFact(ctx, &request)
		value, found := raw.Get()
		if err != nil || !found {
			return nil, err
		}
		if err := value.Validate(); err != nil {
			return nil, err
		}
		if !equalBasis(request.Basis, value.Basis) || !matchesFact(request, value.Fact.ID, value.Fact.Version) {
			return nil, errMismatch
		}
		return &value, nil
	})
}

func (c *Client) ReadFactHistory(ctx context.Context, request dataapi.FactHistoryRequest) (*dataapi.FactHistoryPage, error) {
	if err := validateBasis(request.Basis); err != nil {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return invoke(ctx, c, "FactHistory", func(ctx context.Context) (*dataapi.FactHistoryPage, error) {
		page, err := c.invoker.FactHistory(ctx, &request)
		if err != nil {
			return nil, err
		}
		if page == nil || !equalBasis(request.Basis, page.Basis) || len(page.Items) > request.Limit {
			return nil, errMismatch
		}
		if err := page.Validate(); err != nil {
			return nil, err
		}
		after := request.After
		for _, item := range page.Items {
			if item.Fact.ID != request.FactID || item.Fact.Version <= after {
				return nil, errMismatch
			}
			after = item.Fact.Version
		}
		if cursor, ok := page.NextCursor.Get(); ok && (len(page.Items) != request.Limit || cursor != strconv.FormatInt(after, 10)) {
			return nil, errMismatch
		}
		return page, nil
	})
}

func (c *Client) ReadFacts(ctx context.Context, request dataapi.QueryFactsRequest) (*dataapi.FactPage, error) {
	if err := validateBasis(request.Basis); err != nil {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return invoke(ctx, c, "QueryFacts", func(ctx context.Context) (*dataapi.FactPage, error) {
		page, err := c.invoker.QueryFacts(ctx, &request)
		if err != nil {
			return nil, err
		}
		if page == nil || !equalBasis(request.Basis, page.Basis) || len(page.Items) > request.Limit {
			return nil, errMismatch
		}
		if err := page.Validate(); err != nil {
			return nil, err
		}
		after := request.After
		for _, fact := range page.Items {
			if fact.ID <= after {
				return nil, errMismatch
			}
			after = fact.ID
		}
		if cursor, ok := page.NextCursor.Get(); ok && (len(page.Items) != request.Limit || cursor != after) {
			return nil, errMismatch
		}
		return page, nil
	})
}

func (c *Client) ReadBidResultUnits(ctx context.Context, request dataapi.FactRequest) (*dataapi.BidResultPage, error) {
	if err := validateBasis(request.Basis); err != nil {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return invoke(ctx, c, "BidResultUnits", func(ctx context.Context) (*dataapi.BidResultPage, error) {
		page, err := c.invoker.BidResultUnits(ctx, &request)
		if err != nil {
			return nil, err
		}
		if page == nil || !equalBasis(request.Basis, page.Basis) {
			return nil, errMismatch
		}
		if err := page.Validate(); err != nil {
			return nil, err
		}
		for _, unit := range page.Items {
			if !matchesFact(request, unit.Subject.ID, unit.Subject.Version) {
				return nil, errMismatch
			}
		}
		return page, nil
	})
}

func (c *Client) ReadEvidence(ctx context.Context, request dataapi.EvidenceRequest) (*dataapi.EvidenceRead, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return invoke(ctx, c, "ResolveEvidence", func(ctx context.Context) (*dataapi.EvidenceRead, error) {
		raw, err := c.invoker.ResolveEvidence(ctx, &request)
		value, found := raw.Get()
		if err != nil || !found {
			return nil, err
		}
		if err := value.Validate(); err != nil {
			return nil, err
		}
		evidence := value.Evidence
		if evidence.ID != request.EvidenceRef {
			return nil, errMismatch
		}
		_, locator := evidence.Locator.Get()
		snippet, hasSnippet := evidence.Snippet.Get()
		_, hasContext := evidence.Context.Get()
		switch evidence.DisplayLevel {
		case dataapi.EvidenceReadEvidenceDisplayLevelCitationOnly:
			if locator || hasSnippet || hasContext {
				return nil, errMismatch
			}
		case dataapi.EvidenceReadEvidenceDisplayLevelExcerpt:
			if !locator || !hasSnippet || hasContext || len([]rune(snippet)) > 512 {
				return nil, errMismatch
			}
		case dataapi.EvidenceReadEvidenceDisplayLevelFullText:
			if !locator || !hasSnippet || !hasContext {
				return nil, errMismatch
			}
		}
		return &value, nil
	})
}
