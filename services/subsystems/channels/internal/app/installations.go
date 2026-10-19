package app

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"

	chn "github.com/superailabs/miaoqia/services/contracts/apis/channels"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/domain"
)

// 渠道安装（PRD 第1段 F4）：未安装 ──授权──► 待验证 ──管理员本人绑定并确认──► 已启用；停用使全部绑定失效。
// 授权中：InstallChannel 生成一次性 state 与官方授权页地址；官方授权回调（InstallCallback）以 state 找到安装，
// 换得企业并登记企业路由（同一企业同时只属于一个租户）。

type installation struct {
	id, tenant, channel, state, corpID, corpName string
	version                                      int64
	createdAt                                    time.Time
	verifiedAt, disabledAt                       *time.Time
	bound, rejected                              int
}

const installationColumns = `i.id, i.tenant_id, i.channel, i.state, i.version, coalesce(i.corp_id,''), coalesce(i.corp_name,''), i.created_at, i.verified_at, i.disabled_at,
 (SELECT count(*) FROM chn_bindings b WHERE b.installation_id = i.id AND b.state = 'bound'),
 coalesce((SELECT r.rejected_callbacks FROM chn_corp_routes r WHERE r.installation_id = i.id), 0)`

func scanInstallation(row pgx.Row) (*installation, error) {
	var i installation
	err := row.Scan(&i.id, &i.tenant, &i.channel, &i.state, &i.version, &i.corpID, &i.corpName, &i.createdAt, &i.verifiedAt, &i.disabledAt, &i.bound, &i.rejected)
	return &i, err
}

func (i *installation) out() *chn.Installation {
	o := &chn.Installation{ID: chn.InstallationID(i.id), Channel: chn.ChannelKind(i.channel), State: chn.InstallationState(i.state), Version: i.version,
		BoundCount: i.bound, RejectedCallbacks: chn.NewOptInt(i.rejected), CreatedAt: i.createdAt.UTC()}
	if i.corpID != "" {
		o.CorpID = chn.NewOptString(i.corpID)
	}
	if i.corpName != "" {
		o.CorpName = chn.NewOptString(i.corpName)
	}
	if i.verifiedAt != nil {
		o.VerifiedAt = chn.NewOptDateTime(i.verifiedAt.UTC())
	}
	if i.disabledAt != nil {
		o.DisabledAt = chn.NewOptDateTime(i.disabledAt.UTC())
	}
	return o
}

