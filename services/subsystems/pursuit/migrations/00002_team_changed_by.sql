-- +goose Up
-- 写入当前参与版本的成员（创建者、改派者或撤销者；系统按成员变化或 identity 拒绝写入的版本为 svc:pursuit）。identity 拒绝该版本中的
-- 部分成员时据此通知操作者（PRD 第6段 F12 第6条，pursuit.pursuit.authorization_rejected.v1 的 operator）；旧行为空（不通知）。
ALTER TABLE pur_pursuits ADD COLUMN team_changed_by text;

-- +goose Down
ALTER TABLE pur_pursuits DROP COLUMN team_changed_by;
