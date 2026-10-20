-- +goose Up
-- 信号、匹配记录、处理与反馈（PRD 第3段；detection.md 第4、12节）。租户私有数据：行级安全按事务会话变量 app.tenant_id 过滤。
-- 时间：evaluated_at 是业务时间（逻辑求值时间，触发事件的时间）；created_at 是记录时间（信号写入时间，即发现时间）；
-- occurred_at 是处理操作的业务时间，recorded_at 是写入时间。

-- 视图快照：按内容哈希保存求值时组装的视图（事实视图字段、关联视图项与证据），供 ExplainSignal 返回视图项与版本。内容不可变。
CREATE TABLE det_view_snapshots (
    tenant_id  text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    hash       text NOT NULL CHECK (hash ~ '^sha256:[0-9a-f]{64}$'),
    content    jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, hash)
);

-- 匹配记录：一个目标对一个求值单位版本的一次求值（含 no_signal 与求值成本超限），是“每个事实版本最多求值一次”的依据：
-- 主键不含策略代次，激活或回滚后重复投递的同一事实版本不按新代次再求值，也不重算已有信号（PRD 第7段第5节）。
-- 保留期不短于漏报诊断所需（PRD 第3段第13节第6项：从首次激活起至少 180 天）。
CREATE TABLE det_matches (
    tenant_id        text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    objective_id     text NOT NULL,
    unit_key         text NOT NULL,
    subject_version  bigint NOT NULL CHECK (subject_version >= 1),
    policy_id        text NOT NULL,
    policy_version   bigint NOT NULL CHECK (policy_version >= 1),
    generation       bigint NOT NULL CHECK (generation >= 1),
    context_version  bigint NOT NULL CHECK (context_version >= 1),
    outcome          text NOT NULL CHECK (outcome IN ('pursue_candidate','review_candidate','watch_signal','excluded','no_signal','cost_exceeded','not_triggered')),
    signal_id        text,
    trigger_event_id text NOT NULL,
    evaluated_at     timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, objective_id, unit_key, subject_version)
);

