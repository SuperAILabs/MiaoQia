-- +goose Up
-- 授权同步告警与转交提醒的转回（PRD 第6段 F12 第2、4、6条；C-17）。
-- sync_delayed_at / sync_failed_at：当前待处理期间（sync_pending_since 相同）已发布 authorization_sync_delayed（超过 5 分钟）/
-- authorization_sync_failed（超过 24 小时）的时间；每个待处理期间各至多一次。同步完成（applied_version >= participation_version）时与
-- sync_pending_since 一起清空，告警过的发布 authorization_sync_recovered。NULL 表示该期间尚未告警（不是“检查过、没有问题”）。
ALTER TABLE pur_pursuits ADD COLUMN sync_delayed_at timestamptz;
ALTER TABLE pur_pursuits ADD COLUMN sync_failed_at timestamptz;
ALTER TABLE pur_pursuits ADD CONSTRAINT pur_pursuits_sync_alert_check
    CHECK ((sync_delayed_at IS NULL AND sync_failed_at IS NULL) OR sync_pending_since IS NOT NULL);
CREATE INDEX pur_pursuits_sync_pending ON pur_pursuits(tenant_id, sync_pending_since) WHERE applied_version < participation_version;

-- 转交提醒的转回：已发出的转给决策者的提醒（escalated），负责人恢复写入准入后同一提醒转回给负责人一次。handback 为结果：
-- sent 已转回（发布了带 handed_back 的 pursuit.task.due.v1）、skipped 不再转回（任务已完成或取消、截止已改、跟进已关闭、同一截止版本已有
-- 更晚的提醒发出）；NULL 为尚未决定（负责人仍不满足准入时留待下一轮）。handback_checked_at 为最近一次检查，用于轮转（不让长期未决的行
-- 挡住其他行）。
ALTER TABLE pur_reminders ADD COLUMN handback text CHECK (handback IN ('sent', 'skipped'));
ALTER TABLE pur_reminders ADD COLUMN handback_at timestamptz;
ALTER TABLE pur_reminders ADD COLUMN handback_checked_at timestamptz;
ALTER TABLE pur_reminders ADD CONSTRAINT pur_reminders_handback_state_check
    CHECK ((handback IS NULL) = (handback_at IS NULL) AND (handback IS NULL OR (state = 'sent' AND escalated)));
CREATE INDEX pur_reminders_handback ON pur_reminders(tenant_id, handback_checked_at NULLS FIRST, id) WHERE state = 'sent' AND escalated AND handback IS NULL;

-- +goose Down
DROP INDEX pur_reminders_handback;
ALTER TABLE pur_reminders DROP CONSTRAINT pur_reminders_handback_state_check;
ALTER TABLE pur_reminders DROP COLUMN handback_checked_at;
ALTER TABLE pur_reminders DROP COLUMN handback_at;
ALTER TABLE pur_reminders DROP COLUMN handback;
DROP INDEX pur_pursuits_sync_pending;
ALTER TABLE pur_pursuits DROP CONSTRAINT pur_pursuits_sync_alert_check;
ALTER TABLE pur_pursuits DROP COLUMN sync_failed_at;
ALTER TABLE pur_pursuits DROP COLUMN sync_delayed_at;
