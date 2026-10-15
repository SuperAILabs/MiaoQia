package reqctx

import "context"

type mfaKey struct{}

// WithMFA carries assurance from the verified access token within the gateway process.
// It is deliberately NOT serialized in request-context headers: internal peers cannot assert MFA.
func WithMFA(ctx context.Context, verified bool) context.Context {
	return context.WithValue(ctx, mfaKey{}, verified)
}

// MFA reports signed MFA assurance validated by the public gateway.
func MFA(ctx context.Context) bool { value, _ := ctx.Value(mfaKey{}).(bool); return value }
