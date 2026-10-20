-- +goose Up
-- 审批请求（PRD 第7段 F4、F5；identity.md 第5节“审批”与 7.3）。请求绑定不可变的对象版本与基准（binding），批准只针对它；
-- 状态变化（决定、撤回、过期、失效）在请求行锁下按版本串行，每次发布一条 identity.approval.*.v1。
CREATE TABLE identity_approvals (
 id text PRIMARY KEY CHECK (id ~ '^apr_[0-9A-HJKMNP-TV-Z]{26}$'),
 tenant_id text NOT NULL REFERENCES identity_tenants(id),
 type text NOT NULL CHECK (type IN ('policy_activation','policy_rollback')),
 resource text NOT NULL,
 applicant text NOT NULL,
 note text NOT NULL CHECK (length(note) BETWEEN 1 AND 2000),
 binding jsonb NOT NULL,
 state text NOT NULL CHECK (state IN ('pending','approved','rejected','withdrawn','expired','invalidated')),
 version bigint NOT NULL CHECK (version >= 1),
 -- 业务时间：提交时间与最长有效期（提交起7天）
 submitted_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL,
 decided_by text,
 decided_at timestamptz,
 decision_reason text,
 closed_reason text CHECK (closed_reason IN ('withdrawn','expired','applicant_left','applicant_lost_permission','baseline_changed','object_changed')),
 closed_at timestamptz,
 -- 记录时间
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK ((state IN ('approved','rejected')) = (decided_by IS NOT NULL AND decided_at IS NOT NULL)),
 CHECK ((state IN ('withdrawn','expired','invalidated')) = (closed_reason IS NOT NULL AND closed_at IS NOT NULL)),
 CHECK (state <> 'rejected' OR decision_reason IS NOT NULL),
 CHECK (decided_by IS NULL OR decided_by <> applicant)
);
-- 同一资源同时只能有一个待审批请求
CREATE UNIQUE INDEX identity_approvals_pending_resource ON identity_approvals(resource) WHERE state = 'pending';
CREATE INDEX identity_approvals_tenant ON identity_approvals(tenant_id, created_at DESC, id DESC);
CREATE INDEX identity_approvals_applicant ON identity_approvals(tenant_id, applicant, created_at DESC, id DESC);
CREATE INDEX identity_approvals_due ON identity_approvals(expires_at) WHERE state = 'pending';

-- +goose Down
DROP TABLE identity_approvals;
