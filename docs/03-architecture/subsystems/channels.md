# 渠道与会话子系统（channels）

> 状态：评审中 · 版本：3.9 · 更新：2026-10-27 · 平面：交互

## 1. 使命

让用户在自己日常使用的工作与通讯工具中，与妙洽完成**双向**交互：接收简报、信号与提醒，在渠道内直接向 Agent 提问、发起研究、反馈、分派和审批。各渠道接入同一套业务能力、权限与审计，新增渠道只需新增适配器。

设计参考了 OpenClaw 等开源项目的多渠道 Agent 网关模式：渠道只是 Agent 的入口与出口，Agent 能力与渠道解耦。是否直接复用此类开源组件，在技术选型专题中决定。

## 2. 渠道分批

| 批次 | 渠道 | 说明 |
|---|---|---|
| **首批** | 企业微信、飞书、钉钉、邮件 | 国内企业主流工作平台；邮件用于摘要、外部协作者与归档 |
| 后续 | Telegram、Discord、Slack 等海外即时通讯渠道 | 面向海外或特定客户；启用前按数据分级确认可发送的内容范围 |
| 持续 | Web 站内消息 | 始终可用，作为所有渠道的兜底 |

所有渠道从第一天起使用同一套渠道抽象与适配器接口。新增渠道是新增一个适配器，不修改业务子系统。

## 3. 职责与非职责

**职责**

- **出站通知**：根据事件与订阅规则生成通知；偏好、频率、安静时段、合并、摘要、退订；
- **简报**：按周期组装每日/每周简报及持续型推进的变化简报，保存简报历史；只经各子系统公开查询取数，选取与排序规则见 4.4；
- **渠道内对话**：接收用户在渠道中发给妙洽的消息（私聊或群聊中 @妙洽），识别用户与租户，维护会话，转交 Agent 处理，并把结果回传到同一会话；
- **快捷动作**：卡片按钮、菜单、回复指令触发的低风险动作（反馈、分派、关注、确认），以及审批提醒；
- **身份绑定**：渠道账号（企业微信、飞书、钉钉成员，已验证邮箱等）与平台用户的绑定与解绑；
- **消息渲染**：把渠道无关的消息表示渲染为各渠道格式，并按渠道能力降级；
- **投递与可靠性**：投递记录、重试、去重、限流、渠道健康。

**非职责**

