-- +goose Up
-- 卡片上的“跟进”（PRD 第5段第12节第26条延后部分、第6段 F1 第1条与 F9 第4条）：动作令牌追加动作 pursue。令牌标识即创建跟进的
-- 操作标识；按钮只在租户开启跟进入口（发布开关）且收件人有目标的 can_assign 时出现。
ALTER TABLE chn_action_tokens DROP CONSTRAINT chn_action_tokens_action_check;
ALTER TABLE chn_action_tokens ADD CONSTRAINT chn_action_tokens_action_check CHECK (action IN ('relevant', 'irrelevant', 'undo', 'pursue'));

-- +goose Down
ALTER TABLE chn_action_tokens DROP CONSTRAINT chn_action_tokens_action_check;
ALTER TABLE chn_action_tokens ADD CONSTRAINT chn_action_tokens_action_check CHECK (action IN ('relevant', 'irrelevant', 'undo'));
