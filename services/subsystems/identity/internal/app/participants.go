package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// syncRelations 是各类资源的同步授权（model.fga tenant 上的服务动作；只授予资源所属子系统）。
var syncRelations = map[string]string{
	"objective":     "can_sync_objective_participants",
	"pursuit":       "can_sync_pursuit_participants",
	"research_case": "can_sync_research_case_participants",
}

type rejection = identityapi.SyncParticipantsResultRejectedItem

// SyncResourceParticipants 写入业务资源上的参与关系（identity.md 第5节 SyncResourceParticipants）。
//
// 只接受高于已应用版本的参与版本：期望关系与成员状态及资源所在单元的范围访问求交，在一次 OpenFGA 写入中加上应有的、
// 删掉多余的关系，在同一 SQL 事务中记录已应用版本与结果；同一版本同一请求返回首次结果，同一版本不同请求冲突，
// 低于已应用版本返回 stale 且不做修改。资源行在事务中加锁，同一资源的并发同步串行；涉及的成员行加共享锁，
// 与成员命令及退出后的清理（二者对成员行加排他锁，见 purgeAfterLeave）串行：清理不会被并发同步重新写入，
// 也不会删除成员重新加入后经更高版本写入的关系。
func (d *Directory) SyncResourceParticipants(ctx context.Context, req *identityapi.SyncParticipantsRequest) (*identityapi.SyncParticipantsResult, error) {
	if req == nil || req.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	kind, _, _ := strings.Cut(req.Resource, ":")
	allowed := resourceRelations[kind]
	for _, p := range req.Participants {
		if !slices.Contains(allowed, string(p.Relation)) {
			return nil, failure(400, "invalid_request")
		}
	}
	rc, ok := reqctx.From(ctx)
	if !ok || rc.Validate() != nil || rc.Principal.Kind != "service" || rc.Tenant == "" {
		return nil, failure(403, "forbidden")
	}
	permitted, err := d.CheckService(ctx, syncRelations[kind], "tenant:"+rc.Tenant)
	if err != nil {
		return nil, err
	}
	if !permitted {
		return nil, failure(403, "forbidden")
	}
	desired := slices.Clone(req.Participants)
	slices.SortFunc(desired, func(a, b identityapi.ParticipantRelation) int {
		return strings.Compare(a.Subject+"\x00"+string(a.Relation), b.Subject+"\x00"+string(b.Relation))
	})
	encoded, err := json.Marshal(struct {
		Unit         string                            `json:"org_unit_id"`
		Participants []identityapi.ParticipantRelation `json:"participants"`
	}{string(req.OrgUnitID), desired})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(digest[:])
	out := &identityapi.SyncParticipantsResult{Resource: req.Resource, Rejected: []rejection{}}
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := serviceTenant(ctx, tx, rc); err != nil {
			return err
		}
		chain, err := unitChain(ctx, tx, rc.Tenant, string(req.OrgUnitID))
		if err != nil {
			return err
		}
		if chain == nil {
			return failure(404, "org_unit_not_found")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identity_resources(object,tenant_id,org_unit_id,applied_version,fingerprint,result) VALUES($1,$2,$3,0,'','{}') ON CONFLICT (object) DO NOTHING`,
			req.Resource, rc.Tenant, string(req.OrgUnitID)); err != nil {
			return err
		}
		var owner, previousHash string
		var applied int64
		var previous []byte
		if err := tx.QueryRow(ctx, `SELECT tenant_id, applied_version, fingerprint, result FROM identity_resources WHERE object=$1 FOR UPDATE`, req.Resource).
			Scan(&owner, &applied, &previousHash, &previous); err != nil {
			return err
		}
		if owner != rc.Tenant {
			return failure(409, "resource_conflict")
		}
		switch {
		case req.Version < applied:
			out.AppliedVersion, out.Outcome = applied, identityapi.SyncParticipantsResultOutcomeStale
			return nil
		case req.Version == applied:
			if previousHash != fingerprint {
				return failure(409, "participants_version_conflict")
			}
			if err := json.Unmarshal(previous, out); err != nil {
				return err
			}
			out.Outcome = identityapi.SyncParticipantsResultOutcomeReplayed
			return nil
		}
		accepted, rejected, err := d.admit(ctx, tx, rc.Tenant, chain, desired)
		if err != nil {
			return err
		}
		current, err := d.store().ReadObject(ctx, req.Resource)
		if err != nil {
			return err
		}
		want := map[[3]string]bool{}
		for _, p := range accepted {
			want[[3]string{"user:" + p.Subject, string(p.Relation), req.Resource}] = true
		}
		var adds, deletes [][3]string
		have := map[[3]string]bool{}
		for _, t := range current {
			if !slices.Contains(allowed, t[1]) {
				continue
			}
			have[t] = true
			if !want[t] {
				deletes = append(deletes, t)
			}
		}
		for _, p := range accepted {
			t := [3]string{"user:" + p.Subject, string(p.Relation), req.Resource}
			if !have[t] {
				adds = append(adds, t)
			}
		}
		if err := d.store().Write(ctx, adds, deletes); err != nil {
			return err
		}
		out.AppliedVersion, out.Outcome, out.Rejected = req.Version, identityapi.SyncParticipantsResultOutcomeApplied, rejected
		stored, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE identity_resources SET org_unit_id=$2, applied_version=$3, fingerprint=$4, result=$5, updated_at=now() WHERE object=$1`,
			req.Resource, string(req.OrgUnitID), req.Version, fingerprint, stored); err != nil {
			return err
		}
		return (audit.Recorder{Subsystem: api.Name}).Record(ctx, tx, audit.Entry{Action: "identity.participants.sync",
			Resource: fmt.Sprintf("%s/v%d", req.Resource, req.Version), Result: "succeeded", Basis: syncRelations[kind]})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// admit 把期望的参与关系与成员状态及资源所在单元的范围访问（org_unit#scope_member）求交。
func (d *Directory) admit(ctx context.Context, tx pgx.Tx, tenantID string, chain []string, desired []identityapi.ParticipantRelation) ([]identityapi.ParticipantRelation, []rejection, error) {
	subjects := []string{}
	for _, p := range desired {
		if !slices.Contains(subjects, p.Subject) {
			subjects = append(subjects, p.Subject)
		}
	}
	type member struct {
		state string
		roles []string
		units map[string][]string
	}
	members := map[string]*member{}
	rows, err := tx.Query(ctx, `SELECT subject, state, roles FROM identity_members WHERE tenant_id=$1 AND subject = ANY($2) FOR SHARE`, tenantID, subjects)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		m := &member{units: map[string][]string{}}
		var subject string
		if err := rows.Scan(&subject, &m.state, &m.roles); err != nil {
			rows.Close()
			return nil, nil, err
		}
		members[subject] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows, err = tx.Query(ctx, `SELECT subject, org_unit_id, roles FROM identity_member_unit_roles WHERE tenant_id=$1 AND subject = ANY($2) AND org_unit_id = ANY($3)`, tenantID, subjects, chain)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var subject, unit string
		var roles []string
		if err := rows.Scan(&subject, &unit, &roles); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if m := members[subject]; m != nil {
			m.units[unit] = roles
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	scoped := map[string]bool{}
	for subject, m := range members {
		if m.state != "active" {
			continue
		}
		sc, err := newScope()
		if err != nil {
			return nil, nil, err
		}
		user := "user:" + subject
		sc.member(user, m.roles, m.units, chain)
		sc.chain(chain)
		ok, err := d.store().Evaluate(ctx, user, "scope_member", sc.unit(chain[0]), sc.tuples)
		if err != nil {
			return nil, nil, err
		}
		scoped[subject] = ok
	}
	var accepted []identityapi.ParticipantRelation
	rejected := []rejection{}
	for _, p := range desired {
		m := members[p.Subject]
		reason := identityapi.SyncParticipantsResultRejectedItemReason("")
		switch {
		case m == nil:
			reason = identityapi.SyncParticipantsResultRejectedItemReasonNotMember
		case m.state != "active":
			reason = identityapi.SyncParticipantsResultRejectedItemReasonMemberInactive
		case !scoped[p.Subject]:
			reason = identityapi.SyncParticipantsResultRejectedItemReasonNoScopeAccess
		}
		if reason == "" {
			accepted = append(accepted, p)
			continue
		}
		rejected = append(rejected, rejection{Subject: p.Subject, Relation: identityapi.SyncParticipantsResultRejectedItemRelation(p.Relation), Reason: reason})
	}
	return accepted, rejected, nil
}

// purgeAfterLeave 是成员退出提交后的清理。它在新事务中先对成员行加排他锁并重核成员仍未处于有效状态，再读取并删除
// 参与关系，直到提交才释放锁：重新加入（成员命令对成员行加排他锁）与参与关系同步（admit 对成员行加共享锁）都与清理串行。
// 清理开始前成员已重新加入时直接跳过（重新加入在自己的事务中已清理过旧关系），因此旧的清理不会删除新一代成员经
// 更高参与版本写入的关系，同一版本的重放也无需重建。
func (d *Directory) purgeAfterLeave(ctx context.Context, tenantID, subject string) error {
	return d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var state string
		err := tx.QueryRow(ctx, `SELECT state FROM identity_members WHERE tenant_id=$1 AND subject=$2 FOR UPDATE`, tenantID, subject).Scan(&state)
		if err != nil || state == "active" {
			return err
		}
		return d.purgeParticipation(ctx, tx, tenantID, subject)
	})
}

// purgeParticipation 删除主体在本租户已登记资源上的全部参与关系（成员退出后清理、重新加入前兜底）。
// 调用方须在同一事务中持有该成员行的排他锁。
func (d *Directory) purgeParticipation(ctx context.Context, tx pgx.Tx, tenantID, subject string) error {
	user := "user:" + subject
	var candidates [][3]string
	objects := []string{}
	for kind, relations := range resourceRelations {
		tuples, err := d.store().ReadUser(ctx, user, kind)
		if err != nil {
			return err
		}
		for _, t := range tuples {
			if t[0] == user && slices.Contains(relations, t[1]) {
				candidates = append(candidates, t)
				objects = append(objects, t[2])
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT object FROM identity_resources WHERE tenant_id=$1 AND object = ANY($2)`, tenantID, objects)
	if err != nil {
		return err
	}
	owned, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var deletes [][3]string
	for _, t := range candidates {
		if slices.Contains(owned, t[2]) {
			deletes = append(deletes, t)
		}
	}
	for len(deletes) > 0 {
		n := min(len(deletes), 100)
		if err := d.store().Write(ctx, nil, deletes[:n]); err != nil {
			return err
		}
		deletes = deletes[n:]
	}
	return nil
}
