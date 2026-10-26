-- +goose Up
-- 对象页的本租户信号分区（PRD 第4段 F3–F5；detection.md 第5节 ListSignalsForObject）：信号记录求值单位所在的采购项目与采购人
-- （来自信号视图的展示字段，写入后不变）。之前生成的信号没有这两列的值，不出现在按采购项目或采购人的查询中（中标方照常）。
ALTER TABLE det_signals ADD COLUMN procurement_project_id text CHECK (procurement_project_id ~ '^pp_[0-9A-HJKMNP-TV-Z]{26}$');
ALTER TABLE det_signals ADD COLUMN buyer_org_id text CHECK (buyer_org_id ~ '^org_[0-9A-HJKMNP-TV-Z]{26}$');
ALTER TABLE det_signals ADD COLUMN buyer_name text;
CREATE INDEX det_signals_project ON det_signals(tenant_id, procurement_project_id, created_at DESC, id DESC) WHERE procurement_project_id IS NOT NULL;
CREATE INDEX det_signals_winner ON det_signals(tenant_id, winner_org_id, created_at DESC, id DESC) WHERE winner_org_id IS NOT NULL;
CREATE INDEX det_signals_buyer ON det_signals(tenant_id, buyer_org_id, created_at DESC, id DESC) WHERE buyer_org_id IS NOT NULL;

-- +goose Down
DROP INDEX det_signals_buyer;
DROP INDEX det_signals_winner;
DROP INDEX det_signals_project;
ALTER TABLE det_signals DROP COLUMN buyer_name;
ALTER TABLE det_signals DROP COLUMN buyer_org_id;
ALTER TABLE det_signals DROP COLUMN procurement_project_id;
