-- +goose Up
-- 持续跟踪的归档与对象来源（PRD 第6段 F10 第1–3条；C-13）。
-- 生命周期追加已归档：已停止 → 已归档（只读，不在缺省列表中），取消归档回到已停止。stopped_at 在已停止与已归档时都有值（归档不改停止
-- 时点），archived_at 只在已归档时有值。
ALTER TABLE pur_tracking DROP CONSTRAINT pur_tracking_state_check;
ALTER TABLE pur_tracking ADD CONSTRAINT pur_tracking_state_check CHECK (state IN ('tracking','paused','stopped','archived'));
ALTER TABLE pur_tracking DROP CONSTRAINT pur_tracking_check1;
ALTER TABLE pur_tracking ADD COLUMN archived_at timestamptz;
ALTER TABLE pur_tracking ADD CONSTRAINT pur_tracking_stopped_check CHECK ((state IN ('stopped','archived')) = (stopped_at IS NOT NULL));
ALTER TABLE pur_tracking ADD CONSTRAINT pur_tracking_archived_check CHECK ((state = 'archived') = (archived_at IS NOT NULL));

-- 对象的来源：组织标识 → {"v": 加入对象集合时的成员版本, "s": manual 手选 | import 名单导入}。本迁移之前加入的对象没有加入版本
-- （不猜测），来源记为 manual（之前只有手选）。结束一期时与实体图中的来源版本一起写入该期的 objects。
ALTER TABLE pur_tracking ADD COLUMN member_sources jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(member_sources) = 'object');
UPDATE pur_tracking SET member_sources = (SELECT COALESCE(jsonb_object_agg(m, jsonb_build_object('s', 'manual')), '{}'::jsonb) FROM unnest(members) AS m);

-- 期末成员快照中每个对象的加入版本、来源与实体来源版本（[{org_id, added_members_version?, source, entity_state, entity_version?}]，
-- 顺序同 members）。本迁移之前结束的期为 NULL（没有记录，不是“没有对象”）。
ALTER TABLE pur_tracking_periods ADD COLUMN objects jsonb CHECK (objects IS NULL OR jsonb_typeof(objects) = 'array');

-- +goose Down
ALTER TABLE pur_tracking_periods DROP COLUMN objects;
ALTER TABLE pur_tracking DROP COLUMN member_sources;
ALTER TABLE pur_tracking DROP CONSTRAINT pur_tracking_archived_check;
ALTER TABLE pur_tracking DROP CONSTRAINT pur_tracking_stopped_check;
UPDATE pur_tracking SET state = 'stopped' WHERE state = 'archived';
ALTER TABLE pur_tracking DROP COLUMN archived_at;
ALTER TABLE pur_tracking ADD CONSTRAINT pur_tracking_check1 CHECK ((state = 'stopped') = (stopped_at IS NOT NULL));
ALTER TABLE pur_tracking DROP CONSTRAINT pur_tracking_state_check;
ALTER TABLE pur_tracking ADD CONSTRAINT pur_tracking_state_check CHECK (state IN ('tracking','paused','stopped'));
