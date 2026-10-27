# 推进与结果子系统（pursuit）

> 状态：评审中 · 版本：1.11 · 更新：2026-10-27 · 平面：客户智能

## 1. 使命

把批准的决策变成有负责人、有节奏、有记录的推进工作，并收集结果与原因，形成复盘与学习的闭环。

## 2. 职责与非职责

**职责**

- 推进实例：按机会路径定义创建推进，管理阶段与状态；推进分交易型与持续型两种形态（见 [业务架构 4.2](../../02-business/01-business-architecture.md#42-客户生命周期租户域)）；
- 持续跟踪的节奏：持续型推进的周期、负责人与暂停/恢复/停止；按周期触发变化简报或研究，不自行监测对象变化（属于 `detection` 的关注），也不执行研究（属于 `research`）；
- 阶段编排：阶段进入时触发默认研究、生成任务模板、设置决策门；
- 任务与分派：任务、负责人、截止时间、协作者、提醒；
- 领域事件响应：与推进相关的领域变化（更正、延期、结果发布）引起提醒或阶段变化；
- 路径衔接：按应用领域包定义的衔接规则建议新推进（如投标落败 → 进入供货）；
- 结果与复盘：结果、原因、对手信息、复盘记录；
- 外部系统同步：CRM、项目管理工具中的记录与推进的映射、字段归属与回写内容；连接、凭证与增量同步由 `acquisition` 的租户集成实例执行（[acquisition 第 10 节](acquisition.md#10-租户集成)）。

**非职责**

- 不替代 CRM 或项目管理系统的完整功能；
- 不执行判断与审批。

## 3. 内部组件

| 组件 | 说明 |
|---|---|
| `playbook_registry` | 机会路径定义（应用领域包提供，版本化） |
| `pursuit_engine` | 推进状态机与阶段编排；长周期提醒与时限由 Temporal 工作流驱动 |
| `tasks` | 任务、分派、提醒 |
| `outcomes` | 结果与复盘 |
| `sync_adapters` | CRM、项目管理工具记录与推进的映射和回写（连接与同步由租户集成实例执行） |

## 4. 拥有的数据（租户域）

| 实体 | 说明 |
|---|---|
| `Pursuit` | `pur_`：形态（`deal` 交易型 / `continuous` 持续型）、目标对象（持续型可为一组对象，以 `subject_set` 定义；每期触发简报或研究时解析出本期成员快照并随请求传递，产物固定该快照）、目标、机会路径版本、阶段、状态、负责人、关联决策与信号；持续型另含周期与下次执行时间；交易型另含目标对象键（A02 为采购项目 + 标包 + 中标方）、来源信号与创建时所见的信号内容版本、版本（改派、任务、关闭、新增信号关联任一改变时递增）、期望参与关系与参与版本（创建、改派、协作者变化、撤销创建时递增）及 identity 已应用的参与版本，状态含进行中 / 已关闭 / 已撤销（撤销创建，保留记录、不计入统计）。同一租户、同一目标、同一目标对象至多一个进行中的交易型推进，以唯一约束保证 |
| `PursuitSignalLink` | 信号与推进的关联记录：关联标识、推进、信号、建立时所见的信号内容版本、建立方式（创建 / 关联到已有推进）与操作标识、状态（有效 / 已解除）；同一推进与同一信号至多一条有效关联（唯一约束）；M2 只随撤销创建解除（[PRD 第6段](../../01-product/prd/06-pursuit-follow-up.md) F2 第5条） |
| `StageTransition` | 阶段变化与原因 |
| `Task` | 任务、负责人、状态（待办 / 已完成 / 已取消）、版本；截止时间保存值、精度（日或分钟）与时区，只填日期的不补成时刻；可绑定目标对象的公开截止时间（`deadline` 标识与类型），绑定的随公开截止变化改时，手工修改即解除绑定；提醒按任务 + 截止版本 + 提醒种类幂等，规则见 [PRD 第6段](../../01-product/prd/06-pursuit-follow-up.md) F5 |
| `Outcome` | 追加式结果记录：结果类型、原因代码、说明、证据、金额条目、事件锚点与获知信息（第 4.1 节）；修订时追加新记录并引用被取代的记录 |
| `Retrospective` | 复盘记录 |
| `ExternalLink` | 与外部系统记录的映射 |

### 4.1 结果与客户价值

客户价值（[商业模式第 2 节](../../01-product/02-business-model.md#2-客户价值的计量)）从 `Outcome` 计算，不另设“结果价值”对象。每条 `Outcome` 记录：

| 内容 | 规则 |
|---|---|
| 金额条目 | 零到多条，每条有金额类型（应用领域包定义，例如中标金额、合同金额、本方供货金额、预计年采购额、毛利）、币种（ISO 4217）、以十进制文本保存的数值、取值性质（`actual` 已发生 / `estimate` 估计 / `customer_stated` 客户自报未核实）及来源（公共事实版本、客户文件或口述）；不同性质不合并，不换算成单一价值分 |
| 事件锚点 | 结果对应的事件及其发生时间：公共事件引用领域事实版本（例如中标结果公告），客户私有事件（例如合同签订、首批供货）记录客户给出的时间与依据 |
| 获知信息 | 客户何时知道结果（`known_at`）与获知渠道（公共公告、妙洽信号、采购人或总包通知、客户内部系统、其他）；`recorded_at` 是写入平台的时间，三者可以不同 |
| 修订 | 结果、金额或性质变化（估计转为实际、金额更正、结果撤销）都追加新记录，引用被取代的记录并写明原因与操作者；旧记录保留，审计可查 |

客户填写的金额只属于本租户，不回写公共事实；与公共事实不一致时两者并存，展示时标明来源。

### 4.2 依据链

推进与结果显式记录前序，不在事后按时间或对象推测：

| 产物 | 记录的前序 |
|---|---|
| `Pursuit` | 来源：信号、决策、另一推进的衔接或手动创建（手动创建标明，没有前序）；引用其固定版本 |
| `StageTransition` | 触发它的决策、判断或任务 |
| `Outcome` | 所属推进；可另引用对结果有作用的信号、研究报告或判断版本 |

`Outcome` 可以带贡献关系，取值为有限枚举：`first_alert`（妙洽信号早于客户获知，以信号时间与 `known_at` 比较）、`decision_basis`（研究或判断被决策引用）、`self_found`（客户自行发现）、`external_notice`（采购人、总包等通知）。贡献关系由系统按记录的引用与时间判定，用户可以更正，更正追加记录。它们说明链上发生过什么，不是因果贡献的计算结果：价值指标按贡献关系分别统计（[质量与验收](../../05-delivery/02-quality-and-acceptance.md)），不合成“妙洽带来的金额”。

## 5. 接口

| 接口 | 类型 | 说明 |
|---|---|---|
| `CreatePursuit` | 命令 | 从决策、信号或手动创建；对没有信号的公共对象手动创建时，界面提示可向 learning 报告漏报（[learning 第 11 节](learning.md#11-漏报反馈与诊断)），不自动提交。带操作标识（Web 为表单意图，渠道为动作令牌标识）与每次尝试的幂等键；推进、负责人、协作者、第一项任务、与来源信号的关联、期望参与关系与授权同步请求在 pursuit 的一个本地事务中写入；identity 参与关系不在该事务内，由授权同步写入（第 5.1 节）。从信号创建时实时检查 `can_assign`，并经 `detection.GetSignal` 核对信号属于本租户与目标、内容版本一致（否则 `SIGNAL_UPDATED`）、不是排除信号（`NOT_APPLICABLE`）与未关闭（`SIGNAL_CLOSED`）。同一目标与目标对象已有进行中的推进时：请求带信号且该信号与该推进还没有有效关联，则只新增关联记录（检查同上），推进版本递增，结果为“已存在 · 已关联”，发布 `pursuit.pursuit.signal_linked.v1`，不再发布 `pursuit.pursuit.created.v1`；已有有效关联或请求不带信号时结果为“已存在”（成功、无改变）；两种情况请求中的负责人与任务都不写入；调用者无权查看时只返回负责人姓名。结果（已创建、已存在 · 已关联、已存在）连同返回时的授权状态（已生效 / 授权同步中）固定在操作标识上，同步完成不使操作再执行（[PRD 第6段](../../01-product/prd/06-pursuit-follow-up.md) F1、F2、F11、F12） |
| `VoidPursuit` | 命令 | 撤销创建：创建者在创建后 10 分钟内、推进版本仍为创建时的版本时，转为已撤销，取消任务与提醒、解除全部有效关联、清空期望参与关系并排队授权同步，在 pursuit 的一个本地事务中提交，提交后推进不再接受任何操作；版本已变（含之后新增关联）返回 `CONFLICT`；发布 `pursuit.pursuit.voided.v1` 与每条解除关联的 `pursuit.pursuit.signal_unlinked.v1`，detection 按有效关联集合决定是否恢复信号（[detection 第 4 节](detection.md#4-拥有的数据租户域)）；identity 不可用时返回“已撤销（授权清理中）” |
| `AdvanceStage` / `ClosePursuit` | 命令 | 阶段推进与关闭 |
| `PauseTracking` / `ResumeTracking` / `StopTracking` | 命令 | 持续型推进的暂停、恢复与停止 |
| `Assign` / `CreateTask` / `UpdateTask` / `CompleteTask` / `ReopenTask` | 命令 | 任务管理；`Assign` 可采用信号上的候选负责人（来自目标的分派规则，[customer-context 第 11 节](customer-context.md#11-分派配置)），写入前检查被分派人的访问，本地事务写入新的期望参与关系与参与版本，identity 参与关系经授权同步写入（第 5.1 节）；改派本地提交即生效，原负责人立即不能以负责人身份操作，新负责人在同步完成后才能操作与收到通知。写操作带推进或任务版本：请求的效果已成立（改派给当前负责人、完成已完成的任务、相同截止）为无变化，否则版本不一致返回 `CONFLICT`；`ReopenTask` 撤销完成，恢复完成前的状态、截止与提醒，带版本保护 |
| `ClosePursuit` / `RecordOutcome` | 命令 | 关闭并记录结果：结果类型与原因代码必填，成功与失败须有获知时间（可只到日，保存精度与时区），金额条目须带取值性质与币种；关闭取消未完成任务的提醒，不改变信号；修订追加新记录（第 4.1 节） |
| `ListPursuits` / `GetPursuit` | 查询 | 看板与详情 |
| `ListMyTasks` | 查询 | 本人的未完成任务（按截止时间，带精度与时区），供简报“待我处理”与工作台使用；无有效负责人的推进的任务列给目标范围的决策者 |

### 5.1 授权同步

推进的参与关系（负责人、协作者）的授权权威在 identity（OpenFGA），不与 pursuit 数据在一个事务中写入（[集成契约 2.3](../03-integration-contracts.md#23-跨子系统一致性)）。规则见 [PRD 第6段](../../01-product/prd/06-pursuit-follow-up.md) F12：

- **请求**：本地事务写入期望参与关系、参与版本 N 与发件箱同步请求；同步任务以推进标识 + N 为幂等键调用 identity `SyncResourceParticipants`（[identity 第 5 节](identity.md#5-接口)），提交完整期望关系；
- **重试与告警**：暂时失败退避重试（1 分钟起，最长间隔 30 分钟）；待处理超过 5 分钟告警；超过 24 小时标“授权同步失败”，通知目标范围的决策者与运营，只允许决策者改派或关闭，之后同步成功即恢复；
- **准入失败关闭**：依赖参与关系的操作须同时通过 pursuit 本地期望关系与 identity `Check`。新负责人在已应用版本达到 N 前不能操作；原负责人即使 identity 中残留旧关系也被本地关系拒绝；identity 中残留的同名旧关系也不算（负责人甲→乙→甲改派且同步失败时，甲在甲重新获得负责人角色的参与版本应用前不能写入）；按角色范围授权的决策者不受影响；
- **返回与通知**：命令在本地提交后返回，带授权状态；分派通知、任务提醒与提醒卡片只在对应参与版本同步完成后发给新负责人；
- **补偿**：identity 返回被拒成员（同步前已退出或失去目标访问）时，被拒的负责人按“无有效负责人”处理，提醒改发决策者并通知操作者。

## 6. 事件

**发布**：`pursuit.pursuit.created.v1`（含操作标识、来源信号与内容版本；只在真正创建时发布）、`pursuit.pursuit.signal_linked.v1` / `pursuit.pursuit.signal_unlinked.v1`（关联标识、推进、信号、所见内容版本；分区键为推进标识；每条关联各发布一次，发件箱重试，超过上限进入死信并告警，可人工重放）、`pursuit.pursuit.voided.v1`（撤销创建）、`pursuit.stage.entered.v1`、`pursuit.task.assigned.v1` 与 `pursuit.task.due.v1`（发给负责人的，在对应参与版本授权同步完成后才发布，第 5.1 节）、`pursuit.outcome.recorded.v1`、`pursuit.pursuit.closed.v1`。

**消费**：`assessment.decision.recorded.v1`、`detection.pursuit_related_event.v1`、`research.case.completed.v1`、`identity.member.changed.v1`。

## 7. 扩展点

- 机会路径定义（阶段、任务模板、衔接规则、结果 Schema）；
- 外部系统同步适配器（平台内部接口）。

## 8. 伸缩与可靠性

- 状态变化事务性写入并发布事件；
- 时限类提醒使用持久化定时器，保证在进程重启后不丢失。

## 9. 安全要点

- 外部协作者只能访问被邀请的推进的受限视图（授权关系 `can_view_limited`，见 [identity](identity.md#72-权限矩阵)）。受限视图按白名单投影，未列入白名单的字段一律不返回：

  | 外部协作者可见 | 外部协作者不可见 |
  |---|---|
  | 推进名称、目标对象的公共数据（公告、项目名称与截止时间） | 目标、策略、信号得分与命中理由 |
  | 当前阶段与阶段截止时间 | 判断与决策（含金额、批准人与理由） |
  | 分派给本人的任务：标题、说明、截止时间、状态 | 其他任务及其负责人 |
  | 本人提交的评论与附件 | 内部评论（评论默认对内，负责人显式标记为对外的才可见） |
  | 负责人显式标记为对外的评论与附件 | 结果、原因、对手信息与复盘 |
  | — | 外部系统映射、其他协作者的身份与联系方式 |

  受限视图使用独立的响应 Schema，不在完整视图上做字段删减；新增字段默认只进入完整视图。实现时以契约测试校验受限视图 Schema 不含右列字段，并以外部协作者身份调用 `GetPursuit` / `ListPursuits` 的负向用例覆盖右列每一类；
- 结果数据只用于本租户的学习与评估。

## 10. 分阶段交付

| 阶段 | 交付范围 | 不在本阶段 |
|---|---|---|
| M2 最小跟进切片 | 从信号或手动创建交易型推进；指派负责人与改派；截止时间与提醒；关闭并记录结果与原因代码；截止更正/延期的最小事件处理（见下）；创建持续型推进并管理暂停/恢复/停止，按周期请求 `channels` 生成变化简报 | 阶段编排、默认研究、决策门、路径衔接、外部协作者、CRM 同步 |
| M3 | 持续型推进按周期触发 `research` 模板（如 `RT-TENDER-ORGTRACK`），报告引用上一期以支持期间对比 | 同上 |
| M4 | 完整机会路径引擎：阶段与任务模板、决策门、通用领域事件挂钩（结果发布、终止等引起阶段变化）、路径衔接、复盘、外部协作者受限视图 | CRM 同步（M5） |

M2 的最小事件处理只消费 `detection.pursuit_related_event.v1` 中由截止时间变化引起的事件（`tendering.deadline.changed.v1`，或更正公告改变了投标截止时间）：截止时间绑定目标对象公开截止的任务按新时间更新，取消旧提醒并重建，变更记入审计；负责人手工设定的截止时间不改，只通知负责人公开截止已变化。其他推进相关事件（结果发布、终止等）在 M2 只由 `channels` 通知负责人，不改变推进状态；按事件推进阶段属于 M4 的通用挂钩。

M2 切片的用户流程、唯一性、撤销创建、提醒与结果规则见 [PRD 第6段](../../01-product/prd/06-pursuit-follow-up.md)。M2 切片沿用本文第 4–6 节的数据、接口与事件，不另建临时模型；授权使用 [identity 7.2](identity.md#72-权限矩阵) 已有的 `can_assign`、`can_edit`、`can_manage_team`。M2 验收必须覆盖：

- 重复点击“跟进”或重复提交：以幂等键返回同一推进，不重复创建任务或通知；
- 改派：原负责人失去“分派给本人”的任务视图，新负责人收到一次通知，审计记录前后负责人；
- 成员退出或停用：其负责的推进在列表中标记“无有效负责人”，提醒转给目标所在范围的决策者，不静默丢失；
- 截止时间变更（人工修改或领域更正导致）：取消旧提醒、按新时间重建，提醒只触发一次；
- 提醒投递失败：按 `channels` 的重试与站内兜底执行，提醒定时器在进程重启后不丢失；
- 关闭：结果与原因代码必填，金额条目与获知信息按第 4.1 节记录（M2 至少记录取值性质、币种与获知时间），修订结果时追加新的 `Outcome` 记录并留审计（结果为追加式，见[决策产物目录](../../02-business/01-business-architecture.md#6-决策产物目录)），结果进入 `learning` 的反馈。

## 11. 实现状况

**PRD 第6段交易型跟进的服务端切片（[pursuit 契约](../../../contracts/apis/pursuit/openapi.yaml) 0.2.0、[pursuit 事件](../../../contracts/events/pursuit/events.asyncapi.yaml) 0.2.0，均为草案）**：

- **接口**：`createPursuit`、`voidPursuit`、`assignPursuit`、`createTask`、`updateTask`、`completeTask`、`reopenTask`、`closePursuit`、`reviseOutcome`、`getPursuit`、`listPursuits`（视图 `mine` / `collaborating` / `team` / `closed`）、`getTask` 与 `listMyTasks`，经 gateway 公开；`createPursuit`、`getTask`、`completeTask`、`reopenTask`、`listMyTasks` 与 `getPursuit` 另经 mTLS 内部端口（`/internal` 前缀）与本地绑定 `pursuit.Actions` 供 channels 以点击者或收件人本人的用户主体调用（0.2.0 追加后两者，用于简报“待我处理”与授权被拒通知的发送前再检查；`getTask` 追加 `viewer_escalated`：任务因无有效负责人转给调用者决定，与 `listMyTasks` 同一口径；`viewer_can_edit` 与写命令同一准入），`channel` 为 `wecom` 的请求只在内部端口与本地绑定接受（公开接口 403）。`MIAOQIA_BINDING_PURSUIT=remote` 时 `Actions` 为远程客户端（`MIAOQIA_PURSUIT_URL`）。阶段、`AdvanceStage` 与外部协作者未实现（持续型推进见下方 C-10 一条）；
- **授权**：创建为目标上的 `can_assign`（customer-context `ObjectiveScope` 的当前口径：决策者经单元关系 `org_unit#can_assign_unit_objectives`，业务开拓者经参与关系），被分派的每个成员以其本人判断“租户有效成员（identity 会话核验）且能查看该目标的信号”，任一不满足 422 `assignee_no_access`、整个命令不写入、不自动授权；读取与更新按“单元关系（`org_unit#can_view_unit_pursuits` / `can_edit_unit_pursuits`），或 pursuit 本地期望关系中的有效负责人或协作者且 identity 在 `pursuit:` 资源上判断通过”放行，每次读取、命令、提醒发送前实时判断，不在快照中固定权限；判断不可用为 503，不当作无权限也不当作有权限；不能查看与不存在、他租户同为 404；
- **唯一性与关联**：同一目标、同一目标对象（事实类型 + 主体 + 部分 + 中标方）同时只有一个进行中的跟进（部分唯一索引 + 按目标对象的咨询锁）；已存在时信号尚未关联则新增关联（`existing_linked`，只发 `signal_linked`），否则 `existing`，请求中的负责人、协作者、任务与说明不写入并在 `not_written` 中列出；调用者不能查看已有跟进时只返回负责人。关闭后可再建并引用上一个，撤销的不占用唯一性；
- **操作与尝试**：操作标识固定一次意图（成功、无变化与确定失败；他人或不同请求使用同一操作标识 409 `operation_conflict`），幂等键记录每次尝试的结果（含失败与暂时失败；同键不同请求 409 `idempotency_conflict`）；`authorization_syncing` 不固定在操作上（同步完成后同一操作可成功）；结果每次返回前按调用者当前权限重新投影；
- **授权同步**：命令在本地事务中写入期望参与关系、参与版本与 `pursuit.team.changed.v1`，提交后立即以服务主体调用 identity `SyncResourceParticipants` 一次，失败由本子系统的 `pursuit.authorization-sync` 消费者按退避重试（`Reminders.RetrySync` 另可补做）；identity 拒绝的成员：负责人标“无有效负责人”、协作者移出，参与版本加一再同步，写时间线与审计，并发布 `pursuit.pursuit.authorization_rejected.v1`（被拒成员、关系与原因；`operator` 为最近一次变更团队的成员——创建、改派或撤销者，记在 `pur_pursuits.team_changed_by`——系统再校验触发的不带），channels 据此通知操作者；分派通知 `pursuit.pursuit.assigned.v1` 只在对应参与版本同步后发布一次；超过 24 小时未同步标 `failed`，只允许决策者改派或关闭；
- **写入准入**：改派、关闭、修订结果与新增 / 修改 / 完成 / 撤销完成任务经同一准入：不能更新跟进 403；单元关系（决策者）放行；只凭参与关系写入的成员在授权同步失败（超过 24 小时）时 403 `authorization_syncing`，其当前角色授予时的参与版本（`pur_pursuits.grants`）尚未在 identity 应用时同样 403 `authorization_syncing`（identity 中残留的同名旧关系不算）；加锁重读后再判断一次；`authorization_syncing` 不固定在操作上；
- **成员与目标变化**：消费 `identity.member.changed.v1`（`pursuit.member-changes`）与 `customer_context.objective.changed.v1`（`pursuit.objective-changes`），以成员本人重新判断负责人与协作者；退出、停用或失去目标访问的负责人标“无有效负责人”（列表置顶，提醒与待办转给单元决策者），协作者移出；重新加入不自动恢复；退出事件可能迟到或重放：每个参与成员的授予记录保存其当前角色授予时的参与版本与 identity 成员版本（创建时与改派时新加入、角色改变——负责人↔协作者——或失效后重新分派的成员都以其本人重新判断成员状态与目标访问并记下此刻的成员版本；只有角色不变的成员沿用原授予；带团队的“已存在”、关闭与修订结果不改授予），退出事件的成员版本不高于授予时成员版本的是授予之前的旧事件，不据此判无效，改以成员本人此刻的成员状态与目标访问判断；授予之后的退出判无效，重新加入不恢复；
- **任务与提醒**：截止保存所填值、精度与时区；提醒按（任务、截止版本、种类）持久计划在 `pur_reminders`（只到日：前一日与当日 09:00；分钟：提前 24 小时与到期；逾期次日 09:00；按所填时区），本地绑定 `pursuit.Reminders.RunDue` 以行锁并跳过已锁定的行执行，发送前按当前值再检查（跟进进行中、任务待办、截止版本未变、参与版本已同步，并以任务负责人本人判断写入准入：不满足——已不在团队中，或仍在团队中但不能更新、授权同步或授予版本未满足——时转给决策者，C-11），同一任务同一截止版本已过的多条只发最晚的一条；完成取消提醒，撤销完成（Web 10 秒、渠道 10 分钟）恢复；
- **关闭与结果**：结果类型、原因代码与金额类型取自应用领域包 `outcomes/*.yaml`（按目标模板）；成功与失败必填获知时间；金额不换算、不相加；贡献关系按信号 `created_at` 与获知时间判定（只到日且同日不判定，用户给出时以用户为准）；修订追加记录并引用旧记录；`pursuit.outcome.recorded.v1` 由 learning 幂等写入评估结果集；
- **翻页**：列表每页至多 50（待办 100），每次至多扫描 500 行后以游标返回；游标为整个载荷连同 HMAC-SHA256 签名（`MIAOQIA_PURSUIT_SIGNING_KEY`，base64、至少 32 字节；只有 development 可缺省为进程内随机密钥），绑定租户、调用者与视图，篡改 400 `invalid_cursor`；
- **进程角色**：pursuit 在 `api`、`consumer` 与 `workflow-worker` 角色加载；从信号创建需要 detection 的 `Briefing` 绑定，只在 `api` 角色必需（其他角色缺少时从信号创建失败关闭为 503）；
- **C-10（契约 pursuit 0.3.0、pursuit 事件 0.3.0、learning 0.4.0，均为追加）**：
  - **持续跟踪 F10（用例46、47）**：`createTracking`、`getTracking`、`listTracking`、`pauseTracking`、`resumeTracking`、`stopTracking`、`updateTrackingMembers`、`listTrackingPeriods`、`getTrackingPeriod`。持续型与交易型共用 `pur_pursuits` 的一行（`kind = continuous`），因而沿用同一授权、期望参与关系与授权同步、成员与目标变化的重新判断和写入准入（`admitWrite`，加锁重读后再判断一次）；交易型接口对持续型标识为 404，反之亦然，交易型列表不含持续型。状态（跟踪中 ⇄ 已暂停 → 已停止）、对象集合（组织标识，至多 200 个，成员版本每次修改加一）与周期（缺省每周一 08:00 Asia/Shanghai）在 `pur_tracking`；本地绑定 `pursuit.Reminders.RunTracking`（新增方法，`RunDue` 与 `RetrySync` 不变）以行锁并跳过已锁定的行结束到点的一期，写入 `pur_tracking_periods`（实际期间、期末成员快照与成员版本、对比基准为上一期、期末负责人与是否有效，追加式）并发布 `pursuit.tracking.period_closed.v1`；状态与对象变化发布 `pursuit.tracking.changed.v1`。暂停期间不生成、恢复后从恢复时起到下一个周期时点为一期，不补；执行者停止后错过的周期时点合并为一期并如实记录实际期间；停止保留已结束的各期。各期列表的游标与持续跟踪列表的游标都是整个载荷签名，绑定租户、调用者与跟踪或筛选；
  - **候选负责人（用例4）**：`getCreateDefaults` 给出创建面板的默认负责人。候选来自出站端口 `api.CandidateOwners`，候选以其本人判断仍是租户有效成员且能查看该目标的信号才作为默认（只在有效时返回其标识），否则为本人与 `no_access`；读取或判断失败 503 `assignment_rules_unavailable`，不退回本人。`createPursuit` 不带 `owner` 时仍以调用者本人为负责人。C-14 起生产组装使用适配器（`services/subsystems/pursuit/candidates.go`）：以调用者的用户主体经 detection 的 `SignalCandidates` 绑定（本地或经 mTLS 内部端口的远程客户端）读取信号生成时按目标分派规则写入的候选（[customer-context 第11节](customer-context.md#11-分派配置)）——`assigned` 为候选、`none` 为没有候选、`rules_unavailable`（信号生成时分派规则不可用）为本人与 `rules_unavailable`，读取失败、信号不属于所请求的目标或无法识别的状态为 503；存储的候选只是提示，每次仍以候选本人再检查。服务对外接口的 `api` 角色缺少该绑定时不启动，其他角色（不服务创建面板）为 `api.NotWired`；
  - **互相提示（用例12 的提示部分）**：`getPursuit` 追加 `sibling_hints`：同一目标、同一事实类型与主体上目标对象键不同的进行中交易型跟进（至多 20 个），每次读取按查看者对那个跟进的当前权限分级投影（白名单）：能查看为 `full`，否则 `owner_only` 只给负责人。跟进目标的部分是中标结果自身的标识、不含标包，暂不能缩小到同一标包（[A02 验收缺口 C-15](../../05-delivery/09-a02-end-to-end-acceptance.md#42-身份与渠道c)）；
  - **结果汇总（用例42）**：learning `getOutcomeSummary` 按截至时间读取每个跟进记录链上的当前记录，时点评估（获知时间在窗口内）与事后统计（获知时间缺失）分开给出，金额按类型、币种与性质精确合计；调用者须能查看目标且在目标所在单元上有 `can_view_unit_pursuits`（只凭参与关系的成员 403）。只读计算，不持久化报告（C-16）；
  - **证据**：`services/kernel/integration/pursuit_test.go`（`TestPursuitFollowUp` 的 “C-10 …” 子测试，含他租户、无关系成员、只能查看的成员、负责人被移出、篡改与他人游标、标识形态混用、确定失败固定的负向用例）、`services/subsystems/pursuit/internal/app/domain_internal_test.go`（周期、对象集合、提示白名单）、`services/subsystems/learning/internal/app/outcome_summary_test.go`；剩余项见 [A02 验收缺口 C-12～C-16](../../05-delivery/09-a02-end-to-end-acceptance.md#42-身份与渠道c)；
- **C-11（契约 pursuit 0.4.0，只改说明）**：转交的口径统一为 `assigneeAdmitted` / `escalatedFor`：提醒发出时负责人不满足写入准入即转交；`getTask` 的 `viewer_escalated` 与 `listMyTasks` 的 `escalated` 为“调用者在单元上有 `can_edit_unit_pursuits`，且任务负责人已不在团队中，或仍在团队中、此刻以本人判断不满足写入准入、且当前截止版本已有转交提醒发出”，负责人本人判断不可用时 503（失败关闭），负责人恢复准入后转交结束。`getTracking` 另经内部端口（`GET /internal/v1/pursuit/tracking/{id}`）与绑定 `pursuit.Trackings`（本地与远程）提供，供 channels 以授权被拒通知的操作者本人再检查持续跟踪。证据：`TestPursuitFollowUp` 的 “C-11 …” 子测试；
- **未实现**：公开截止绑定（F6，依赖 domain-intelligence 的截止事件，见[契约覆盖第9节](../13-contract-readiness.md#9-随功能追加的契约)）、持续跟踪的变化简报与关注（C-12）、持续跟踪的改派与对象来源版本（C-13）、分派规则按我方主体、组织单元与角色的条件（C-22）与规则变更后已有信号候选的重算（C-23）、授权同步待处理 5 分钟告警与超过 24 小时时通知决策者与运营（C-17）。`Reminders` 由 `scheduler` 角色的 `pursuit.run-due-reminders`、`pursuit.retry-authorization-sync` 与 `pursuit.run-tracking`（每分钟，逐个 active 租户以 `svc:pursuit` 调用，`services/subsystems/pursuit/jobs.go`，[A02 验收缺口 R-01](../../05-delivery/09-a02-end-to-end-acceptance.md#43-运行装配r)）调用。证据：`services/kernel/integration/pursuit_test.go`（`TestPursuitFollowUp`）、`services/subsystems/pursuit/internal/app/domain_internal_test.go`；
- **渠道侧（C-09，channels 实现）**：任务提醒卡片（“完成”“撤销”“查看”）、简报“待我处理”中的任务、授权被拒时通知操作者与提醒的安静时段由 channels 消费 `pursuit.task.due.v1` 与 `pursuit.pursuit.authorization_rejected.v1` 实现，每次发送与点击都以收件人或点击者本人经 `getTask` / `getPursuit`（持续跟踪经 `getTracking`，C-11）/ `completeTask` / `reopenTask` 再判断；通知的投递事件与统计（C-11）见 [channels 实现状况](channels.md)。
