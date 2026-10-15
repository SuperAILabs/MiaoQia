package api

import (
	"context"
	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/binding"
)

const SessionBinding = "identity.SessionResolver"

type Session = identityapi.Session

// SessionResolver is the authoritative tenant/membership/cell check, shared by local and remote binding.
type SessionResolver interface {
	Resolve(context.Context) (*Session, error)
}

func LocalSession(impl SessionResolver) SessionResolver { return localSession{impl} }

type localSession struct{ impl SessionResolver }

func (s localSession) Resolve(ctx context.Context) (*Session, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ResolveSession", Idempotent: true}, s.impl.Resolve)
}

// Error is safe to expose at the transport boundary; raw infrastructure errors are not.
type Error struct {
	Status int
	Code   string
}

func (e *Error) Error() string { return e.Code }
