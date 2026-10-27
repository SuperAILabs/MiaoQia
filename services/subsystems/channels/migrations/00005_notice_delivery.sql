-- +goose Up
-- C-11：转给决策者的提醒分页展开（不设总上限）。expand_after 为已处理到的成员（已绑定成员按 subject 升序的键集位置，服务端保存、不交给
-- 客户端）；expanded_recipients 为已展开的条数（累计）。每次投递认领至多处理一页，未处理完的立即重排到下一轮继续，处理完标为 expanded。
ALTER TABLE chn_notices ADD COLUMN expand_after text;
ALTER TABLE chn_notices ADD COLUMN expanded_recipients integer NOT NULL DEFAULT 0 CHECK (expanded_recipients >= 0);
-- 投递统计（getDeliveryStats 的 notices，按 created_at 窗口）
CREATE INDEX chn_notices_created ON chn_notices (tenant_id, created_at);

-- +goose Down
DROP INDEX chn_notices_created;
ALTER TABLE chn_notices DROP COLUMN expanded_recipients;
ALTER TABLE chn_notices DROP COLUMN expand_after;
