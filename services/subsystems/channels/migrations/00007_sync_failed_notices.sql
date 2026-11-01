-- +goose Up
-- 授权同步失败的通知（PRD 第6段 F12 第6条；C-17）：消费 pursuit.pursuit.authorization_sync_failed.v1，每次失败（source 为“跟进/期望参与版本”）
-- 记一行收件人 '*' 的待展开通知；投递时以每个已绑定成员本人经 pursuit getPursuit / getTracking 判断（授权同步仍失败、跟进进行中、且
-- authorization.viewer_can_resolve 为真——在单元上能更新跟进的决策者），展开为各自的一行；每次发送前仍以收件人本人再检查。
ALTER TABLE chn_notices DROP CONSTRAINT chn_notices_kind_check;
ALTER TABLE chn_notices ADD CONSTRAINT chn_notices_kind_check
    CHECK (kind IN ('task_due', 'authorization_rejected', 'signal_immediate', 'authorization_sync_failed'));

-- +goose Down
DELETE FROM chn_notices WHERE kind = 'authorization_sync_failed';
ALTER TABLE chn_notices DROP CONSTRAINT chn_notices_kind_check;
ALTER TABLE chn_notices ADD CONSTRAINT chn_notices_kind_check CHECK (kind IN ('task_due', 'authorization_rejected', 'signal_immediate'));
