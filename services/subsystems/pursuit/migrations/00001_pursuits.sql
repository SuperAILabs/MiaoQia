-- +goose Up
-- 交易型跟进、信号关联、任务、提醒、结果记录、操作与尝试、时间线（PRD 第6段；pursuit.md 第4–7节）。租户私有数据：行级安全按事务会话变量
-- app.tenant_id 过滤。时间：*_at 为平台记录时间（UTC）；截止与获知时间另以所填的值、精度与时区保存（due_value / due_precision / due_tz），
-- due_at 是截止的时点（只到日的取该日在所填时区的次日 00:00），只用于排序与提醒计划。

-- 跟进：同一目标、同一目标对象（target_key）至多一个进行中的跟进（部分唯一索引）。期望参与关系（负责人、协作者）与参与版本在本地保存，
-- identity 中的参与关系经授权同步写入，applied_version 为 identity 已应用的版本；sync_pending_since 为期望版本首次高于已应用版本的时间。
-- notify_* 是待发的分派通知（只在该参与版本同步完成后发布 pursuit.pursuit.assigned.v1）。
-- grants 记录当前每个参与成员的角色是在哪个参与版本授予的（pv）以及授予时该成员在 identity 的成员版本（mv）：成员以参与关系写入时
-- 须 applied_version >= pv（identity 中可能仍是旧版本的同名关系），成员退出事件的版本不高于 mv 时是授予之前的旧事件。
CREATE TABLE pur_pursuits (
    id                  text PRIMARY KEY CHECK (id ~ '^pur_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id           text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    objective_id        text NOT NULL CHECK (objective_id ~ '^obj_[0-9A-HJKMNP-TV-Z]{26}$'),
    org_unit_id         text NOT NULL,
    template_id         text NOT NULL,
    kind                text NOT NULL DEFAULT 'deal' CHECK (kind IN ('deal')),
    fact_type           text NOT NULL,
    subject_id          text NOT NULL,
    part_id             text,
    winner_org_id       text,
    target_key          text NOT NULL,
    source_kind         text NOT NULL CHECK (source_kind IN ('signal','manual')),
    source_signal_id    text,
    source_content_version bigint,
    source_signal_created_at timestamptz,
    channel             text NOT NULL CHECK (channel IN ('web','wecom')),
    previous_pursuit_id text,
    state               text NOT NULL CHECK (state IN ('open','closed','voided')),
    owner               text NOT NULL,
    owner_valid         boolean NOT NULL DEFAULT true,
    collaborators       text[] NOT NULL DEFAULT '{}',
    note                text,
    created_by          text NOT NULL,
    create_operation_id text NOT NULL,
    version             bigint NOT NULL CHECK (version >= 1),
    participation_version bigint NOT NULL CHECK (participation_version >= 1),
    applied_version     bigint NOT NULL DEFAULT 0 CHECK (applied_version >= 0),
    sync_pending_since  timestamptz,
    notify_version      bigint,
    notify_assignee     text,
    notify_by           text,
    notify_kind         text CHECK (notify_kind IN ('created','reassigned')),
    result              text CHECK (result IN ('success','failure','abandoned')),
    current_outcome_id  text,
    changed_by          text NOT NULL,
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    closed_at           timestamptz,
    voided_at           timestamptz,
    grants              jsonb NOT NULL DEFAULT '{}'::jsonb,
    CHECK ((source_kind = 'signal') = (source_signal_id IS NOT NULL)),
    CHECK ((state = 'closed') = (closed_at IS NOT NULL AND result IS NOT NULL)),
    CHECK ((state = 'voided') = (voided_at IS NOT NULL)),
    CHECK ((notify_version IS NULL) = (notify_assignee IS NULL))
);
CREATE UNIQUE INDEX pur_pursuits_open_target ON pur_pursuits(tenant_id, objective_id, target_key) WHERE state = 'open';
CREATE INDEX pur_pursuits_owner ON pur_pursuits(tenant_id, owner) WHERE state = 'open';
CREATE INDEX pur_pursuits_objective ON pur_pursuits(tenant_id, objective_id, state);

-- 信号与跟进的关联（pursuit 拥有）：同一跟进与同一信号只有一条有效关联。M2 只随撤销创建解除。
CREATE TABLE pur_signal_links (
    id                   text PRIMARY KEY CHECK (id ~ '^psl_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id            text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    pursuit_id           text NOT NULL REFERENCES pur_pursuits(id),
    signal_id            text NOT NULL CHECK (signal_id ~ '^sig_[0-9A-HJKMNP-TV-Z]{26}$'),
    seen_content_version bigint NOT NULL CHECK (seen_content_version >= 0),
    method               text NOT NULL CHECK (method IN ('create','link_existing')),
    operation_id         text NOT NULL,
    state                text NOT NULL CHECK (state IN ('active','removed')),
    linked_at            timestamptz NOT NULL,
    removed_at           timestamptz,
    CHECK ((state = 'removed') = (removed_at IS NOT NULL))
);
CREATE UNIQUE INDEX pur_signal_links_active ON pur_signal_links(pursuit_id, signal_id) WHERE state = 'active';

-- 任务：截止保存所填的值、精度与时区；截止版本每次改变截止加一（提醒按任务 + 截止版本 + 种类幂等）。
CREATE TABLE pur_tasks (
    id               text PRIMARY KEY CHECK (id ~ '^tsk_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id        text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    pursuit_id       text NOT NULL REFERENCES pur_pursuits(id),
    title            text NOT NULL,
    assignee         text NOT NULL,
    due_value        text,
    due_precision    text CHECK (due_precision IN ('day','minute')),
    due_tz           text,
    due_at           timestamptz,
    note             text,
    status           text NOT NULL CHECK (status IN ('todo','done','cancelled')),
    version          bigint NOT NULL CHECK (version >= 1),
    deadline_version bigint NOT NULL CHECK (deadline_version >= 1),
    created_at       timestamptz NOT NULL,
    completed_at     timestamptz,
    CHECK ((due_value IS NULL) = (due_precision IS NULL) AND (due_value IS NULL) = (due_tz IS NULL) AND (due_value IS NULL) = (due_at IS NULL)),
    CHECK ((status = 'done') = (completed_at IS NOT NULL))
);
CREATE INDEX pur_tasks_pursuit ON pur_tasks(tenant_id, pursuit_id);
CREATE INDEX pur_tasks_assignee ON pur_tasks(tenant_id, assignee, due_at) WHERE status = 'todo';

-- 提醒（持久定时器）：每个任务、截止版本与种类一条；到时由 Reminders.RunDue 按当前值再检查后发布 pursuit.task.due.v1（只一次）。
-- cancelled_by 为取消它的操作（撤销完成时恢复由该完成取消的提醒）。
CREATE TABLE pur_reminders (
    id               text PRIMARY KEY CHECK (id ~ '^rmd_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id        text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    task_id          text NOT NULL REFERENCES pur_tasks(id),
    pursuit_id       text NOT NULL REFERENCES pur_pursuits(id),
    deadline_version bigint NOT NULL CHECK (deadline_version >= 1),
    kind             text NOT NULL CHECK (kind IN ('before_24h','at_due','day_before_0900','due_day_0900','overdue_next_day_0900')),
    fire_at          timestamptz NOT NULL,
    state            text NOT NULL CHECK (state IN ('planned','sent','cancelled')),
    cancelled_by     text,
    escalated        boolean,
    recipient        text,
    sent_at          timestamptz,
    UNIQUE (task_id, deadline_version, kind),
    CHECK ((state = 'sent') = (sent_at IS NOT NULL))
);
CREATE INDEX pur_reminders_due ON pur_reminders(tenant_id, fire_at) WHERE state = 'planned';

-- 结果记录（追加式）：修订追加新记录并引用被取代的记录。
CREATE TABLE pur_outcomes (
    id              text PRIMARY KEY CHECK (id ~ '^otc_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id       text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    pursuit_id      text NOT NULL REFERENCES pur_pursuits(id),
    content         jsonb NOT NULL,
    supersedes      text REFERENCES pur_outcomes(id),
    recorded_by     text NOT NULL,
    recorded_at     timestamptz NOT NULL,
    UNIQUE (supersedes)
);
CREATE INDEX pur_outcomes_pursuit ON pur_outcomes(tenant_id, pursuit_id, recorded_at);

-- 操作（追加式）：每个操作标识最多提交一次，确定结果（已创建、已存在、成功、无变化、确定失败）固定在操作标识上；
-- fingerprint 为请求内容的摘要（不同请求或他人使用同一操作标识为 409 operation_conflict）。response 为固定的结果（不含权限投影），
-- parts 为撤销所需的前值（撤销完成）。
CREATE TABLE pur_operations (
    tenant_id     text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    operation_id  text NOT NULL CHECK (operation_id ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{7,191}$'),
    actor         text NOT NULL,
    fingerprint   text NOT NULL,
    action        text NOT NULL,
    pursuit_id    text,
    task_id       text,
    channel       text NOT NULL CHECK (channel IN ('web','wecom')),
    outcome       text NOT NULL CHECK (outcome IN ('created','existing_linked','existing','succeeded','no_change','failed')),
    status        integer NOT NULL,
    code          text,
    response      jsonb NOT NULL,
    parts         jsonb NOT NULL DEFAULT '{}',
    recorded_at   timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, operation_id),
    CHECK ((outcome = 'failed') = (code IS NOT NULL))
);

-- 尝试：一次请求（幂等键按调用者与接口区分），同键重发返回该尝试的结果（含失败与暂时失败）。
CREATE TABLE pur_attempts (
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

-- 时间线（追加式，平台记录时间）。
CREATE TABLE pur_timeline (
    seq          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id    text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    pursuit_id   text NOT NULL REFERENCES pur_pursuits(id),
    kind         text NOT NULL,
    actor        text NOT NULL,
    detail       text,
    recorded_at  timestamptz NOT NULL
);
CREATE INDEX pur_timeline_pursuit ON pur_timeline(tenant_id, pursuit_id, seq);

ALTER TABLE pur_pursuits ENABLE ROW LEVEL SECURITY;
ALTER TABLE pur_signal_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE pur_tasks ENABLE ROW LEVEL SECURITY;
ALTER TABLE pur_reminders ENABLE ROW LEVEL SECURITY;
ALTER TABLE pur_outcomes ENABLE ROW LEVEL SECURITY;
ALTER TABLE pur_operations ENABLE ROW LEVEL SECURITY;
ALTER TABLE pur_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE pur_timeline ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pur_pursuits USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON pur_signal_links USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON pur_tasks USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON pur_reminders USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON pur_outcomes USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON pur_operations USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON pur_attempts USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON pur_timeline USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- +goose ENVSUB ON
REVOKE UPDATE, DELETE, TRUNCATE ON pur_outcomes, pur_operations, pur_attempts, pur_timeline FROM ${MIAOQIA_DB_APP_ROLE};
REVOKE DELETE, TRUNCATE ON pur_pursuits, pur_signal_links, pur_tasks, pur_reminders FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
DROP TABLE pur_timeline;
DROP TABLE pur_attempts;
DROP TABLE pur_operations;
DROP TABLE pur_outcomes;
DROP TABLE pur_reminders;
DROP TABLE pur_tasks;
DROP TABLE pur_signal_links;
DROP TABLE pur_pursuits;
