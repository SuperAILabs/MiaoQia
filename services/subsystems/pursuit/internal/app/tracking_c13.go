package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// 持续跟踪的改派与协作者、归档、按名单导入对象与各期对象的实体来源版本（PRD 第6段 F10 第1–3条，C-13）。

// 上限（契约 AssignRequest.collaborators 与 TrackingImportRequest.rows 的 maxItems）。
const (
	MaxCollaborators = 20
	MaxImportRows    = 500
)

// ArchiveTracking 归档（契约 archiveTracking；F10 第2条）：已停止 → 已归档（只读，不在缺省列表中）。已归档为无变化，跟踪中与已暂停
// 409 invalid_transition（须先停止）。
func (s *Service) ArchiveTracking(ctx context.Context, id, key string, req VersionRequest) (*TrackingResult, error) {
	return s.changeTracking(ctx, id, key, req.OperationID, req.Expected, req, trackingChange{action: "archive_tracking",
		apply: func(p *Pursuit, t *trackingRow, now time.Time) (string, string, string, error) {
			switch t.State {
			case "archived":
				return "", "", "", errNoChange
			case "tracking", "paused":
				return "", "", "", conflict("invalid_transition", p, nil)
			}
			at := now
			t.State, t.ArchivedAt = "archived", &at
			return "archived", "tracking_archived", "", nil
		}})
}

// UnarchiveTracking 取消归档（契约 unarchiveTracking）：已归档 → 已停止（不恢复跟踪）。已停止为无变化，跟踪中与已暂停
// 409 invalid_transition。
func (s *Service) UnarchiveTracking(ctx context.Context, id, key string, req VersionRequest) (*TrackingResult, error) {
	return s.changeTracking(ctx, id, key, req.OperationID, req.Expected, req, trackingChange{action: "unarchive_tracking",
		apply: func(p *Pursuit, t *trackingRow, now time.Time) (string, string, string, error) {
			switch t.State {
			case "stopped":
				return "", "", "", errNoChange
			case "tracking", "paused":
				return "", "", "", conflict("invalid_transition", p, nil)
			}
			t.State, t.ArchivedAt = "stopped", nil
			return "unarchived", "tracking_unarchived", "", nil
		}})
}

// AssignTracking 改派持续跟踪的负责人与设置协作者（契约 assignTracking；F4、F12）：与交易型同一流程（assignTeam：推进的写入准入、
// 被分派人以其本人判断、加锁重读后再判断、授权同步），已归档 409 invalid_transition。
func (s *Service) AssignTracking(ctx context.Context, id, key string, req AssignRequest) (*TrackingResult, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	req.Collaborators = dedupe(req.Collaborators, req.Owner)
	if req.Owner == "" || len(req.Collaborators) > MaxCollaborators {
		return nil, failure(400, "invalid_request")
	}
	fp := fingerprint("assign_tracking", id, req)
	return s.runTracking(ctx, rc, "assign_tracking", key, fp, func() (*Fixed, error) {
		return s.assignTeam(ctx, rc, id, "continuous", req, op{id: req.OperationID, fp: fp, action: "assign_tracking", channel: "web", pursuit: id})
	})
}

// ImportRow 是契约 TrackingImportResult。
type ImportRow struct {
	Index   int    `json:"index"`
	OrgID   string `json:"org_id,omitempty"`
	Outcome string `json:"outcome"`
}

// ImportLine 是契约 TrackingImportRow。
type ImportLine struct {
	OrgID string `json:"org_id,omitempty"`
	Name  string `json:"name,omitempty"`
}

// ImportRequest 是契约 TrackingImportRequest。
type ImportRequest struct {
	OperationID string       `json:"operation_id"`
	Expected    int64        `json:"expected_version"`
	Rows        []ImportLine `json:"rows"`
}

