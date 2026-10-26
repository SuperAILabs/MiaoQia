package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

// 对象页的本租户信号分区（PRD 第4段 F3–F5，detection.md 第5节 ListSignalsForObject）：跨目标列出与一个对象相关的本租户信号，
// 只返回查看者有 can_view_signals 的目标的信号。授权逐目标经 customer-context 的 ObjectiveAccess 按其当前口径判断（与收件箱相同），
// 每页请求重新判断；判断失败时整页失败关闭（503），不退化为只返回部分目标或不返回授权检查。

const objectPageSize = 20 // 对象页列表每页条数（PRD 第4段第13节第4项）

// ObjectPage 是对象页的一页本租户信号（契约 ObjectSignalPage）。
type ObjectPage struct {
	ObjectID   string `json:"object_id"`
	Signals    []Row  `json:"signals"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// ListObjectSignals 按采购项目、项目或组织列出本租户信号（契约 listObjectSignals），按发现时间新到旧。
func (s *Service) ListObjectSignals(ctx context.Context, object, role string, limit int, after string) (*ObjectPage, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	if role == "" {
		role = "any"
	}
	prefix, _, _ := strings.Cut(object, "_")
	if prefix != "org" && role != "any" {
		return nil, &api.Error{Status: 400, Code: "invalid_request", Detail: "role 只用于组织"}
	}
	if limit <= 0 || limit > objectPageSize {
		limit = objectPageSize
	}
	bind := binding("object", object, role, rc.Principal.ID)
	pos, err := decodeCursor(after, bind)
	if err != nil {
		return nil, err
	}
	page := &ObjectPage{ObjectID: object, Signals: []Row{}}
	var cond string
	var match any = object
	switch prefix {
	case "pp":
		cond = "s.procurement_project_id = $2"
	case "prj":
		// 项目经当前有效的归属边展开到采购项目；归属边读取失败或未接入时失败关闭，不当作“没有信号”
		if s.Links == nil {
			return nil, failure(503, "project_links_unavailable")
		}
		projects, err := s.Links.ProcurementProjects(ctx, object)
		if err != nil {
			if !errors.Is(err, api.ErrNotWired) {
				s.logger().WarnContext(ctx, "归属边读取失败", "error", err)
			}
			return nil, failure(503, "project_links_unavailable")
		}
		if len(projects) == 0 {
			return page, nil
		}
		cond, match = "s.procurement_project_id = ANY($2)", projects
	case "org":
		switch role {
		case "winner":
			cond = "s.winner_org_id = $2"
		case "buyer":
			cond = "s.buyer_org_id = $2"
		default:
			cond = "(s.winner_org_id = $2 OR s.buyer_org_id = $2)"
		}
	default:
		return nil, &api.Error{Status: 400, Code: "invalid_request", Detail: "不支持的对象类型"}
	}
	// 相关信号所属的目标（行级安全限定本租户），逐个按当前口径判断 can_view_signals
	var objectives []string
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT s.objective_id FROM det_signals s WHERE `+strings.ReplaceAll(cond, "$2", "$1")+` ORDER BY 1`, match)
		if err != nil {
			return err
		}
		objectives, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return nil, err
	}
	allowed := []string{}
	for _, o := range objectives {
		visible, granted, err := s.access(ctx, o, relViewSignals)
		if err != nil {
			return nil, err
		}
		if visible && has(granted, relViewSignals) {
			allowed = append(allowed, o)
		}
	}
	if len(allowed) == 0 {
		return page, nil
	}
	args := []any{rc.Principal.ID, match, allowed}
	where := " WHERE " + cond + " AND s.objective_id = ANY($3)"
	if pos != nil {
		args = append(args, pos.Created, pos.ID)
		where += " AND (s.created_at, s.id) < ($4, $5)"
	}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+rowColumns+`, 0`+rowFrom+where+fmt.Sprintf(` ORDER BY s.created_at DESC, s.id DESC LIMIT %d`, limit+1), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key int64
			r, err := s.scanRow(scanAppend{rows, &key})
			if err != nil {
				return err
			}
			page.Signals = append(page.Signals, *r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(page.Signals) > limit {
			last := page.Signals[limit-1]
			page.Signals = page.Signals[:limit]
			page.NextCursor = encodeCursor(cursor{Binding: bind, Created: last.CreatedAt, ID: last.ID})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}
