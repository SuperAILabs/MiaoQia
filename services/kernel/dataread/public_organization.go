package dataread

import (
	"context"
	"strings"

	dataapi "github.com/superailabs/miaoqia/services/contracts/apis/data-api"
)

// PublicOrganizationReader reads a confirmed organization only while the requested version remains current.
type PublicOrganizationReader interface {
	ReadPublicOrganization(context.Context, dataapi.PublicOrganizationRequest) (*dataapi.PublicOrganizationRead, error)
}

var _ PublicOrganizationReader = (*Client)(nil)

func (c *Client) ReadPublicOrganization(ctx context.Context, request dataapi.PublicOrganizationRequest) (*dataapi.PublicOrganizationRead, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return invoke(ctx, c, "GetPublicOrganization", func(ctx context.Context) (*dataapi.PublicOrganizationRead, error) {
		raw, err := c.invoker.GetPublicOrganization(ctx, &request)
		value, found := raw.Get()
		if err != nil || !found {
			return nil, err
		}
		if err := value.Validate(); err != nil {
			return nil, err
		}
		if value.OrgID != request.OrgID || value.Version != request.Version ||
			strings.TrimSpace(value.Name) == "" || strings.ContainsRune(value.Name, '\x00') || !validUSCCChecksum(value.Uscc) {
			return nil, errMismatch
		}
		return &value, nil
	})
}

func validUSCCChecksum(code string) bool {
	const alphabet = "0123456789ABCDEFGHJKLMNPQRTUWXY"
	weights := [...]int{1, 3, 9, 27, 19, 26, 16, 17, 20, 29, 25, 13, 8, 24, 10, 30, 28}
	if len(code) != len(weights)+1 {
		return false
	}
	sum := 0
	for position, weight := range weights {
		value := strings.IndexByte(alphabet, code[position])
		if value < 0 {
			return false
		}
		sum += value * weight
	}
	return code[len(weights)] == alphabet[(len(alphabet)-sum%len(alphabet))%len(alphabet)]
}
