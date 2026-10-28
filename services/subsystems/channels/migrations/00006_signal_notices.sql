-- +goose Up
-- C-08（PRD 第5段 F7）：信号的即时通知。消费 detection.signal.detected.v1 时每条信号记一行（recipient 为 '*'，受众尚未确定）：投递时按目标
-- 当前生效策略 route 的 notify（mode 为 immediate）与目标当前的负责人、参与人展开为每名候选收件人一行（source 为信号标识），每条在发送前
-- 以收件人本人再检查（偏好、安静时段、成员、route 与受众、目标与信号的访问、处理状态、节流、并入简报）。即时通知没有跟进，pursuit_id 为空。
-- item 为信号卡片的渲染来源（条目快照，来源字段已按渲染时的展示级别白名单写入）：重试前按当前来源权利收紧后重新渲染正文，按钮与令牌不变。
ALTER TABLE chn_notices ALTER COLUMN pursuit_id DROP NOT NULL;
ALTER TABLE chn_notices ADD COLUMN signal_id text CHECK (signal_id ~ '^sig_[0-9A-HJKMNP-TV-Z]{26}$');
ALTER TABLE chn_notices ADD COLUMN objective_id text CHECK (objective_id ~ '^obj_[0-9A-HJKMNP-TV-Z]{26}$');
ALTER TABLE chn_notices ADD COLUMN item jsonb;
ALTER TABLE chn_notices DROP CONSTRAINT chn_notices_kind_check;
ALTER TABLE chn_notices ADD CONSTRAINT chn_notices_kind_check CHECK (kind IN ('task_due', 'authorization_rejected', 'signal_immediate'));
ALTER TABLE chn_notices ADD CONSTRAINT chn_notices_signal_check
    CHECK ((kind = 'signal_immediate') = (signal_id IS NOT NULL AND objective_id IS NOT NULL) AND (kind = 'signal_immediate') = (pursuit_id IS NULL));
-- 组装简报时按收件人与信号查找即时通知（已通知 / 并入简报），节流按收件人统计最近送达的即时通知
CREATE INDEX chn_notices_signal ON chn_notices (tenant_id, recipient, signal_id) WHERE kind = 'signal_immediate';
CREATE INDEX chn_notices_signal_accepted ON chn_notices (tenant_id, recipient, accepted_at) WHERE kind = 'signal_immediate' AND state = 'delivered';

-- 成员是否接收即时通知（缺省是）。与其他偏好不同，按最新版本立即生效（不按周期）。
ALTER TABLE chn_briefing_preferences ADD COLUMN instant_notify boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE chn_briefing_preferences DROP COLUMN instant_notify;
DROP INDEX chn_notices_signal_accepted;
DROP INDEX chn_notices_signal;
ALTER TABLE chn_notices DROP CONSTRAINT chn_notices_signal_check;
ALTER TABLE chn_notices DROP CONSTRAINT chn_notices_kind_check;
ALTER TABLE chn_notices ADD CONSTRAINT chn_notices_kind_check CHECK (kind IN ('task_due', 'authorization_rejected'));
ALTER TABLE chn_notices DROP COLUMN item;
ALTER TABLE chn_notices DROP COLUMN objective_id;
ALTER TABLE chn_notices DROP COLUMN signal_id;
ALTER TABLE chn_notices ALTER COLUMN pursuit_id SET NOT NULL;
