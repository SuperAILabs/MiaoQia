-- +goose Up
-- 信号的候选负责人（PRD 第6段 F1 第2条；customer-context.md 第11节；C-14）：生成信号时按目标的分派规则（生效策略读取中的分派配置）
-- 以第一条命中且被分派人仍是目标负责人或参与人的规则算出，只是建议。assignment_version 为所用的分派配置版本（配置过才有，从 1 开始）；
-- 为空表示生成时分派规则不可用（目标从未配置分派规则、C-14 之前的信号，或提供方未给出分派配置），读取时说明为 rules_unavailable。
ALTER TABLE det_signals ADD COLUMN candidate_owner text CHECK (candidate_owner IS NULL OR candidate_owner ~ '^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,127}$');
ALTER TABLE det_signals ADD COLUMN assignment_version bigint CHECK (assignment_version IS NULL OR assignment_version >= 1);
ALTER TABLE det_signals ADD CONSTRAINT det_signals_candidate_version CHECK (candidate_owner IS NULL OR assignment_version IS NOT NULL);

-- +goose Down
ALTER TABLE det_signals DROP CONSTRAINT det_signals_candidate_version;
ALTER TABLE det_signals DROP COLUMN assignment_version;
ALTER TABLE det_signals DROP COLUMN candidate_owner;