- 不实现业务逻辑：渠道内的提问、反馈、审批都转为对业务子系统公开接口的调用，与 Web 完全一致；
- 不决定“什么值得通知”（由处置、判断、推进事件携带的优先级决定）；简报的分区、区内排序与配额规则由应用领域包声明，channels 只执行；
- 不在渠道内执行高影响动作（策略激活、批量导出、联系人使用、自动提交）和对外发送，这些必须回到强认证的 Web 界面；审批规则见 [identity 7.3](identity.md#73-高影响动作与审批)，其中对外发送不需审批，按白名单与内容范围执行。

## 4. 架构

```text
企业微信 / 飞书 / 钉钉 / 邮件 / …（后续 Telegram、Discord …）
        │ 回调、长连接、邮件收发
        ▼
┌──────────────────────── channels ────────────────────────┐
│ 渠道适配器（每渠道一个，实现 ChannelAdapter 接口）         │
│   ↓ 归一化为渠道无关的入站消息 / ↑ 渲染渠道格式            │
│ 身份绑定 → 会话管理 → 意图路由                             │
│   · 提问 / 研究请求 → research 的研究入口（同 Web“问妙洽”）│
│   · 快捷动作 → 对应子系统的命令接口                        │
│   · 查询 → gateway 公共查询能力                            │
│ 出站编排：事件 → 规则 → 合并/限频 → 渲染 → 投递            │
└───────────────────────────────────────────────────────────┘
```

### 4.1 渠道无关的消息表示

所有消息先表达为统一的结构，再由适配器渲染：

| 元素 | 说明 | 降级方式 |
|---|---|---|
| 标题、正文（Markdown 子集） | 文本内容 | 不支持 Markdown 的渠道转纯文本 |
| 卡片字段 | 关键字段的键值对 | 转为文本列表 |
| 按钮与动作 | 绑定动作令牌 | 转为带编号的回复指令或链接 |
| 证据与详情链接 | 短时授权链接 | 始终可用 |
| 附件 | 研究报告 PDF / Word | 不支持文件的渠道发送下载链接 |
| AI 生成标识 | 统一标识 | 始终保留 |

### 4.2 渠道能力声明

每个适配器声明其能力，渲染与交互据此选择：

`text` · `markdown` · `card` · `buttons` · `file` · `thread` · `group` · `mention` · `edit_message` · `inbound_conversation` · `stream_connection`（如长连接模式） · 速率限制

### 4.3 渠道内对话

```text
用户在飞书私聊妙洽：“某公司近三年在华东的中标情况和主要客户”
  → 飞书适配器 → 身份绑定得到 用户 / 租户
  → 会话管理：新建或延续会话（会话与研究案例关联）
  → research 研究入口：匹配研究模板或执行检索
  → 立即回复“已开始研究”卡片；完成后回传摘要卡片 + 报告链接 + 可选 PDF
  → 用户在同一会话中追问，延续同一研究案例
```

- 会话是渠道侧对话线程与平台研究案例之间的映射；研究案例本身由 `research` 拥有，同时出现在 Web 的研究列表中；
- 群聊中：只响应 @妙洽 的消息；以提问者身份执行权限检查；群内只发布摘要，详情通过需登录的链接查看，避免群内非授权成员获得完整内容。

### 4.4 简报

简报是“今日简报”导航与渠道早间推送的共同来源。组装规则：

1. **按区组织**：每个启用目标一个区（如“直接投标”“进入供货”“竞争情报”），另设“待我处理”（待审批、到期任务）与“关注变化”（持续型推进的对象变化，以及本人的个人关注，[detection 第 11 节](detection.md#11-关注)）两个跨目标区。不同目标的 `rank_score` 不混排，符合 [业务不变量](../../02-business/01-business-architecture.md#7-业务不变量)第 8 条；
2. **区内排序**：目标区按该目标策略的排序分；“待我处理”按截止时间；“关注变化”按变化时间；“待我处理”的到期任务来自 `pursuit.ListMyTasks`（分派给收件人、未完成、在下一期之前到期或已逾期；无有效负责人的列给目标范围的决策者），截止时间带精度与时区；
3. **配额与优先**：每区条数上限与区的顺序由应用领域包声明，用户可在偏好中调整；截止时间在 24 小时内的事项和更正类变化不受配额截断；
4. **去重**：同一目标对象在多个区出现时，只在优先级最高的区完整展示，其他区显示引用；
5. **空态与截至时间**：每区标明数据截至时间；无结果、来源中断、目标未启用、无权限分别说明，不以空列表代替；
6. **AI 摘要可选**：区内条目由确定性规则选出；可选的开头摘要经 `ai-platform` 生成并标注 AI 生成，摘要失败不影响简报发送；
7. **周期与受众**：日常简报按成员逐人组装，内容只来自其本人有权查看的目标与事项。周期是固定区间（上一次计划发送时间到本次计划发送时间），按成员偏好的时区计算，起止以带时区偏移的时刻保存；修改发送时间或时区从下一期生效，新旧周期首尾相接；
8. **目标区取数**：区的覆盖窗口为 [起点, 终点)，起点是该区上一次成功取数并随简报保存的终点，只有取数成功后才前移；某区取数失败或整期组装失败时窗口不前移，下一期覆盖未取数的部分，不遗漏、不重复。列入的信号须在组装时待处理，并且窗口内新发现（`created_at`，回填信号不算）或窗口内有实质更新（`detection` 在 `SignalUpdate` 上标记，时间轴为更新记录写入平台的时间）。同一信号无论新发现与更新、更新几次，每期每区只列一条，按组装时的当前内容版本渲染并列出窗口内的实质更新；组装时已被处理的只在区头计数。查询语义见 [detection](detection.md) `ListSignals`；
9. **排序列表版本**：每区记录所列条目的信号标识、内容版本、排序分与区内顺序，作为与 Top-K 对照的依据；
10. **发送前再检查**：组装与推送之间、以及每次重试前，按收件人当前状态检查成员有效、渠道绑定为“已绑定”、各目标仍可查看；失去访问的区与条目删除，成员或绑定失效的整期不推送到渠道，只留在站内与历史；
11. **空与部分**：全部区无条目时照常生成并保存，是否推送按偏好；有区来源中断或暂不可用时不算空简报。一区不可用不推迟其他区；
12. **投递与重新推送**：同一期简报只组装一次（幂等键见 `ComposeBriefing`）。投递失败按渠道退避重试，重试发送同一条已渲染的消息，卡片与动作令牌不变，重复送达的卡片上的同一按钮仍是同一个操作；重试截止后改投站内。本人对当日一期重新推送是新的投递，渲染新的卡片与新的动作令牌，新令牌上的点击是新的操作，按业务所有者的版本与状态规则重新判断，不假定为无变化；不重新组装。截止时间与默认值见 [PRD 第5段](../../01-product/prd/05-briefing-quick-actions.md)。

简报不是质量评估中的 Top-K 列表。Top-K 按“租户/目标/每周至多 20 条”计算（见 [质量与验收](../../05-delivery/02-quality-and-acceptance.md#10-指标执行口径)）；简报的目标区记录它对应的排序列表版本，便于两者对照，但简报的配额、去重和截断不改变 Top-K 的样本与口径。

### 4.5 通知合并

即时通知（信号、更新、任务、审批）在短时间内密集到达时按收件人合并，避免刷屏：

| 要素 | 规则 |
|---|---|
| 合并单位 | 按收件人逐人合并，不按群或目标合并；每人的合并内容只来自其本人有权查看的通知，不同授权受众不会因合并看到彼此的内容 |
| 合并窗口 | 同一收件人、同一渠道、同一合并组（同一目标对象，或同一目标的同类通知）在窗口内合并为一条；窗口长度由偏好与应用领域包声明 |
| 对象版本 | 同一对象在窗口内多次变化时只发最新版本，并列出变化摘要；每条明细记录所对应的对象版本 |
| 紧急更正 | 截止时间变化、更正与撤回，以及 24 小时内截止的事项不进入合并窗口，立即发送 |
| 幂等 | 合并通知的幂等键为租户 + 收件人 + 渠道 + 合并组 + 窗口起点；被合并的单条通知记录它并入了哪一条，重试不重复发送 |
| 失败重试 | 合并通知发送失败按渠道退避重试；重试时重新检查收件人权限与对象最新版本，权限已失去的明细删除，不发送过期版本 |

合并只影响即时通知；简报按 4.4 组装，不受合并影响。安静时段内的即时通知延后到安静时段结束，结束时仍待处理且当期简报即将发送的并入简报，不再单独发送。

### 4.6 卡片快捷动作

卡片按钮的动作是对业务子系统命令的调用，规则与 Web 相同（[PRD 第5段](../../01-product/prd/05-briefing-quick-actions.md) F4–F5）：

| 要素 | 规则 |
|---|---|
| 动作令牌 | 每个按钮一个令牌：租户、收件人、对象、动作、渲染时所见的对象内容版本、到期时间、所属卡片与投递。令牌不携带登录身份 |
| 操作与尝试 | 令牌标识即操作标识：同一按钮的多次点击、同一回调的重放、投递重试使同一张卡片重复送达，都是同一个操作，最多提交一次；本人重新推送的卡片使用新令牌，是新的操作。每次调用业务命令是一次尝试，带自己的幂等键；暂时失败（确认未提交）用新的幂等键、同一操作标识重试；调用前先按操作标识查找已提交的结果 |
| 令牌作废 | 只有进入业务调用后得到的成功、无变化或确定失败才固定在令牌上并使令牌作废，再点返回该结果与执行时间（之后状态已变的同时显示当前值），不重新执行；暂时失败不作废；到期后点击返回“已过期”并给出 Web 入口 |
| 执行前检查 | 依次：回调验签与去重（按渠道事件标识，无标识时用账号、卡片、按钮键与事件时间组合）；点击者渠道账号所属租户与令牌一致、绑定为“已绑定”且成员有效；点击者是令牌的收件人；令牌未过期；以点击者当前权限实时 `Check`。前四项在操作开始前拒绝：只对该次调用生效并写审计，不消耗令牌、不固定结果，收件人之后照常执行；返回已固定的结果前同样先做前四项检查，不向他人返回。之后由业务所有者检查版本与状态。未绑定或绑定失效时只回复绑定引导 |
| 组合动作 | 由多个部分组成的动作（例如“不相关 + 忽略”）按业务所有者返回的逐部分结果展示；“无变化”按实际值判断 |
| 结果回写 | 渠道支持时只更新点击者本人的卡片为结果状态，否则另发结果消息；其他收件人的卡片不更新 |
| 撤销 | 撤销是新的操作，有自己的令牌与操作标识，调用业务所有者的撤销命令（如 `detection.UndoSignalTriage`），按原操作实际改变的部分恢复操作前的值；一项内各部分的补偿一起提交，任一部分版本核对不通过则整项不撤销，各部分保持当前值 |
| 内容已变化 | 业务所有者返回内容版本不一致时不执行，回复变化摘要并附按新版本渲染、带新令牌的卡片 |

## 5. 拥有的数据（租户域）

| 实体 | 说明 |
|---|---|
| `ChannelInstallation` | 租户在某渠道的应用安装与配置：所属企业标识、状态（待验证 / 已启用 / 异常 / 已停用）、健康状态（凭证存放于密钥管理系统） |
| `BindingRequest` | 绑定请求：发起的渠道账号、固定的 Web 会话、过期时间、状态；一次性 |
| `ChannelBinding` | 渠道账号与平台成员的绑定：渠道账号、所属安装、成员、状态（已绑定 / 失效中 / 已失效）、版本、最后一张签发凭据的到期时间、失效原因；一个渠道账号只绑定一个成员，一个成员在同一渠道只绑定一个账号 |
| `Conversation` | 渠道会话：渠道、线程标识、参与者、关联的研究案例 |
| `NotificationRule` / `Preference` | 通知规则与个人偏好（简报发送时间、时区、发送日、安静时段、空简报是否推送、每区条数，带偏好版本）；规则决定通知受众，发送前逐人检查访问，受众不能超出访问权限（[customer-context 第 11 节](customer-context.md#11-分派配置)） |
| `Notification` / `Delivery` | 通知与投递记录（幂等键：`notification_id + channel + recipient`；合并通知另见 4.5）；投递记录分别保存平台提交、渠道接受与用户阅读（渠道支持时）的时间与结果，供送达指标分段统计（[质量与验收第 7 节](../../05-delivery/02-quality-and-acceptance.md#7-可靠性与安全)） |
| `ActionToken` | 卡片按钮的动作令牌（4.6）：租户、收件人、对象、动作、所见对象内容版本、到期时间、所属卡片与投递；标识即操作标识，记录各次尝试（幂等键、结果、是否已提交）与固定的最终结果；操作开始前的拒绝（验签、租户、绑定、收件人、过期）不记入令牌，只写审计。投递重试沿用同一批令牌，重新推送生成新令牌 |
| `Briefing` | 一期简报：租户、受众、简报来源（日常简报定义或持续型推进 ID）、周期起止时间（带时区偏移）、各区状态与覆盖窗口、各区条目引用（信号标识与内容版本）与排序列表版本（4.4 第9条）、数据截至时间、偏好版本、规则版本、组装时间、投递记录（含重新推送） |

## 6. 接口

| 接口 | 类型 | 说明 |
|---|---|---|
| `InstallChannel` / `VerifyInstallation` / `UninstallChannel` | 命令 | 租户管理员安装、以测试消息验证、停用渠道应用；停用使该安装下全部绑定失效 |
| `StartBinding` / `BindAccount` / `UnbindAccount` / `ListBindings` | 命令/查询 | 渠道账号绑定：`StartBinding` 为发起的渠道账号签发一次性绑定链接；`BindAccount` 只在渠道内置浏览器中、经渠道网页授权取得的当前渠道账号与链接一致且已登录妙洽时确认，请求与首次登录的会话固定。绑定的权威在本子系统；免登时核对绑定有效后签发 30 秒有效的签发凭据，交 identity 的 `IssueChannelSession` 使用，不在凭据有效期外重试；失效提交后绑定进入“失效中”、不再签发凭据，同步调用 `RevokeChannelSessions`（失败时经事件重试），撤销确认且最后一张凭据过期后才标为已完成。流程见 [PRD 第1段](../../01-product/prd/01-login-and-channel-binding.md) |
| `ReceiveInbound` | 命令 | 适配器回调入口（经 `gateway` 暴露，验签） |
| `ExecuteAction` | 命令 | 动作令牌触发，按 4.6 检查后以点击者身份转调对应子系统命令（`detection.TriageSignal` / `UndoSignalTriage`、`pursuit.CreatePursuit`）；令牌标识为操作标识，每次调用为一次尝试；任务提醒卡片的“完成”转调 `pursuit.CompleteTask`，撤销转调 `ReopenTask`，规则同 4.6；截止改变后重建的提醒是新卡片与新令牌（[PRD 第6段](../../01-product/prd/06-pursuit-follow-up.md) F9） |
| `GetPreferences` / `UpdatePreferences` | 查询/命令 | 偏好 |
| `ListNotifications` / `ListConversations` | 查询 | 工作台中的通知与会话列表 |
| `ListBriefings` / `GetBriefing` | 查询 | 本人的简报历史与详情：条目为组装时快照，另叠加当前处理状态并分别标注时间轴；只返回本人的简报 |
| `ResendBriefing` | 命令 | 本人对当日一期简报重新推送：新的投递与新的动作令牌，不重新组装；推送前按 4.4 第10条再检查 |
| `ComposeBriefing` | 命令 | 由定时任务或持续型推进的周期触发，按 4.4 组装并投递。幂等键：租户 + 受众 + 简报来源（日常简报定义 ID，或持续型推进 ID）+ 周期起止时间；同一期重试复用已生成的简报，不同推进或不同简报定义各自生成一期 |

**实现状况（PRD 第1段 F4–F7 服务端切片，[channels 契约](../../../contracts/apis/channels/openapi.yaml) 0.1.0）**：

- **渠道适配器**：首批只有企业微信（`wecom`）。适配器是端口 `api.Provider`（安装授权地址与授权码交换、回调验签、网页授权取当前渠道账号、发送消息），测试使用内存实现 `api/wecomtest`（HMAC 签名、一次性授权码，不做外部调用）。生产企业微信适配器未接入（C10 只批准了设计，未授权真实调用与凭证），`channels.Module()` 缺省为“未接入”：开始安装 503 `channel_unavailable` 且不写入任何状态，免登与绑定按渠道网页授权不可用处理；
- **安装**（`GET/POST /v1/channels/installations`、`…/{id}/verify`、`…/{id}/disable`）：开始安装、验证与停用经 `identity.Guard.RequireAdmin`（`can_manage_members` + 15 分钟内的强认证），幂等键与 `expected_version` 同其他命令；列表只对管理员开放。开始安装返回渠道官方授权地址（只在首次响应中，state 为一次性随机数，只保存摘要，10 分钟有效）；授权回调（本地绑定 `api.Callbacks.InstallCallback`）核对 state 与安装版本后登记“企业 → 租户”路由（同一企业只能属于一个租户，409 `corp_installed_elsewhere`），安装进入待验证。授权中或已停用的安装可重新开始授权（版本加一，此前的 state 失效）。验证以“管理员本人已在该企业完成绑定”代替“发送测试消息”（否则 409 `verification_binding_required`），之后为已启用。停用在一个事务中使全部有效绑定以 `channel_disabled` 进入失效中、作废未完成的绑定请求、删除企业路由，提交后逐个同步撤销；
- **绑定**（`POST /v1/channels/bindings:prepare`、`:confirm`，`GET /v1/channels/bindings`）：未绑定账号发消息时（`api.Callbacks.ReceiveInbound`）只收到绑定引导，内含一次性绑定链接（32 字节随机数，只保存摘要，10 分钟有效；同一账号尚未打开的旧链接作废）。打开与确认都须同时提供渠道网页授权码（缺失 400 `channel_browser_required`，即不在内置浏览器中）与交互登录的妙洽令牌（须带 Web 会话标识 `sid`，免登会话不能用来绑定，403 `interactive_login_required`）。依次核验：当前渠道账号所属企业是本租户已安装的企业（403 `channel_tenant_mismatch`）；链接未作废、未过期（410）；当前渠道账号与链接记录的一致（转发到他人企业微信，403 `channel_identity_mismatch`，链接作废）；首次打开时固定成员与 `sid`，之后成员或会话不同 409 `binding_session_mismatch`（链接作废）；确认须原样提交打开时返回的内容摘要（409 `binding_request_changed`）。首次打开时同时记下成员当前的成员版本（经 identity 会话核验取得），成为绑定的成员版本。一个渠道账号只绑定一个成员、一个成员在同一渠道只绑定一个账号（409 `channel_account_already_bound` / `member_already_bound`，唯一索引兜底并发）。已完成的链接以同一会话、同一渠道身份、同一摘要重试且绑定仍有效时返回首次结果（`already_bound`），不新建绑定、不再通知；其他情况 410 `binding_link_used`。成功后在渠道内通知“绑定成功”，发送失败不回滚；
- **解绑与失效**（`POST /v1/channels/bindings/{id}/unbind`）：本人解绑不要求强认证；他人绑定须管理员 + 强认证并填写原因（400 `reason_required`），非管理员与其他租户看到 404 `binding_not_found`。解绑、强制解绑、成员退出（消费 `identity.member.changed.v1`：只使绑定的成员版本低于事件版本、即这次退出之前建立的绑定失效，固定到该成员且早于这次退出的未完成请求一并作废；迟到的旧退出事件不影响重新加入后建立的绑定）与渠道停用都使绑定版本加一并进入失效中（`revoking`，记录原因与时间），发布 `channels.binding.changed.v1`，从此不再签发免登凭据、不再投递；提交后同步调用 identity `RevokeChannelSessions`。identity 确认撤销到当前版本、且最后一张签发凭据已过期（含 5 秒偏差）后才标为已失效（`invalidated`，再发布一次事件）；在此之前列表中显示为失效中。同步撤销失败时经本子系统消费自己的 `channels.binding.changed.v1`、identity 消费同一事件，以及 `api.Maintenance.RetryRevocations`（按上次尝试时间轮转，失败的不挡住其他绑定）送达；重新绑定是新的绑定标识；
- **退出前的旧绑定**：已绑定的账号发消息（`ReceiveInbound`）与选择投递目标（`DeliveryTarget`）时，都经 identity `MemberLifecycle` 核对绑定仍属于成员当前的成员资格（成员有效，且绑定的成员版本高于最近一次退出时的成员版本；不要求相等，普通的角色变化不使绑定过时）。来自退出之前的旧绑定（成员退出后又重新加入、退出事件尚未送达）立即以 `member_left` 失效：消息回调随后按未绑定发出绑定引导，投递改投站内（`not_bound`）。identity 不可用时失败关闭：消息回调返回 503 `identity_unavailable` 且不写入任何状态（含去重记录、不发引导），由渠道重发后再处理；投递目标返回 503 `identity_unavailable`（可重试），既不投到渠道，也不声称未绑定。核对之后、写入之前绑定发生变化时消息回调返回 503 `channel_state_changed`，同样不写入状态；
- **免登**（`api.Callbacks.ChannelSSO`）：经渠道网页授权取得当前渠道账号，在企业路由与已启用的安装下锁定该账号的有效绑定行，记录本次签发凭据的到期时间（30 秒，`last_credential_expires_at` 取较大值）后提交，再以凭据请 identity 签发 12 小时免登会话；调用在凭据“到期前 5 秒”截止，不在有效期外重试。签发请求带绑定的成员版本；identity 以 409 `member_rejoined` 拒绝（绑定建立后成员退出过，之后又重新加入、退出事件尚未送达）时，立即使这条旧绑定以 `member_left` 失效并转登录页，成员须重新绑定。签发返回后再核对一次绑定，其间已失效时不交出会话（该会话由撤销作废）。失效与签发锁同一绑定行，因此失效要么先于凭据（签发被拒），要么看得到这张凭据并等它过期才完成。任何不可用都转登录页并说明原因（`channel_oauth_unavailable`、`channel_oauth_invalid`、`channel_not_installed`、`not_bound`、`binding_inactive`、`member_inactive`、`identity_unavailable`、`session_rejected`），不会把“不可用”当作“未绑定”；
- **对账**：`api.Maintenance.ReconcileChannelSessions` 分页读取 identity 中仍有有效会话的全部绑定，对本子系统中不是有效绑定的（含不存在的）撤销到两边版本的较大者；本子系统查询失败或 identity 不可用时中止并返回错误，不把未知当作失效；
- **卡片跳转**（`POST /v1/channels/card-links:open`）：出站编排经本地绑定 `api.Messaging.CreateCardLink` 为收件人与对象生成链接（只保存令牌摘要）。打开时不切换账号：链接收件人不是当前成员时 409 `account_mismatch` 并带出收件人，由客户端提示切换；链接不存在、属于其他租户、对象不存在与无权查看（实时 `Check can_view`）都是同一个 404 `content_unavailable`，响应不含标题、归属或负责人；过期 410 `card_link_expired`。`api.Messaging.DeliveryTarget` 只在渠道已启用、绑定有效且经上述核对仍属于成员当前的成员资格时投到渠道，否则改投站内并给出原因；查询失败或 identity 不可用时返回错误，不当作“未绑定”；
- **回调**：先验签（失败 401 `callback_signature_invalid`），再核对时间戳在业务时钟前后 5 分钟内（否则 401 `callback_replayed`），再按（渠道、企业、渠道事件标识）去重，去重与业务写入在同一事务中；被拒绝与重复的回调不改变任何业务状态，计入该企业路由上的被拒回调数（安装列表的 `rejected_callbacks`）；
- 尚未实现：网关的未认证入口（安装回调、消息回调与免登目前只以本地绑定提供）、生产企业微信适配器、渠道凭证入库、“发送测试消息”与安装健康状态“异常”（只有被拒回调计数）、渠道内对话、飞书、钉钉与 Telegram。简报、偏好与卡片快捷动作见下文第5段实现状况，信号的即时通知见下文 C-08 实现状况。定时任务由 `scheduler` 角色逐个 active 租户以 `svc:channels` 调用（`services/subsystems/channels/jobs.go`，验收线 R-01）：撤销重试与简报投递每分钟、免登会话对账每 15 分钟、简报按到时组装（`ComposeDue`，More 为真时本轮继续）每分钟、卡片动作每 15 秒、渠道通知投递（`Notices.DeliverNotices`）每分钟。

本切片选定的缺省值（规格未定处）：

| 事项 | 取值 |
|---|---|
| 绑定链接 | 10 分钟有效；同一账号再发消息时尚未打开的旧链接作废，已固定会话的不作废 |
| 绑定所需的 Web 会话 | 令牌的 OIDC `sid`；没有 `sid` 的令牌不能绑定 |
| 签发凭据 | 30 秒有效，5 秒时钟偏差两侧从严（identity 只接受早于到期前 5 秒的，channels 到期后 5 秒才认为过期） |
| 免登会话 | 12 小时绝对时长，identity 业务时钟 |
| 卡片链接 | 72 小时有效 |
| 回调时间窗 | 业务时钟前后 5 分钟 |
| 安装验证 | 管理员本人在该企业已有有效绑定 |
| 安装列表 | 只对管理员开放 |
| 绑定列表 | 管理员看到本租户全部，其他成员只看到本人；按 `(created_at, id)` 倒序分页，每页至多 500，游标绑定租户、可见范围与 `state` 过滤 |
| 撤销重试 | 每次至多 `limit`（1–500）条失效中的绑定，按上次尝试时间轮转 |

**实现状况（PRD 第5段简报与卡片快捷动作的服务端切片，[channels 契约](../../../contracts/apis/channels/openapi.yaml) 0.2.0、[channels 事件](../../../contracts/events/channels/events.asyncapi.yaml) 0.2.0）**：

- **组装**（本地绑定 `api.Briefings.Compose`，带租户的服务主体调用；定时任务未接入）：按成员逐人组装。先经 identity `MemberLifecycle` 确认成员有效，按偏好版本序列求出 `at` 所在的周期（`internal/domain`：固定区间，起止以带偏移的时刻保存；新偏好版本从修改时所在周期的下一个边界生效，生效当天不额外产生一期）。幂等键为租户 + 成员 + 简报来源（应用领域包 `briefing.yaml` 的 `BRF-TENDER-DAILY`）+ 周期起止，表上唯一约束与按成员的咨询锁兜底并发；已存在时返回 `existing`。以 channels 服务主体经 customer-context `ActivePolicies` 枚举生效目标，再以**收件人本人**的用户主体逐目标判断 `can_view_signals` / `can_feedback`（不可看的目标不列出，判断不可用时该区不列出、简报标为部分），并经 mTLS 内部端口调用 detection `listBriefingSignals` 取数（覆盖窗口从该区上一次成功取数并随简报保存的终点起，至多 62 天）。取数暂时失败为 `unavailable`、窗口不前移；覆盖端口报告来源中断时为 `source_interrupted`（覆盖不可得时各领域为 `unknown`，不当作正常或中断）。“待我处理”区经 identity 内部端口以本人身份列出待其批准的审批，排在首位；任务随第6段；
- **条目**：每区按配额（成员偏好或包中缺省）列出，更正类不受配额且排在区首（detection 已排序），超出的与 detection 单次上限之外的待处理条目计入 `overflow_count`；同一采购项目在后面的区为引用行（`references`），前面的区标 `also_matched`。来源字段按展示级别逐字段白名单写入（`full_text` 原值、`excerpt` 截断、`citation_only` 只有 http(s) 原文链接、`none` 都没有），多个来源取最严格，未知级别、缺少权利与权利端口失败都按 `none`；不复制来源记录。保存的快照只固定内容、不固定展示权限（[数据架构第 12 节](../02-data-architecture.md#12-来源权利与数据权益)）：读取完整简报、本人重新推送与每次发送尚未送达的消息（含排队与重试）前，按当前来源权利重新裁剪，每个字段取快照级别与当前级别中更严格者，`citation_only` 与 `none` 不保留原值，权利不可得时按 `none`；投递保存每张信号卡片的渲染来源（区标题与条目快照），发送前重新渲染正文与总览卡，按钮、令牌与链接不变；
- **推送**（`api.Briefings.DeliverDue`）：空简报默认不推送（`not_pushed`/`empty`）；没有有效绑定只在 Web 与站内（`not_bound`）；当地 12:00 及之后才组装的不推送、窗口不前移（`late`）；晚于计划 10 分钟以上标 `delayed`。投递保存已渲染的消息（总览卡 + 每区前 3 条、合计至多 6 张信号卡片），重试发送同一批消息，卡片、链接与令牌不变；每次发送前以租约认领，再检查成员有效、绑定仍是投递时的绑定、各目标仍可看（失去访问的区删除并重新渲染尚未送达的总览卡），依赖不可用时延后重试、不当作退出或失效；安静时段内延后到结束；失败按退避重试到当日 18:00，之后改投站内（`in_app`/`cutoff`）。发布 `channels.delivery.sent.v1` 与 `channels.delivery.failed.v1`（`final` 区分重试与结束）；
- **动作令牌**：每个按钮一个 `atk_` 令牌，卡片上携带的是 channels 签发的整个载荷（令牌标识、租户、收件人、信号、动作、原因、所见内容版本、卡片、到期时间）连同 HMAC-SHA256 签名（`MIAOQIA_CHANNELS_SIGNING_KEY`，base64、至少 32 字节，同一部署相同；只有 development 可缺省为进程内随机密钥），用途分隔，签名不符在任何读取之前拒绝。按钮 24 小时有效，“撤销” 10 分钟；
- **点击**（`api.Callbacks.ReceiveInbound` 的卡片回调 + `api.Briefings.ProcessActions`）：回调验签与时间窗之后先校验令牌签名、令牌租户等于回调企业所属租户、卡片一致（否则 `rejected` 并写审计，令牌不受影响），按渠道事件标识去重后排队（`queued`）。执行时依次检查：令牌存在且卡片一致；点击者账号有有效绑定且仍属于成员当前的成员资格（否则只发绑定引导）；点击者是收件人（否则回复“此卡片发给其他成员”，不返回结果）；已有固定结果时返回该结果与执行时间及当前状态；未过期。之后以点击者的用户主体经内部端口调用 detection：首次执行时按信号当前的状态与反馈版本构造请求并固定在令牌上，至多 3 次尝试，每次幂等键为“令牌标识-序号”、操作标识为令牌标识；成功、无变化与确定失败固定在令牌上并发布 `channels.action.executed.v1`，暂时失败不作废。结果优先更新点击的卡片（`UpdateCard`），不支持或失败时另发结果消息；成功的反馈带“撤销”；`signal_updated` 另发按新版本渲染、带新令牌的卡片；
- **简报历史与完整简报**（`GET /v1/channels/briefings`、`/{id}`）：只返回本人的简报（他人、其他租户与不存在同为 404 `briefing_not_found`）；历史按 `(period_end, id)` 倒序、每页至多 30 条，游标签名并绑定租户与成员（否则 400 `invalid_cursor`）。完整简报的条目为快照，按调用者当前权限再检查（失去访问的区只剩种类、顺序与 `no_permission`），处理状态经 detection `listSignalCards` 按“当前”叠加并标注处理人与内容是否已更新；
- **重新推送**（`POST /v1/channels/briefings/{id}/resend`）：只限本人当日一期（409 `resend_not_allowed`）与有效绑定（409 `not_bound`），不重新组装，新的投递与新的令牌；同一幂等键重放返回首次的投递。重新推送的重试时限为发起后 2 小时，不受 18:00 截止与安静时段限制；
- **简报链接**（`POST /v1/channels/briefing-links:open`）：链接只指向对象（信号、简报、审批），7 天有效；他人打开 409 `account_mismatch`，不存在、其他租户与无权限同为 404 `content_unavailable`，过期 410；
- **偏好**（`GET/PUT /v1/channels/briefing-preferences`）：`expected_version` 乐观并发（409 `version_conflict`），非法值 422 `invalid_preferences`，生效时刻为下一个周期边界；写审计；
- **投递统计**（`GET /v1/channels/delivery-stats`）：需要 `can_manage_members`，只有状态、原因与计数；C-11 追加渠道通知的计数 `notices`（见第6段 C-11 实现状况）；
- 尚未实现：企业微信生产适配器、覆盖与来源权利的生产适配器（`api.Coverage` / `api.Rights` 为 NotWired）、AI 摘要（M2 不开启，C-18）、“关注变化”区（关注开放前不显示，C-19）。定时任务已由 `scheduler` 角色调用（R-01），即时通知（F7）见下文 C-08 实现状况。

本切片选定的缺省值（规格未定处）：

| 事项 | 取值 |
|---|---|
| 缺省偏好 | 工作日 07:30、Asia/Shanghai、安静时段 21:00–07:00、不推送空简报、各区条数取包中缺省值 |
| 租户时区 | 固定为 Asia/Shanghai（租户时区设置未建模） |
| 延迟发送 | 组装晚于计划发送时间 10 分钟以上 |
| 当日不再推送 | 当地 12:00 及之后组装；投递重试截止当地 18:00 |
| 令牌与链接 | 按钮 24 小时、撤销 10 分钟、简报链接 7 天 |
| 一次点击的尝试 | 至多 3 次，暂时失败不作废令牌 |
| 卡片数 | 每区前 3 条、每期至多 6 张信号卡片 |
| 签名密钥 | `MIAOQIA_CHANNELS_SIGNING_KEY`，至少 32 字节；动作令牌与历史游标共用、按用途分隔 |

**实现状况（PRD 第6段卡片“跟进”，[channels 事件](../../../contracts/events/channels/events.asyncapi.yaml) 0.3.0）**：信号卡片在租户已开启跟进入口（发布开关 `MIAOQIA_CHANNELS_PURSUE_TENANTS`，逗号分隔的租户，缺省为空即不显示）、pursuit 的 `Actions` 绑定可用、且收件人本人在目标上有 `can_assign`（经 customer-context `ObjectiveAccess` 以收件人本人判断，判断失败不显示）时带“跟进”按钮；排除信号不显示。点击沿用第5段的令牌、回调与执行检查（签名、租户、卡片、绑定、收件人、已固定结果、到期），之后以点击者的用户主体调用 pursuit `CreatePursuit`（`channel` 为 `wecom`，负责人为点击者本人、没有任务；操作标识为令牌标识，每次尝试的幂等键为“令牌标识-序号”）；已创建与“已存在 · 已关联”为成功、“已存在”为无变化，pursuit 的确定失败（4xx）固定在令牌上，暂时失败不作废令牌；`channels.action.executed.v1` 的 `action` 为 `pursue` 并带 `pursuit_id`。卡片“跟进”不提供撤销（撤销创建在 Web 中）。

**实现状况（PRD 第6段渠道侧 C-09，[channels 契约](../../../contracts/apis/channels/openapi.yaml) 0.3.0、[channels 事件](../../../contracts/events/channels/events.asyncapi.yaml) 0.4.0）**：

- **通知记录**：消费者 `channels.pursuit-notices` 订阅 pursuit 主题：`pursuit.task.due.v1` 为每个（提醒、收件人）记一行 `chn_notices`（转给决策者的提醒没有收件人，记为 `*`）；乱序按（截止版本、提醒时刻）比较，同一任务已有更新的通知时新到的旧事件记为取消（`superseded`），否则取消同一任务更早的未发通知。`pursuit.pursuit.authorization_rejected.v1` 带操作者（成员，非服务主体）时给操作者记一行，每个跟进的每个参与版本一条；
- **投递**（本地绑定 `channels.Notices.DeliverNotices`，调度器周期调用）：以租约认领；渠道重试时限为事件时刻起 24 小时（安静时段延后时顺延），之后留在 Web 与站内（`in_app`）。转给决策者的一行先展开：以每个已绑定成员本人经 pursuit `getTask` 判断 `viewer_escalated`（C-11 起分页展开、不设总上限，见下文），满足的各记一行（未绑定的决策者在简报“待我处理”与 Web 中看到）。每次发送（含重试）前按收件人当前状态再检查，顺序为：安静时段（收件人本人当前偏好；延后到结束、不丢弃，在任何渲染与令牌签发之前）→ 成员有效 → 以收件人本人经 pursuit 读取（任务：不能查看取消、跟进关闭或任务非待办取消、截止版本已变取消 `superseded`、已不是负责人也不是转交给他的决策者取消 `reassigned`、写入准入不满足（授权同步中）稍后重试；同一任务已有更新的通知取消；授权被拒通知：操作者已不能查看该跟进取消 `pursuit_not_visible`）→ 投递目标（未绑定为 `in_app`）。卡片在第一次发送前渲染并固定（重试发送同一张卡片、令牌不变；解绑后重新绑定的按新绑定重新渲染），正文每次按当前值重新渲染；结果写审计（`channels.notice.*`），C-11 起另发布通知投递事件 `channels.notice.*.v1`（`channels.delivery.*` 仍只用于简报）；
- **任务提醒卡片**：标题为任务名（转给决策者的为“待你决定的任务”），正文为截止（所填值与时区）、逾期、转交说明与备注；按钮“完成”（动作令牌，24 小时，令牌带任务与提醒所在的截止版本）与“查看”（Web 任务地址 `/pursuits/tasks/{id}`，只指向对象、不带令牌，打开时由 pursuit 以当前登录成员授权）。授权被拒通知：被移出的人数（负责人、协作者）与原因，负责人失效时说明任务转给决策者，“查看”为 `/pursuits/{id}`；
- **“完成”与“撤销”**：点击沿用第5段的回调、令牌与执行检查（签名、租户、卡片、点击者的有效绑定与成员资格、点击者是收件人、已固定的结果、到期；任一不满足不消耗令牌），由 `Briefings.ProcessActions` 执行：以点击者本人经 `getTask` 再判断——不能查看为确定失败 `forbidden`；令牌的截止版本不是当前版本为确定失败 `deadline_changed`（截止改变后的新提醒是新卡片与新令牌）；已不是负责人也不是转交给他的决策者为 `forbidden`——之后以点击者调用 `completeTask`（所见任务版本在首次执行时固定在令牌上）或 `reopenTask`（`channel` 为 `wecom`；操作标识为令牌标识，每次尝试的幂等键为“令牌标识-序号”）。成功、无变化与确定失败固定在令牌上并发布 `channels.task_action.executed.v1`；暂时失败与 `authorization_syncing` 不作废令牌（同步完成后同一令牌可再执行）。成功的“完成”带“撤销”（10 分钟，pursuit 的渠道撤销时限），回写卡片显示点击者此刻可见的当前状态；
- **简报“待我处理”中的任务**：组装时以成员本人经 pursuit `listMyTasks`（截止早于下一期计划发送，含已逾期；含转给本人决定的 escalated 任务）读取，排在审批之后；pursuit 不可用时本区暂不可用。读取完整简报、重新推送与每次投递发送前以本人经 `getTask` 再检查：已不能查看的整条移除，其余叠加 `task_current`（todo / done / cancelled / reassigned / pursuit_closed），总览卡只计仍待处理的；
- **调度器入口**：`Notices.DeliverNotices`（通知投递与安静时段结束后的延后通知）与 `Briefings.ComposeDue`（为本租户已绑定成员中、按本人偏好与时区在 `at` 已到计划发送时间且本期尚未组装的成员组装简报；同一成员同一期只组装一次，未生成简报的记在 `chn_compose_marks`；已错过的上一期不补，由下一期覆盖；每次至多 `limit` 名，`More` 表示还有）。两者都只有本地绑定（与 `Briefings` 其他方法相同），调度器接入见 [A02 验收缺口 R-01](../../05-delivery/09-a02-end-to-end-acceptance.md#43-运行装配r)；
- **证据**：`services/kernel/integration/pursuit_test.go`（`TestPursuitFollowUp` 的 C-09 用例：安静时段延后与重试同一张卡片、篡改 / 伪造签名 / 他租户 / 换卡片的令牌、非收件人点击、完成与重放与撤销、截止改变后的旧卡片与未发旧提醒、授权同步中不作废令牌、改派与成员退出后的未发提醒取消、授权被拒通知与已不能查看的操作者、转给决策者的展开与简报）与 `services/kernel/integration/briefing_test.go`（`ComposeDue`：限额与 More、重复调用、已错过的上一期、未绑定与已退出成员、他租户、时区边界）；
- **未实现**：C-09 登记的三项（通知的投递事件与统计、负责人仍在团队但写入准入不满足时转交提醒没有接收者、展开上限）已由 C-11 实现，见下文。

**实现状况（C-11：渠道通知的投递事件与统计、写入准入不满足时的转交、分页展开；C-12 中持续跟踪的授权被拒通知，[channels 契约](../../../contracts/apis/channels/openapi.yaml) 0.4.0、[channels 事件](../../../contracts/events/channels/events.asyncapi.yaml) 0.5.0、[pursuit 契约](../../../contracts/apis/pursuit/openapi.yaml) 0.4.0）**：

- **投递事件**：每条收件人确定的通知（任务提醒卡片，含转交展开后的各条；授权被拒通知）在状态确实改变时、与状态变化在同一事务中发布：送达一次 `channels.notice.sent.v1`（`attempt` 从 1 起）；每次渠道失败 `channels.notice.failed.v1`（`state` 为 `retrying`、`reason` 为 `channel_error`、`final` 为假）；结束一次 `channels.notice.failed.v1`（`final` 为真：`in_app` 的 `not_bound` / `cutoff`，`cancelled` 的 `member_inactive`、`task_not_visible`、`task_closed`、`superseded`、`reassigned`、`pursuit_not_visible`）。消费提醒事件时即被取代的旧提醒（乱序到达）与被新提醒取消的未发通知也各发布一次结束事件（`attempt` 为记下的尝试数）；重放同一提醒事件不新增通知、不再发布。转交尚未展开的行（收件人 `*`）不是一次投递，不发布；
- **字段白名单**：按通知种类逐项列出（`noticeEventData`）：共有 `notice_id`、`kind`、`recipient`、`channel`、`pursuit_id`、`attempt`；`task_due` 另有 `task_id`、`deadline_version`、`escalated`；`authorization_rejected` 只有跟进标识（交易型或持续跟踪）。个人数据只有收件人（与简报投递事件相同），不含标题、正文、备注、截止值、被拒成员与原因；原因只取状态白名单（`noticeReasons`），白名单外的不发布；
- **投递统计**：`getDeliveryStats` 追加 `notices`（按种类、状态与原因计数，每个收件人一条；转交未展开的行与 `expanded` 不计）。仍只对本租户管理员（`can_manage_members`）开放，查询带显式租户条件并受行级安全约束；原因按状态白名单投影，其余为 `other`，`pending` 与 `delivered` 不带原因；不含收件人、跟进、任务或标题；
- **转交的口径**（pursuit）：pursuit 发出提醒时以任务负责人本人判断写入准入（与写命令同一 `admitWrite`：能否更新、授权同步与本人当前角色的授予版本），不满足（含已不在团队中）即转给决策者（`escalated`）。`getTask` 的 `viewer_escalated` 与 `listMyTasks` 的 `escalated` 同一口径：调用者在单元上有 `can_edit_unit_pursuits`，且任务负责人已不在团队中，或仍在团队中、此刻以本人判断不满足写入准入、且当前截止版本已有一条转交提醒发出；负责人本人的判断不可用时 503（失败关闭）。负责人恢复写入准入后转交即结束：决策者尚未送达的转交通知在发送前再检查时取消（`reassigned`）；
- **分页展开**：转给决策者的一行每次认领至多处理一页已绑定成员（缺省 200，按 `subject` 升序的键集位置保存在 `chn_notices.expand_after`，不交给客户端），每人以本人经 `getTask` 判断；一页处理完立即重排到下一轮，最后一页处理完标为 `expanded`（`reason` 为累计条数）并写审计。任一判断暂时失败时整页重排、位置不前移（已记下的行唯一约束去重）。展开出的每一条在发送前仍以收件人本人再检查（撤销决策者角色、不再可见、负责人恢复准入都取消）；
- **持续跟踪的授权被拒通知**（C-12 的一部分）：授权被拒通知先以操作者本人经 `getPursuit`（交易型），确定失败时再经 `getTracking`（持续跟踪；pursuit 内部端口新增 `GET /internal/v1/pursuit/tracking/{id}` 与绑定 `pursuit.Trackings`，本地与远程两种装配）；两者都确定失败才取消（`pursuit_not_visible`），任一暂时失败重试。“查看”为 `/pursuits/tracking/{id}`；
- **证据**：`services/kernel/integration/pursuit_test.go`（`TestPursuitFollowUp` 的 “C-11 …” 子测试）与 `services/subsystems/channels/internal/app/notices_internal_test.go`。

本切片选定的缺省值（规格未定处）：

| 事项 | 取值 |
|---|---|
| 通知投递事件 | 新事件类型 `channels.notice.sent.v1` / `channels.notice.failed.v1`，不复用以简报为单位的 `channels.delivery.*` |
| 转交展开 | 分页，每页 200 名已绑定成员，不设总上限 |
| 转交的锚点 | 负责人仍在团队中时，以当前截止版本已发出的转交提醒为锚点，再以负责人本人实时判断 |
| 统计 | 只对管理员开放；白名单外的原因记为 `other`；转交未展开的行不计 |
| 持续跟踪的“查看” | `/pursuits/tracking/{id}` |

**实现状况（C-08：信号的即时通知，PRD 第5段 F7、用例5与41；[channels 契约](../../../contracts/apis/channels/openapi.yaml) 0.5.0、[channels 事件](../../../contracts/events/channels/events.asyncapi.yaml) 0.6.0、[customer-context 契约](../../../contracts/apis/customer-context/openapi.yaml) 0.6.0）**：

- **记录**：消费者 `channels.signal-notices` 订阅 detection 主题的 `detection.signal.detected.v1`，每条信号记一行待展开的通知（种类 `signal_immediate`、收件人 `*`，`chn_notices` 追加 `signal_id`、`objective_id` 与渲染来源 `item`，迁移 `00006`）；排除与回填的信号、标识不合法的事件不记。事件只是触发：信号、目标、处置与可见性在发送前都以收件人本人重新读取，伪造或改写的事件不能让任何人看到其无权查看的内容；重放同一事件不新增通知（唯一键：租户 + 种类 + 信号 + 收件人）；
- **展开**：由 `Notices.DeliverNotices` 认领后以 channels 服务主体经 customer-context `getActivePolicies` 读取目标当前的生效策略（0.6.0 起带 `owner` 与 `participants`），按 `route` 中该处置的 `notify`：`mode` 为 `immediate` 时把受众 `objective_owner` / `objective_members` 展开为每名候选收件人一行（`role:<角色>` 尚不支持，C-20）；`digest`、目标未激活或不存在都展开为零行。展开写审计 `channels.notice.expanded`（`route=immediate|digest|inactive`）；
- **发送前再检查**（每次发送与重试前，任何一步不满足之前不渲染、不签发令牌）：即时通知开关（成员最新偏好 `instant_notify`，`preference_off`）→ 安静时段（延后到结束、写审计 `channels.notice.deferred`）→ 成员有效（`member_inactive`）→ 以收件人本人判断目标的 `can_view_signals`（`objective_not_visible`）→ 以收件人本人经 detection `listSignalCards` 读取信号（不可见或不属于该目标为 `signal_not_visible`，已不待处理为 `signal_handled`，回填为 `not_immediate`）→ 目标当前生效策略对信号当前的系统处置仍为 `immediate`（`not_immediate`）且收件人仍在受众中（`not_in_audience`）→ 只在第一次尝试前：节流（本人最近一小时已送达 6 张，`throttled`）与并入简报（安静时段内到达、结束时当期简报在 60 分钟内发送，`merged_into_briefing`）→ 投递目标（未绑定为 `in_app`/`not_bound`）。依赖暂时不可用时按退避重排，不当作失去访问；渠道重试时限沿用 24 小时（`cutoff`）；
- **卡片**：与简报信号卡片同一模板、按钮与动作（F3–F5，含“跟进”的显示条件），文字前缀“即时通知：”；第一次发送前渲染并固定（重试发送同一张卡片、令牌不变；重新绑定后按新绑定重新渲染），每次发送前按当前来源权利收紧后重新渲染正文；点击沿用 `Briefings.ProcessActions` 的令牌与执行检查；
- **与简报的关系**：组装时，本人已送达或已过发送闸门（发送中或渠道失败待重试）的即时通知、仍待处理的信号标 `notified`（总览行后加“（已通知）”），不再推送信号卡片；尚未发送过的即时通知随本期简报结束（`merged_into_briefing`，与简报保存同一事务），安静时段内到达的条目标 `arrived_overnight`、卡片标“夜间到达”；
- **发送闸门（与组装的先后与并发）**：每次发送前在一个事务中取收件人的简报咨询锁（与组装保存简报同一把）并锁定通知行：通知已不是待发送时不发送；第一次发送前收件人已有含该信号的简报（推送中、已送达或已在站内）时并入（`merged_into_briefing`，覆盖组装时收件人 `*` 的行尚未展开的顺序；渲染与签发令牌之前也先判断一次）；否则把 `attempts` 加一后再发送。组装保存时在同一把锁下复核：本期列出的信号中有已送达或已过闸门、而组装时未标 `notified` 的，组装按新状态重做（至多 3 次）。因此无论先后与并发，同一信号对同一收件人只以即时卡片或简报卡片之一送达；
- **偏好**：`BriefingPreferences.instant_notify`（缺省开启）。与其他偏好不同，开关立即生效（发送前读取最新版本）；更新请求不提供时保持当前值；
- **投递事件与统计**：`channels.signal_notice.sent.v1` / `channels.signal_notice.failed.v1`，与 `channels.notice.*.v1` 同一语义（送达一次，每次渠道失败与结束各一次，与状态变化同一事务）；字段白名单为通知、种类、收件人、渠道、信号、目标与尝试数（`signalNoticeEventData`），不含项目、中标方、金额、理由或卡片正文；原因按状态白名单（`signalNoticeReasons`）。`getDeliveryStats` 的 `notices` 计入种类 `signal_immediate`，未展开的行不计；
- **与 4.5 的差异**：当前不生成合并卡片——超出节流的不单独推送、留给下一期简报（C-20）；`detection.signal.updated.v1` 的实质更新与紧急更正不即时通知（C-21）；
- **证据**：`services/kernel/integration/notify_test.go`（`TestInstantNotifications`：channels 经 mTLS 远程绑定调用 detection 与 customer-context，由 `scheduler` 角色的 `channels.deliver-notices` 任务驱动；“C-08 用例41 …”与“C-08 越权与反例测试 …”子测试覆盖另一租户、非成员、失去目标可见性、来源权利撤销、伪造与重放的事件、安静时段、节流、偏好关闭，以及简报先于展开、先展开后组装、组装前已过发送闸门与组装和发送并发），`services/subsystems/channels/internal/app/signalnotices_internal_test.go`、`services/subsystems/channels/internal/domain/briefing_test.go`（TestMergeIntoBriefing）。

本切片选定的缺省值（规格未定处）：

| 事项 | 取值 |
|---|---|
| 节流 | 每名收件人每小时至多 6 张即时通知卡片；超出的不单独推送（`throttled`），信号照常进入下一期简报；不生成合并卡片 |
| “简报即将发送” | 安静时段结束后 60 分钟内有本人的计划发送 |
| 即时通知开关 | `instant_notify` 缺省开启、立即生效，更新时不提供则保持当前值 |
| 不即时通知 | 排除、回填与人工处置；以发送时信号当前的系统处置重新判断 `route` |
| 节流与并入的判断时刻 | 只在第一次尝试前；之后的重试不再因节流或并入取消 |
| 投递事件 | 新事件类型 `channels.signal_notice.*.v1`，不扩展 `channels.notice.*.v1` 的必填字段 |
| 卡片文字 | 前缀“即时通知：” |

## 7. 事件

**消费**：`identity.member.changed.v1`（成员停用或退出时使其绑定失效）、`detection.signal.detected.v1`、`detection.signal.updated.v1`、`detection.signal.triaged.v1`（简报中的当前状态叠加）、`assessment.assessment.created.v1`、`assessment.decision.drafted.v1`、`identity.approval.requested.v1`、`pursuit.task.assigned.v1`、`pursuit.task.due.v1`、`detection.pursuit_related_event.v1`、`research.case.completed.v1`、`research.case.awaiting_input.v1`。

**发布**：`channels.binding.changed.v1`、`channels.delivery.sent.v1`、`channels.delivery.failed.v1`、`channels.notice.sent.v1`、`channels.notice.failed.v1`、`channels.signal_notice.sent.v1`、`channels.signal_notice.failed.v1`、`channels.action.executed.v1`、`channels.conversation.message_received.v1`。

## 8. 扩展点

- **`ChannelAdapter` 接口**（平台内部适配器，由平台团队实现）：`capabilities()`、`send(rendered_message)`、`receive(raw_callback) → inbound_message | action`、`verify(callback)`、`resolve_identity(raw_user)`；
- **渲染模板**：应用领域包提供领域卡片模板（渠道无关表示）；
- 新渠道接入清单：适配器、能力声明、身份绑定方式、回调验签、速率限制、数据分级允许范围、测试夹具。

## 9. 伸缩与可靠性

- 入站回调快速确认、异步处理，避免渠道超时重发；以渠道消息标识去重；
- 出站按渠道与租户限流，遵守各平台频率限制；失败重试、退避与死信；
- 单个渠道故障不影响其他渠道；渠道健康纳入运营监控。

## 10. 安全要点

- 每个渠道回调都验证签名与来源；渠道凭证存放于密钥管理系统；
- 身份只来自已绑定的渠道账号，未绑定账号只能收到绑定引导；
- 动作令牌短时效、绑定收件人与对象，令牌标识即操作标识，得到确定结果后作废（再点返回已固定的结果），执行时以点击者身份再次授权（4.6）；
- 渠道内只展示与该用户相关、且其有权查看的内容；群聊只发摘要；
- 境外渠道（如 Telegram、Discord）启用前，由 `governance` 按数据分级限定可发送的内容（例如只允许通知标题与登录链接）；
- 只使用各平台官方开放能力，不使用个人号自动化。


## 11. 分阶段启用与外部接入准备

M2交付按4.4组装的简报及历史、结构化查询和快捷反馈/分派、Web强认证跳转；研究提问和追问随M3研究入口启用，不能用渠道已接入代替研究能力就绪。对未启用能力明确显示不可用和可用入口。每个首批渠道在使用前完成官方应用账号/租户安装授权、域名与回调条件、凭证入库、验签、重放保护、限流、消息与动作负向测试、真实客户测试安装；四个适配器分别验收，单个失败不阻断站内兜底。
