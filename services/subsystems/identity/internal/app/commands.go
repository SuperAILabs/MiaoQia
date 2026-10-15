package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"regexp"
)

var commandKey = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// command records a typed response in the SAME transaction as the mutation/outbox/audit.
// The caller authorizes first. A scoped advisory lock also serializes creation before a tenant row exists.
func command(ctx context.Context, tx pgx.Tx, key, operation string, request, response any, apply func() error) error {
	if !commandKey.MatchString(key) {
		return failure(400, "invalid_idempotency_key")
	}
	rc, _ := reqctx.From(ctx)
	scope := rc.Principal.ID + "/" + rc.Tenant + "/" + key
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, scope); err != nil {
		return err
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(digest[:])
	var previousOperation, previousHash string
	var previous []byte
	err = tx.QueryRow(ctx, `SELECT operation,fingerprint,response FROM identity_commands WHERE actor=$1 AND tenant_id=$2 AND request_key=$3`, rc.Principal.ID, rc.Tenant, key).Scan(&previousOperation, &previousHash, &previous)
	if err == nil {
		if previousOperation != operation || previousHash != fingerprint {
			return failure(409, "idempotency_conflict")
		}
		return json.Unmarshal(previous, response)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err := apply(); err != nil {
		return err
	}
	encoded, err = json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO identity_commands(actor,tenant_id,request_key,operation,fingerprint,response) VALUES($1,$2,$3,$4,$5,$6)`, rc.Principal.ID, rc.Tenant, key, operation, fingerprint, encoded)
	return err
}