-- 信号：同一目标对同一求值单位只有一条信号（unit_key 唯一）。固定的求值输入（策略标识、版本、代次、模板与启用领域集合、上下文
-- 快照版本、视图快照哈希、触发事件、逻辑求值时间）写入后不变；内容版本随重新求值的更新记录递增；处理状态与人工处置共用状态版本。
CREATE TABLE det_signals (
    id               text PRIMARY KEY CHECK (id ~ '^sig_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id        text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    objective_id     text NOT NULL,
    objective_code   text NOT NULL,
    org_unit_id      text NOT NULL,
    policy_id        text NOT NULL,
    policy_version   bigint NOT NULL CHECK (policy_version >= 1),
    generation       bigint NOT NULL CHECK (generation >= 1),
    template_id      text NOT NULL,
    template_version text NOT NULL,
    enabled_domains  text[] NOT NULL,
    unit_key         text NOT NULL,
    fact_type        text NOT NULL,
    subject_id       text NOT NULL,
    subject_version  bigint NOT NULL CHECK (subject_version >= 1),
    part_id          text,
    title            text NOT NULL,
    winner_org_id    text,
    winner_name      text,
    amount_value     text,
    amount_currency  text,
    amount_cny       bigint,
    location         text,
    categories       text[] NOT NULL DEFAULT '{}',
    lot_key          text,
    disposition      text NOT NULL CHECK (disposition IN ('pursue_candidate','review_candidate','watch_signal','excluded')),
    score            integer NOT NULL,
    needs_review     boolean NOT NULL,
    reason_code      text,
    conclusion       text NOT NULL,
    reasons          text[] NOT NULL DEFAULT '{}',
    unknown_count    integer NOT NULL CHECK (unknown_count >= 0),
    trace            jsonb NOT NULL,
    features         jsonb NOT NULL,
    view_snapshot_hash text NOT NULL,
    context_version  bigint NOT NULL CHECK (context_version >= 1),
    trigger_event_id text NOT NULL,
    evaluated_at     timestamptz NOT NULL,
    created_at       timestamptz NOT NULL,
    backfill         boolean NOT NULL DEFAULT false,
    content_version  bigint NOT NULL DEFAULT 0 CHECK (content_version >= 0),
    material_updated_at timestamptz,
    status           text NOT NULL DEFAULT 'open' CHECK (status IN ('open','triaged','converted','dismissed','closed')),
    manual_disposition text CHECK (manual_disposition IN ('pursue_candidate','review_candidate','watch_signal')),
    manual_reason    text,
    state_version    bigint NOT NULL DEFAULT 0 CHECK (state_version >= 0),
    CHECK (disposition <> 'excluded' OR reason_code IS NOT NULL),
    CHECK (disposition <> 'excluded' OR manual_disposition IS NULL),
    CHECK ((status = 'triaged') <= (manual_disposition IS NOT NULL)),
    UNIQUE (tenant_id, objective_id, unit_key)
);
CREATE INDEX det_signals_inbox ON det_signals(tenant_id, objective_id, score DESC, created_at DESC, id DESC);
CREATE INDEX det_signals_lot ON det_signals(tenant_id, objective_id, lot_key) WHERE lot_key IS NOT NULL;

-- 信号更新（追加式）：重新求值的记录。重新求值的生产者随视图触发与撤回实现（A02 结果视图目前只在首次发布时求值）。
CREATE TABLE det_signal_updates (
    tenant_id          text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    signal_id          text NOT NULL REFERENCES det_signals(id),
    content_version    bigint NOT NULL CHECK (content_version >= 1),
    trigger            text NOT NULL,
    changed_fields     text[] NOT NULL DEFAULT '{}',
    disposition_before text NOT NULL,
    disposition_after  text NOT NULL,
    material           boolean NOT NULL,
    recorded_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (signal_id, content_version)
);

-- 成员的当前反馈：每人每条信号一行，反馈版本按（信号、成员）每次改变递增；撤回为 result 为空（版本照常递增）。
-- 全部变化另以处理记录追加保存（det_triage_ops）。
CREATE TABLE det_feedback (
    tenant_id   text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    signal_id   text NOT NULL REFERENCES det_signals(id),
    member      text NOT NULL,
    result      text CHECK (result IN ('relevant','irrelevant','exclusion_correct','exclusion_wrong')),
    reasons     text[] NOT NULL DEFAULT '{}',
    note        text,
    based_on_content_version bigint NOT NULL DEFAULT 0,
    version     bigint NOT NULL CHECK (version >= 1),
    updated_at  timestamptz NOT NULL,
    PRIMARY KEY (signal_id, member)
);

-- 操作（追加式）：每个操作标识最多提交一次，提交后的结果固定在操作标识上——成功（至少一个部分改变）、无变化与确定失败。
-- fingerprint 是操作内容的摘要：同一操作标识用于不同请求或由他人使用时 409 operation_conflict。parts 只含改变的部分
-- （前值、后值与写入后的版本）；response 是该操作的固定响应（成功为 TriageResult，失败为 Problem）。
CREATE TABLE det_triage_ops (
    seq              bigint GENERATED ALWAYS AS IDENTITY,
    tenant_id        text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    operation_id     text NOT NULL CHECK (operation_id ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{7,191}$'),
    actor            text NOT NULL,
    fingerprint      text NOT NULL,
    signal_id        text,
    objective_id     text,
    action           text NOT NULL CHECK (action IN ('feedback','withdraw_feedback','dismiss','restore','adjust','undo')),
    outcome          text NOT NULL CHECK (outcome IN ('succeeded','no_change','failed')),
    status           integer NOT NULL,
    code             text,
    response         jsonb NOT NULL,
    parts            jsonb NOT NULL DEFAULT '{}',
    reason           text,
    seen_content_version bigint NOT NULL DEFAULT 0,
    content_version  bigint NOT NULL DEFAULT 0,
    channel          text NOT NULL DEFAULT 'web' CHECK (channel IN ('web','wecom')),
    batch_id         text,
    undo_of          text,
    occurred_at      timestamptz NOT NULL,
    recorded_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, operation_id),
    CHECK ((action = 'undo') = (undo_of IS NOT NULL)),
    CHECK ((outcome = 'failed') = (code IS NOT NULL))
);
CREATE INDEX det_triage_ops_signal ON det_triage_ops(tenant_id, signal_id, seq DESC) WHERE outcome = 'succeeded';
CREATE INDEX det_triage_ops_batch ON det_triage_ops(tenant_id, batch_id) WHERE batch_id IS NOT NULL;

-- 尝试：一次请求（幂等键按调用者与接口区分）。同键重发返回该尝试的结果，包括失败；暂时失败（503，未提交）只记在尝试上，
-- 同一操作标识的新尝试（新键）照常执行。
CREATE TABLE det_triage_attempts (
    tenant_id   text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    actor       text NOT NULL,
    scope       text NOT NULL,
    idem_key    text NOT NULL,
    fingerprint text NOT NULL,
    status      integer NOT NULL,
    response    jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, actor, scope, idem_key)
);

