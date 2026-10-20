-- +goose Up
-- 策略标识（PRD 第3段；contracts/identifiers.yaml 的 pol_ 前缀，信号契约 intelligence/signal.schema.json 的 policy_ref.policy_id）：
-- 每个目标一个策略（策略 DSL 第2节），标识随策略状态行建立后不变。已有的行由目标标识确定性派生（md5 的十六进制大写字符
-- 均属 Crockford Base32 字母表，首位取 0 使其仍是合法的 ULID 文本）；之后建立的行由应用以 ids.New("pol") 分配。

ALTER TABLE ctx_policies ADD COLUMN policy_id text;
UPDATE ctx_policies SET policy_id = 'pol_0' || upper(substr(md5('ctx-policy/' || objective_id), 1, 25)) WHERE policy_id IS NULL;
ALTER TABLE ctx_policies ALTER COLUMN policy_id SET NOT NULL;
ALTER TABLE ctx_policies ADD CONSTRAINT ctx_policies_policy_id_format CHECK (policy_id ~ '^pol_[0-9A-HJKMNP-TV-Z]{26}$');
CREATE UNIQUE INDEX ctx_policies_policy_id ON ctx_policies(policy_id);

-- +goose Down
DROP INDEX ctx_policies_policy_id;
ALTER TABLE ctx_policies DROP CONSTRAINT ctx_policies_policy_id_format;
ALTER TABLE ctx_policies DROP COLUMN policy_id;