// readInstallation 读取本租户的安装（行级安全）并加行锁；先锁行再计算绑定数。
func readInstallation(ctx context.Context, tx pgx.Tx, where string, args ...any) (*installation, error) {
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM chn_installations i WHERE `+where+` FOR UPDATE`, args...).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, failure(404, "installation_not_found")
		}
		return nil, err
	}
	return scanInstallation(tx.QueryRow(ctx, `SELECT `+installationColumns+` FROM chn_installations i WHERE i.id = $1`, id))
}

func mustURL(raw string) url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		return url.URL{}
	}
	return *u
}

// ListInstallations 列出本租户的渠道安装（can_manage_members，只读不要求强认证）。
func (s *Service) ListInstallations(ctx context.Context) (*chn.InstallationList, error) {
	rc, err := s.member(ctx)
	if err != nil {
		return nil, err
	}
	ok, err := s.allowed(ctx, rc, "can_manage_members", "tenant:"+rc.Tenant)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, failure(403, "forbidden")
	}
	out := &chn.InstallationList{Installations: []chn.Installation{}}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+installationColumns+` FROM chn_installations i ORDER BY i.channel`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			i, err := scanInstallation(rows)
			if err != nil {
				return err
			}
			out.Installations = append(out.Installations, *i.out())
		}
		return rows.Err()
	})
	return out, err
}

// InstallChannel 开始（或重新开始）本租户在渠道的安装：管理员 + 强认证。
func (s *Service) InstallChannel(ctx context.Context, req *chn.InstallChannelRequest, key string) (*chn.InstallationStarted, error) {
	if req == nil || req.Validate() != nil || string(req.Channel) != Channel {
		return nil, failure(400, "invalid_request")
	}
	if err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	rc, _ := reqctx.From(ctx)
	state, digest, err := newToken()
	if err != nil {
		return nil, err
	}
	// 渠道适配器未接入时不写入任何状态
	authorizeURL, err := s.provider().AuthorizeURL(ctx, state)
	if err != nil {
		return nil, failure(503, "channel_unavailable")
	}
	out := &chn.InstallationStarted{}
	var fresh bool
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		fresh, err = command(ctx, tx, key, "installation.install/"+Channel, req, out, func() error {
			var id string
			var current string
			var version int64
			err := tx.QueryRow(ctx, `SELECT id, state, version FROM chn_installations WHERE channel = $1 FOR UPDATE`, Channel).Scan(&id, &current, &version)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				id, version = ids.New("chi"), 1
				if _, err := tx.Exec(ctx, `INSERT INTO chn_installations (id, tenant_id, channel, state, version) VALUES ($1, $2, $3, 'authorizing', 1)`, id, rc.Tenant, Channel); err != nil {
					return err
				}
			case err != nil:
				return err
			case current == "pending_verification" || current == "enabled":
				return failure(409, "channel_already_installed")
			default:
				// 授权中或已停用：重新开始授权，版本加一使此前的 state 失效；企业由新的授权决定
				version++
				if _, err := tx.Exec(ctx, `UPDATE chn_installations SET state = 'authorizing', version = $2, corp_id = NULL, corp_name = NULL, verified_at = NULL, updated_at = now() WHERE id = $1`, id, version); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO chn_install_states (digest, tenant_id, cell, installation_id, installation_version, expires_at) VALUES ($1, $2, $3, $4, $5, $6)`,
				digest, rc.Tenant, rc.Cell, id, version, s.businessNow().Add(domain.InstallStateTTL)); err != nil {
				return err
			}
			i, err := readInstallation(ctx, tx, `i.id = $1`, id)
			if err != nil {
				return err
			}
			out.Installation = *i.out()
			return record(ctx, tx, "channels.installation.authorizing", id, "can_manage_members")
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	if fresh {
		out.AuthorizeURL = chn.NewOptURI(mustURL(authorizeURL))
	}
	return out, nil
}