-- 批量作业：固定的信号标识清单（1–200）与逐项结果。执行者以租约持有作业（lease_until 用数据库时钟）：租约过期而仍有未处理项
-- 时作业为 interrupted；取消后不再发起新的尝试。
CREATE TABLE det_signal_batches (
    id           text PRIMARY KEY CHECK (id ~ '^sgb_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id    text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    objective_id text NOT NULL,
    created_by   text NOT NULL,
    action       text NOT NULL CHECK (action IN ('relevant','irrelevant','dismiss','restore','exclusion_correct','exclusion_wrong')),
    reasons      text[] NOT NULL DEFAULT '{}',
    note         text,
    dismiss      boolean NOT NULL DEFAULT false,
    channel      text NOT NULL DEFAULT 'web',
    cancel_requested boolean NOT NULL DEFAULT false,
    lease_token  text,
    lease_until  timestamptz,
    created_at   timestamptz NOT NULL,
    updated_at   timestamptz NOT NULL
);

CREATE TABLE det_signal_batch_items (
    tenant_id   text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    batch_id    text NOT NULL REFERENCES det_signal_batches(id),
    position    integer NOT NULL CHECK (position >= 0 AND position < 200),
    signal_id   text NOT NULL,
    seen_content_version bigint NOT NULL,
    state_version bigint NOT NULL,
    feedback_version bigint NOT NULL,
    result      text NOT NULL DEFAULT 'unprocessed' CHECK (result IN ('succeeded','no_change','failed','unprocessed')),
    failure     text CHECK (failure IN ('definite','transient')),
    code        text,
    parts       jsonb,
    attempts    integer NOT NULL DEFAULT 0,
    PRIMARY KEY (batch_id, position),
    UNIQUE (batch_id, signal_id),
    CHECK ((result = 'failed') = (failure IS NOT NULL))
);

ALTER TABLE det_view_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE det_matches ENABLE ROW LEVEL SECURITY;
ALTER TABLE det_signals ENABLE ROW LEVEL SECURITY;
ALTER TABLE det_signal_updates ENABLE ROW LEVEL SECURITY;
ALTER TABLE det_feedback ENABLE ROW LEVEL SECURITY;
ALTER TABLE det_triage_ops ENABLE ROW LEVEL SECURITY;
ALTER TABLE det_triage_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE det_signal_batches ENABLE ROW LEVEL SECURITY;
ALTER TABLE det_signal_batch_items ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON det_view_snapshots USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON det_matches USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON det_signals USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON det_signal_updates USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON det_feedback USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON det_triage_ops USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON det_triage_attempts USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON det_signal_batches USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON det_signal_batch_items USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- +goose ENVSUB ON
REVOKE UPDATE, DELETE, TRUNCATE ON det_view_snapshots, det_matches, det_signal_updates, det_triage_ops, det_triage_attempts FROM ${MIAOQIA_DB_APP_ROLE};
REVOKE DELETE, TRUNCATE ON det_signals, det_feedback, det_signal_batches, det_signal_batch_items FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
DROP TABLE det_signal_batch_items;
DROP TABLE det_signal_batches;
DROP TABLE det_triage_attempts;
DROP TABLE det_triage_ops;
DROP TABLE det_feedback;
DROP TABLE det_signal_updates;
DROP TABLE det_signals;
DROP TABLE det_matches;
DROP TABLE det_view_snapshots;
