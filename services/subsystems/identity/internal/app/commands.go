package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

var commandKey = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// committedFailure 是随事务提交的失败结果（例如决定时发现申请人已失权，失效已写入）：command 把它记为回执，
// 同一幂等键的重试返回同一错误，而不是空的成功结果。调用方收到后让事务提交，再把 Err 返回给调用者。
type committedFailure struct{ Err *api.Error }

func (c *committedFailure) Error() string { return c.Err.Error() }

// failureReceipt 是失败回执的存储形式；成功回执是响应本身，不含该键。
type failureReceipt struct {
	Failure *struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	} `json:"__committed_failure,omitempty"`
}

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
		var receipt failureReceipt
		if err := json.Unmarshal(previous, &receipt); err == nil && receipt.Failure != nil {
			return &committedFailure{Err: &api.Error{Status: receipt.Failure.Status, Code: receipt.Failure.Code}}
		}
		return json.Unmarshal(previous, response)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var stored error
	if err := apply(); err != nil {
		var committed *committedFailure
		if !errors.As(err, &committed) {
			return err
		}
		stored = committed
		var receipt failureReceipt
		receipt.Failure = &struct {
			Status int    `json:"status"`
			Code   string `json:"code"`
		}{committed.Err.Status, committed.Err.Code}
		response = receipt
	}
	encoded, err = json.Marshal(response)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO identity_commands(actor,tenant_id,request_key,operation,fingerprint,response) VALUES($1,$2,$3,$4,$5,$6)`, rc.Principal.ID, rc.Tenant, key, operation, fingerprint, encoded); err != nil {
		return err
	}
	return stored
}