// InstallCallback 是渠道官方授权页的回调（Callbacks）：state 一次性、10分钟有效、只对安装的当前版本有效。
func (s *Service) InstallCallback(ctx context.Context, channel, state, code string) (*api.Installation, error) {
	if channel != Channel || state == "" || code == "" {
		return nil, failure(400, "invalid_request")
	}
	digest := tokenDigest(state)
	var tenant, cell, installationID string
	var installationVersion int64
	check := func(ctx context.Context, tx pgx.Tx, lock bool) error {
		q := `SELECT tenant_id, cell, installation_id, installation_version, expires_at, used_at IS NOT NULL FROM chn_install_states WHERE digest = $1`
		if lock {
			q += ` FOR UPDATE`
		}
		var expires time.Time
		var used bool
		err := tx.QueryRow(ctx, q, digest).Scan(&tenant, &cell, &installationID, &installationVersion, &expires, &used)
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(404, "install_state_not_found")
		}
		if err != nil {
			return err
		}
		if used {
			return failure(409, "install_state_used")
		}
		if !s.now().Before(expires) {
			return failure(410, "install_state_expired")
		}
		return nil
	}
	if err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error { return check(ctx, tx, false) }); err != nil {
		return nil, err
	}
	corp, err := s.provider().ExchangeInstallCode(ctx, code)
	if errors.Is(err, api.ErrInvalidCode) {
		return nil, failure(400, "install_code_invalid")
	}
	if err != nil {
		return nil, failure(503, "channel_unavailable")
	}
	out := &api.Installation{}
	tctx := serviceContext(ctx, tenant, cell)
	err = s.tx(tctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := check(ctx, tx, true); err != nil {
			return err
		}
		i, err := readInstallation(ctx, tx, `i.id = $1`, installationID)
		if err != nil {
			return err
		}
		if i.version != installationVersion || i.state != "authorizing" {
			return failure(409, "install_state_superseded")
		}
		tag, err := tx.Exec(ctx, `INSERT INTO chn_corp_routes (channel, corp_id, tenant_id, cell, installation_id) VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
			Channel, corp.CorpID, tenant, cell, installationID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return failure(409, "corp_installed_elsewhere")
		}
		if _, err := tx.Exec(ctx, `UPDATE chn_installations SET state = 'pending_verification', version = version + 1, corp_id = $2, corp_name = $3, updated_at = now() WHERE id = $1`,
			installationID, corp.CorpID, corp.Name); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE chn_install_states SET used_at = now() WHERE digest = $1`, digest); err != nil {
			return err
		}
		*out = api.Installation{ID: installationID, Tenant: tenant, State: "pending_verification", CorpID: corp.CorpID, Version: i.version + 1}
		return record(ctx, tx, "channels.installation.authorized", installationID, "channel.install_callback")
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// VerifyInstallation 启用待验证的安装：管理员 + 强认证，且管理员本人已在该安装下完成绑定（F4 第3步）。
func (s *Service) VerifyInstallation(ctx context.Context, id string, req *chn.VersionRequest, key string) (*chn.Installation, error) {
	if req == nil || req.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	if err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	rc, _ := reqctx.From(ctx)
	out := &chn.Installation{}
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := command(ctx, tx, key, "installation.verify/"+id, req, out, func() error {
			i, err := readInstallation(ctx, tx, `i.id = $1`, id)
			if err != nil {
				return err
			}
			if i.version != req.ExpectedVersion {
				return failure(409, "version_conflict")
			}
			if i.state != "pending_verification" {
				return failure(409, "installation_not_pending")
			}
			var bound bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chn_bindings WHERE installation_id = $1 AND subject = $2 AND state = 'bound')`, id, rc.Principal.ID).Scan(&bound); err != nil {
				return err
			}
			if !bound {
				return failure(409, "verification_binding_required")
			}
			if _, err := tx.Exec(ctx, `UPDATE chn_installations SET state = 'enabled', version = version + 1, verified_at = $2, updated_at = now() WHERE id = $1`, id, s.businessNow()); err != nil {
				return err
			}
			i, err = readInstallation(ctx, tx, `i.id = $1`, id)
			if err != nil {
				return err
			}
			*out = *i.out()
			return record(ctx, tx, "channels.installation.enabled", id, "can_manage_members")
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DisableInstallation 停用渠道：管理员 + 强认证。在同一事务中使该安装下全部 bound 的绑定变为 revoking（原因 channel_disabled），
// 作废未完成的绑定请求，解除企业路由；提交后逐个同步撤销免登会话（失败由事件与定时重试送达）。
func (s *Service) DisableInstallation(ctx context.Context, id string, req *chn.VersionRequest, key string) (*chn.Installation, error) {
	if req == nil || req.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	if err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	rc, _ := reqctx.From(ctx)
	out := &chn.Installation{}
	var revoked []*binding
	var fresh bool
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		fresh, err = command(ctx, tx, key, "installation.disable/"+id, req, out, func() error {
			i, err := readInstallation(ctx, tx, `i.id = $1`, id)
			if err != nil {
				return err
			}
			if i.version != req.ExpectedVersion {
				return failure(409, "version_conflict")
			}
			if i.state == "disabled" {
				return failure(409, "installation_disabled")
			}
			if _, err := tx.Exec(ctx, `UPDATE chn_installations SET state = 'disabled', version = version + 1, disabled_at = $2, updated_at = now() WHERE id = $1`, id, s.businessNow()); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM chn_corp_routes WHERE installation_id = $1`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE chn_binding_requests SET state = 'voided', void_reason = 'channel_disabled', updated_at = now() WHERE installation_id = $1 AND state IN ('initiated', 'pinned')`, id); err != nil {
				return err
			}
			revoked, err = s.invalidate(ctx, tx, `installation_id = $1`, []any{id}, "channel_disabled", "")
			if err != nil {
				return err
			}
			i, err = readInstallation(ctx, tx, `i.id = $1`, id)
			if err != nil {
				return err
			}
			*out = *i.out()
			return record(ctx, tx, "channels.installation.disabled", id, "can_manage_members")
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	if fresh {
		for _, b := range revoked {
			s.revokeAfterCommit(ctx, rc.Tenant, rc.Cell, b)
		}
	}
	return out, nil
}
