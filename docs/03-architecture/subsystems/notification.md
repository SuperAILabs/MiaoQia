# 通知与渠道子系统（notification）

> 状态：评审中 · 版本：1.0 · 更新：2026-10-12 · 平面：交互 · 首期部署单元：`platform`

## 1. 使命

在正确的时间、通过正确的渠道、以正确的密度把信息送到正确的人，并让用户能在渠道内完成低风险的快捷动作。

## 2. 职责与非职责

**职责**

- 通知编排：根据事件与订阅规则生成通知；
- 偏好与频率：渠道偏好、安静时段、合并规则、摘要周期、退订；
- 摘要：日报、周报、管理层摘要；
- 渠道投递：企业微信（应用消息、卡片）、邮件、短信（仅限安全类通知）、站内信，后续可扩展钉钉、飞书；
- 快捷动作：一次性动作令牌（绑定租户、用户、对象、动作、过期时间），支持反馈、分派、确认；
- 回复式动作：邮件回复中的白名单低风险指令；
- 投递记录与渠道健康：发送、失败、重复、延迟、退订。

**非职责**

- 不决定业务上“什么值得通知”（由处置、判断与推进事件携带的优先级决定，本子系统只负责编排与投递）；
- 不在渠道内执行高风险动作（策略激活、批量导出、对外发送必须回到强认证界面）。

## 3. 内部组件

`orchestrator`（规则与合并）、`preferences`、`digest_builder`、`channel_adapters`（企业微信、邮件、短信、站内）、`action_tokens`、`inbound`（回调与邮件回复解析）、`delivery_log`。

## 4. 拥有的数据（租户域）

`NotificationRule`、`Preference`、`Notification`、`Delivery`（幂等键：`notification_id + channel + recipient`）、`ActionToken`、`ChannelBinding`（企业微信账号与平台用户绑定）。

## 5. 接口

`GetPreferences` / `UpdatePreferences`、`ListNotifications`、`ExecuteAction`（由动作令牌触发，转调对应子系统的命令接口）、`BindChannel`。

## 6. 事件

**消费**：`detection.signal.detected.v1`、`assessment.assessment.created.v1`、`assessment.decision.drafted.v1`、`identity.approval.requested.v1`、`pursuit.task.assigned.v1`、`pursuit.task.due.v1`、`detection.pursuit_related_event.v1`、`research.case.completed.v1`、`research.case.awaiting_input.v1`。

**发布**：`notification.delivery.sent.v1`、`notification.delivery.failed.v1`、`notification.action.executed.v1`。

## 7. 扩展点

- 渠道适配器 SPI（新增即时通讯渠道）；
- 卡片模板（应用领域包提供领域卡片）。

## 8. 安全要点

- 卡片只展示低敏摘要，详情通过短时授权链接查看；
- 动作令牌一次性、短时效、绑定用户与对象，执行时再次进行授权检查；
- 邮件回复指令只接受白名单低风险动作，来源地址必须已验证；
- 只使用企业微信等平台的官方能力，不使用个人号自动化。