// ImportTrackingMembers 按名单导入对象（契约 importTrackingMembers；F10 第1条“取自企业清单”）。逐行判断：不是组织标识（或空行）invalid、
// 只有名称 unresolved（按名称解析组织尚未接入，不猜测）、与前面的行重复 duplicate、实体图中不存在 not_found、已在集合中 already_member、
// 超过 200 个对象的上限 over_limit、其余 added。有加入的行时成员版本加一（members_imported），否则为无变化。
//
// 组织存在经 api.Organizations 在提交前核对（不持锁；未接入时只校验格式并说明 not_wired，暂时不可用 503 不固定）；“已在集合中”与上限在
// 加锁重读后按当前对象集合判断。逐行结果与操作一起固定（重放返回同一结果，含无变化）。
func (s *Service) ImportTrackingMembers(ctx context.Context, id, key string, req ImportRequest) (*TrackingResult, error) {
	if len(req.Rows) == 0 || len(req.Rows) > MaxImportRows {
		return nil, failure(400, "invalid_request")
	}
	// 第一遍：格式与请求内重复（与当前集合无关）
	rows := make([]ImportRow, len(req.Rows))
	var candidates []string
	for i, r := range req.Rows {
		rows[i].Index = i
		switch {
		case r.OrgID != "":
			if !orgPattern.MatchString(r.OrgID) {
				rows[i].Outcome = "invalid"
				continue
			}
			rows[i].OrgID = r.OrgID
			if slices.Contains(candidates, r.OrgID) {
				rows[i].Outcome = "duplicate"
				continue
			}
			candidates = append(candidates, r.OrgID)
		case strings.TrimSpace(r.Name) != "":
			rows[i].Outcome = "unresolved"
		default:
			rows[i].Outcome = "invalid"
		}
	}
	var existing map[string]int64
	check := "verified"
	var final []ImportRow
	return s.changeTracking(ctx, id, key, req.OperationID, req.Expected, req, trackingChange{action: "import_tracking_members",
		prepare: func(ctx context.Context) error {
			if len(candidates) == 0 {
				return nil
			}
			found, err := s.organizations().OrganizationVersions(ctx, candidates)
			switch {
			case errors.Is(err, api.ErrNotWired):
				check, existing = "not_wired", nil
			case err != nil:
				s.logger().WarnContext(ctx, "实体图暂时不可用，名单导入未执行", "error", err.Error())
				return failure(503, "entity_graph_unavailable")
			default:
				existing = found
			}
			return nil
		},
		apply: func(p *Pursuit, t *trackingRow, now time.Time) (string, string, string, error) {
			if t.closed() {
				return "", "", "", conflict("invalid_transition", p, nil)
			}
			final = slices.Clone(rows)
			members := slices.Clone(t.Members)
			added := 0
			for i := range final {
				if final[i].Outcome != "" {
					continue
				}
				org := final[i].OrgID
				switch {
				case check == "verified" && existing[org] < 1:
					final[i].Outcome = "not_found"
				case slices.Contains(t.Members, org):
					final[i].Outcome = "already_member"
				case len(members) >= MaxTrackingMembers:
					final[i].Outcome = "over_limit"
				default:
					final[i].Outcome = "added"
					members = append(members, org)
					added++
				}
			}
			if added == 0 {
				return "", "", "", errNoChange
			}
			t.setMembers(members, "import")
			return "members_imported", "members_imported", fmt.Sprintf("名单导入 %d 行，加入 %d 个；成员版本 %d", len(final), added, t.MembersVersion), nil
		},
		decorate: func(f *Fixed) {
			f.ImportRows, f.ImportCheck = final, check
		}})
}

func (s *Service) organizations() api.Organizations {
	if s.Organizations == nil {
		return api.NotWired{}
	}
	return s.Organizations
}

// PeriodObject 是契约 TrackingObject：期末成员快照中的一个对象。
type PeriodObject struct {
	OrgID         string `json:"org_id"`
	AddedVersion  *int64 `json:"added_members_version,omitempty"`
	Source        string `json:"source"`
	EntityState   string `json:"entity_state"`
	EntityVersion *int64 `json:"entity_version,omitempty"`
}

// entityVersions 是结束一期前（不持锁）读取的实体来源版本：state 为 recorded（versions 中有的为当时的版本，没有的为不存在）、
// not_wired 或 unavailable；asked 是读取时的对象集合（加锁后新出现的对象记为 unavailable，不在锁内调用端口）。
type entityVersions struct {
	state    string
	versions map[string]int64
	asked    []string
}

// readEntityVersions 读取对象的实体来源版本；未接入与暂时不可用都不阻止结束一期，如实记录。
func (s *Service) readEntityVersions(ctx context.Context, members []string) entityVersions {
	ev := entityVersions{state: "recorded", asked: members}
	if len(members) == 0 {
		return ev
	}
	found, err := s.organizations().OrganizationVersions(ctx, members)
	switch {
	case errors.Is(err, api.ErrNotWired):
		ev.state = "not_wired"
	case err != nil:
		s.logger().WarnContext(ctx, "实体图暂时不可用，本期对象的来源版本记为 unavailable", "error", err.Error())
		ev.state = "unavailable"
	default:
		ev.versions = found
	}
	return ev
}

// objects 返回期末每个对象的加入版本、来源与实体来源版本（顺序同 members）。
func (ev entityVersions) objects(t *trackingRow) []PeriodObject {
	out := make([]PeriodObject, 0, len(t.Members))
	for _, m := range t.Members {
		src := t.source(m)
		o := PeriodObject{OrgID: m, AddedVersion: src.Version, Source: src.Source, EntityState: ev.state}
		switch {
		case !slices.Contains(ev.asked, m):
			o.EntityState = "unavailable"
		case ev.state == "recorded":
			if v, ok := ev.versions[m]; ok && v >= 1 {
				o.EntityVersion = &v
			} else {
				o.EntityState = "not_found"
			}
		}
		out = append(out, o)
	}
	return out
}
