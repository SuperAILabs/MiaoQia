# 监测与匹配子系统（detection）

> 状态：评审中 · 版本：2.4 · 更新：2026-10-27 · 平面：客户智能

## 1. 使命

持续把外部世界的变化与每个租户的商业目标对齐：对每一个领域事实或事件，以高召回、低延迟的方式判断它对哪些租户的哪些目标可能有意义，并给出初步处置。

## 2. 职责与非职责

**职责**

- 监测订阅：由策略编译而来的监测条件，以及用户显式创建的关注（关注某组织、某项目、某市场，或由保存的检索转成的订阅，第 11 节）；
- 候选生成：领域事件到达时，通过 `search` 的订阅反查快速找出可能命中的租户与目标；
- 精确匹配：对候选执行策略的硬门槛、排除、特征计算和排序调整；
- 处置分层：`pursue_candidate`、`review_candidate`、`watch_signal`、`excluded`、`no_signal`；
- 信号管理：生成、去重、合并、更新信号（同一项目的多次公告合并为一个信号的多次更新）；
- 关联已有推进：领域事件与租户已有推进的关联（例如对正在推进的项目发布了更正）；
- 回填匹配：策略变更或新租户开通时，对历史窗口回放匹配。

**非职责**

- 不执行深度研究；
- 不负责通知投递（由 `channels` 根据事件处理）；
- 不改变策略（只读取）。

## 3. 内部组件

| 组件 | 说明 |
|---|---|
| `watch_registry` | 监测订阅：策略编译产物与用户关注 |
| `candidate_generator` | 订阅反查 + 实体关注索引 |
| `matcher` | 策略求值器：视图组装、三值求值、处置决策，语义见 [策略 DSL](../11-policy-dsl.md#4-求值语义) |
| `signal_store` | 信号存储、去重与合并 |
| `pursuit_linker` | 领域事件 → 租户已有推进的关联 |
| `backfill` | 历史回放匹配（Temporal 工作流） |

## 4. 拥有的数据（租户域）

| 实体 | 说明 |
|---|---|
| `Watch` | 监测订阅：来源（策略 / 用户关注 / 保存检索 / 持续型推进）、所有者、条件、目标（个人关注可无目标）、状态 |
| `WatchUpdate` | 关注对象的变化记录：关注、对象、变化类型、事实版本、时间；与信号分开 |
| `Signal` | `sig_`：租户、目标、目标对象引用（含版本）、处置、命中规则、特征快照、排序分、策略版本；固定的求值输入（[策略 DSL 4.3](../11-policy-dsl.md#43-确定性与记录)）：视图快照（`view_snapshot_hash`，快照内容按哈希保存，记录各视图项的对象与版本，保存期不短于信号）、客户上下文快照版本（`context_snapshot_version`）、逻辑求值时间（`evaluated_at`，触发事件的时间；回填时为被回放事件的时间）与触发事件（`trigger_event_id`）；生成时间（`created_at`，信号写入时间，回填时可远晚于 `evaluated_at`）；内容版本（已追加的更新数）；处理状态（待处理 / 已调整处置 / 已转跟进 / 已忽略 / 已关闭）与状态版本、人工处置（第 12 节） |
| `SignalTriage` | 信号的处理与反馈记录（追加式）：操作标识（唯一，用于去重）、操作者、动作（反馈、忽略、恢复、调整处置、撤回反馈、撤销）、反馈结果与原因标签、说明、所见内容版本；对实际被改变的部分（本人反馈、处理状态、人工处置）记录前值、后值与写入后的版本；撤销记录引用被撤销的操作；渠道、批次标识、时间 |
| `SignalBatchAction` | 批量处理作业：发起人、目标、动作、固定的信号标识清单（上限见第 12 节）、逐项的操作标识、尝试记录（幂等键、结果、是否已提交）与最终结果（成功 / 无变化 / 确定失败 / 暂时失败 / 未处理）、状态 |
| `SignalUpdate` | 信号对应对象发生变化的追加记录：记录时间（写入平台的时间）、触发、变化的字段与前后值、处置前后、内容版本前后；是否**实质更新**（处置上调，或中标方、金额、品类被更正，口径同 [PRD 第3段](../../01-product/prd/03-signal-inbox-feedback.md)第13节第4项），写入时判定，之后不改 |
| `MatchRun` | 回填运行记录 |

## 5. 接口

| 接口 | 类型 | 说明 |
|---|---|---|
| `ListSignals` / `GetSignal` | 查询 | 信号收件箱：按目标、分栏（待处理、已处理、已排除、已关闭）与筛选游标分页；信号随目标的 `can_view_signals` 授权；不属于本租户与不存在的信号返回相同结果；简报取数（`channels`）：给定目标与窗口 [起点, 终点) 及组装时点，返回组装时待处理、且窗口内新发现（`created_at`，不含回填信号）或窗口内有实质更新（`SignalUpdate` 的记录时间）的信号，每条信号一行：当前内容版本、排序分、类别（新 / 已更新）与窗口内的实质更新列表；以收件人身份授权（[PRD 第5段](../../01-product/prd/05-briefing-quick-actions.md) F1） |
| `CreateWatch` / `RemoveWatch` / `ListWatchUpdates` | 命令/查询 | 用户关注与变化记录（第 11 节）；也以 MCP 工具暴露 |
| `ListSignalsForObject` | 查询 | 对象页的本租户信号分区：按采购项目（及标包）、项目（经 `part_of` 归属边展开到采购项目）或组织（信号视图中的中标方、采购人）列出本租户信号，只返回用户有 `can_view_signals` 的目标的信号；租户上下文来自请求，不接受他租户标识（[PRD 第4段](../../01-product/prd/04-object-page-evidence.md) F3–F5） |
| `ExplainSignal` | 查询 | 命中/未命中的规则、证据、特征（可解释性）；同时返回固定的求值输入：求值单位版本、视图快照中各项的对象与版本（事实版本、实体版本、关系版本）、上下文快照版本、策略版本、`evaluated_at` 与 `created_at`。对象页的“信号判断时”视角先读这些固定版本，其余公共数据以 `evaluated_at` 为记录截止查询；视图快照已过保存期时返回“快照不可用”，调用方据此标“历史不可用”（[PRD 第4段](../../01-product/prd/04-object-page-evidence.md) F7） |
| `TriageSignal` | 命令 | 单条反馈、忽略、恢复、调整处置（第 12 节）：需 `can_feedback`，带操作标识、幂等键、所见内容版本、状态版本与反馈版本；写入 `SignalTriage` 并发布 `signal.triaged` |
| `TriageSignals` / `GetSignalBatch` / `CancelSignalBatch` / `RetrySignalBatchItems` | 命令/查询 | 批量处理：`202` 作业，每项的操作标识为批次标识 + 信号标识，逐项按 `TriageSignal` 的规则执行并给出结果；`RetrySignalBatchItems` 为暂时失败项与未处理项发起新的尝试 |
| `UndoSignalTriage` | 命令 | 撤销一次单条操作或一个批次：按第 12 节对被改变的部分追加补偿，逐项给出结果 |
| `RunBackfill` | 命令 | 回放匹配 |
| `DryRunPolicy` | 查询 | 给定完整输入快照的诊断求值；调用共享 kernel/policy，不是 learning 仿真的依赖 |
| `DiagnoseMiss` | 查询 | 供 learning 的漏报诊断调用：给定对象、目标与时点，返回当时是否生成候选、用当时固定的事实版本、生效策略与上下文重新求值的处置及命中/排除规则（与 `DryRunPolicy` 同一求值器）、信号与投递引用；无法恢复当时输入时返回“输入不足”。求值结果不是 `no_signal`（包括 `excluded`）而没有候选时标记为召回缺陷；不改写信号（[learning 第 11 节](learning.md#11-漏报反馈与诊断)） |

**实现状况（PRD 第3段信号收件箱与反馈的服务端切片，契约 [`contracts/apis/detection/openapi.yaml`](../../../contracts/apis/detection/openapi.yaml) 0.1.0 草案）**：

- **接口**：`ListSignals`（`GET /v1/detection/objectives/{objectiveID}/signals`）、`SelectSignals`（`…/signal-selection`，“全部符合筛选”的固定清单）、`GetSignal`、`ExplainSignal`、`ListSignalTriage`（处理记录，每页 50 条）、`TriageSignal`、`TriageSignals` / `GetSignalBatch` / `CancelSignalBatch` / `RetrySignalBatchItems` 与 `UndoSignalTriage`。`ListSignalsForObject` 见下文第4段的实现状况。简报取数见下文第5段的实现状况；关注、`RunBackfill`、`DryRunPolicy` 与 `DiagnoseMiss` 未实现；
- **授权**：每个用户入口都经 customer-context 的 `ObjectiveAccess` 绑定（远程时为内部接口 `GET /internal/v1/customer-context/objectives/{objectiveID}/access`）按目标所有者的口径判断 `can_view_signals` 与 `can_feedback`（identity 模型：目标的参与人、决策者与研究分析者可看；决策者、研究分析者以及身为参与人的业务开拓者可反馈，[identity 第7.2节](identity.md#72-权限矩阵)）。移除参与人在 customer-context 提交后立即撤权，即使 identity 的参与关系同步失败；授权服务或 customer-context 不可用时 503，不当作无权限。收件箱：不能查看目标与目标不存在同为 404 `objective_not_found`，能查看目标而不能看信号（不是参与人的管理员）为 403；单条信号：不存在、他租户与无权查看同为 404 `signal_not_found`，不返回内容。处理提交时实时判断，批量与撤销逐项判断；
- **收件箱**：一次只看一个目标；分栏、处置、事实类型、区域（含下级）、品类（含下级）、金额区间（未公布的不进入金额筛选）、发现时间、本人反馈与标记（回填、7 天内实质更新、有未知项）筛选；按排序分、发现时间或金额（未公布的在后）键集分页，每页至多 50 条，游标绑定目标、分栏、筛选与排序（不一致 400 `invalid_cursor`）。行内带同标包中标信号数与至多两条为真的规则说明；金额未公布时不出现金额。`SelectSignals` 超过 200 条时只返回数量与 `exceeds_limit`，不返回清单。详情的成员反馈、更新与处理记录各有上限并标明截断，处理记录全量经 `ListSignalTriage` 翻页；
- **处理与反馈**：按第 12 节的部分判断（反馈、处理状态、人工处置）实现无变化、冲突（409 带当前值）、`signal_updated`、`signal_closed` 与 `not_applicable`；反馈原因取自应用领域包（`packs/application_domains/*/feedback/*.yaml`，按目标代码），不相关与排除有误至少一个原因，标为须说明的原因须写说明，调整处置须写原因（422 `invalid_feedback`）。操作结果（成功、无变化、确定失败）固定在操作标识上（`det_triage_ops`，追加式）：同一操作标识的新尝试直接返回已固定的结果，用于不同请求或由他人使用时 409 `operation_conflict`；尝试按（调用者、接口、幂等键）保存（`det_triage_attempts`），同键重发返回该尝试的结果，包括 503 等暂时失败，同键不同请求 409 `idempotency_conflict`；暂时失败不固定在操作标识上，新键同一操作标识可再次尝试。每个改变了部分的操作发布 `detection.signal.triaged.v1` 并写一条审计（资源为信号、操作、内容版本与状态版本的引用；原因与说明只留在处理记录）；
- **批量**：请求内逐项同步执行（不经工作流），作业以数据库时钟的租约（缺省 2 分钟，每项前续租并检查取消）防止两个执行者同时处理；执行者中断而租约过期、仍有未处理项时为 `interrupted`，`RetrySignalBatchItems` 只为暂时失败项与未处理项发起新的尝试，已提交与确定失败的项不在其中。只有发起人可读、取消、继续（其他人与不存在同为 404）。批量“不相关”缺省同时忽略；
- **撤销**：撤销是新的操作，每项的操作标识为“撤销操作标识:信号标识”。按第 12 节逐部分补偿并核对版本；整批撤销的范围是已提交且至少一个部分改变的项（含“状态无变化，反馈已记录”的项）。撤销调整处置恢复原人工处置，但不恢复原调整原因（缺口）；
- **未实现**：重新求值与撤回关闭的生产者（`SignalUpdate` 与 `closed` 的写入方随视图触发与撤回识别实现，测试以拥有者账号写入）、`converted`（随推进）、回填执行、`signal.updated` 事件、来源中断与展示级别、漏报（随 learning 第11节）。

**实现状况（PRD 第4段对象页的服务端切片，契约 0.2.0 草案）**：

- **`ListSignalsForObject`**：`GET /v1/detection/object-signals?object_id=…`，对象为采购项目（`pp_`）、项目（`prj_`，经 `api.ProjectLinks` 端口按归属边展开到采购项目，读取失败 503 `project_links_unavailable`）或组织（`org_`，`role` 为 `winner` / `buyer` / `any`，按信号视图中的中标方与采购人）。先取命中的目标，再逐目标经 `ObjectiveAccess` 判断 `can_view_signals`，只返回可看目标的信号；判断失败时 503，不当作无权限。每页至多 20 条，按（生成时间、标识）倒序键集分页，游标绑定对象、角色与调用者。信号写入时从结果视图记录采购项目与采购人（`det_signals.procurement_project_id`、`buyer_org_id`、`buyer_name`，迁移 00002），`SignalRow` 追加这两个可选字段；之前写入的信号没有这两列，不出现在按采购项目或采购人的查询中（中标方照常）；
- **信号解释的内部绑定**：`SignalExplainer`（本地为 `ExplainSignal` 的同一实现，远程为内部接口 `GET /internal/v1/detection/signals/{signalID}/explanation`）供 gateway 读取固定的求值输入，按用户主体判断，与公共接口的结果一致；
- **缺省**：归属边端口的生产适配器（entity-graph 的 `part_of`）未接入，`detection.Module()` 使用 `api.NotWired`，项目页的信号分区 503；测试使用 `api/facttest`。

**实现状况（PRD 第5段简报取数与卡片的服务端切片，契约 0.3.0 草案）**：

- **简报取数**（`GET /v1/detection/objectives/{objectiveID}/briefing-signals?from=…&to=…`，内部接口 `GET /internal/v1/detection/objectives/{objectiveID}/briefing-signals`，绑定 `detection.Briefing`）：只供 channels 以收件人的用户主体调用，授权同收件箱（`can_view_signals`，不可看与不存在同为 404）。窗口 [from, to) 至多 62 天；列出组装时待处理（`open` / `triaged`）、非排除、且窗口内新发现（不含回填）或窗口内有实质更新的信号，每条一行，带类别（新 / 已更新）、是否更正类（窗口内的实质更新改动中标方、金额或品类）与窗口内的更新；更正类在前，其次排序分降序、发现时间新到旧，至多 200 行。另给出新增、更新、已处理、待处理总数与回填数，以及调用者能否反馈；
- **信号卡片**（`GET /v1/detection/signal-cards?ids=…`，内部接口同名，至多 50 个）：返回调用者可见的信号的当前行、最近一次处理状态改变（处理人与时间）与能否反馈；不可见、不存在与其他租户的信号不出现；任一目标的授权判断失败整次 503，不退化为部分结果。channels 用于卡片动作前读取当前版本、结果回写的当前值、`SIGNAL_UPDATED` 的新卡片与完整简报的状态叠加；
- **渠道动作**：`TriageSignal` 与 `UndoSignalTriage` 经同一绑定以点击者的用户主体调用（内部接口 `POST /internal/v1/detection/signals/{signalID}/triage`、`POST /internal/v1/detection/triage-undos`），规则与公共接口相同；请求的 `channel` 为 `wecom` 时 `detection.signal.triaged.v1` 记录渠道。

**实现状况（PRD 第6段“已转跟进”）**：消费 pursuit 的 `pursuit.pursuit.signal_linked.v1` / `signal_unlinked.v1`（`detection.pursuit-links`），以关联标识幂等维护每条信号的有效关联集合（`det_pursuit_links`，先于加入到达的移除记为墓碑）：集合从空变为非空时记录转换基线（处理状态与人工处置）与转换时的状态版本 V，转为 `converted`（信号已关闭时只记录关联）；集合变空时只有状态版本仍为 V 才恢复基线，否则保持当前值，之后清除基线。晚到或重复的事件不改变结果。

## 6. 事件

**发布**：`detection.signal.detected.v1`、`detection.signal.updated.v1`、`detection.signal.triaged.v1`（载荷含操作标识、动作、反馈结果与原因标签、所见内容版本、各被改变部分的前后值、渠道与批次标识；撤销同样发布并引用被撤销的操作，`learning` 据此写入反馈集，以操作标识幂等）、`detection.pursuit_related_event.v1`。

已实现发布 `detection.signal.detected.v1`（带固定的求值输入：策略标识、版本与代次、模板版本、上下文快照版本、`evaluated_at` 与 `created_at`）与 `detection.signal.triaged.v1`（载荷不含说明），契约见 [`contracts/events/detection/events.asyncapi.yaml`](../../../contracts/events/detection/events.asyncapi.yaml) 0.1.0 草案；已实现消费 `domain.fact.upserted.v1`（第8节实现状况）。策略与上下文的变更事件不消费：正确性只依赖每批读取。

**消费**：领域事件（`domain.fact.upserted.v1` 及各领域事件）、`entity.merge.applied.v1`、`customer_context.policy.activated.v1` 与 `customer_context.policy.rolled_back.v1`（按策略代次提前刷新策略缓存）、`customer_context.objective.changed.v1`、`customer_context.profile.updated.v1` 与 `customer_context.entity_list.changed.v1`（按上下文版本提前刷新快照缓存）、`pursuit.pursuit.created.v1`（建立推进索引，不改变处理状态）、`pursuit.pursuit.signal_linked.v1` / `pursuit.pursuit.signal_unlinked.v1`（按关联标识加入或移除信号的有效关联，派生已转跟进，见第 4 节处理状态）。

## 7. 扩展点

- 应用领域包以 CEL 表达式提供特征函数、处置规则模板与信号合并规则（见 [可扩展性](../04-extensibility.md#32-表达式扩展点应用领域包与行业包cel)）；
- `DryRunPolicy`、在线匹配与 learning 仿真直接使用同一 `kernel/policy` 求值器。仿真遍历固定数据集，不依赖在线候选生成或订阅反查；输入投影须固定事实、上下文、视图、函数与逻辑时间版本。

## 8. 伸缩与可靠性

- 负载 = 领域事件速率 × 候选租户数；通过订阅反查把每个事件的候选集控制在实际可能命中的租户；
- 按租户单元（cell）分区消费，单元之间互不影响；
- 匹配为确定性计算：相同输入（事实版本、策略版本、上下文版本）产生相同输出，支持重放；
- 每批求值前读取 `customer-context.GetContextVersion`，按目标所属组织单元加载快照（缓存键含租户、组织单元与上下文版本），缓存较旧时先重新加载；读取失败时该批重试，不用可能过期的缓存求值；版本不高于缓存的事件被忽略（[PRD 第2段 第5节](../../01-product/prd/02-profile-and-objectives.md#5-业务与权限规则)）；
- 每批求值前同样读取各目标的策略代次（`GetActivePolicies`），缓存较旧时先重新加载；读取失败时该批重试。激活或回滚返回后开始的批次一律使用新版本，事件只用于提前刷新（[PRD 第7段](../../01-product/prd/07-policy-simulation-approval.md) 第5节）；
- 延迟目标：领域事件到信号生成 P95 < 1 分钟（规模化后的目标，具体见 [质量与验收](../../05-delivery/02-quality-and-acceptance.md)）。

**实现状况（生效边界上的匹配，PRD 第7段第5节）**：消费公共域的 `domain.fact.upserted.v1`，经订阅反查端口（`api.Candidates`）得到候选租户、经事实视图端口（`api.Facts`）得到求值单位（A02 结果视图为“公告 + 一条中标结果”，`subject` 为 `ntc_` 与 `part_id` `bdr_`），每个候选租户在各自的租户事务中处理。每批开始时以服务主体经 `customer-context.ActivePolicies` 读取全部生效策略（按目标分页读完）与 `GetContextVersion`，任一读取失败时事件返回错误、按消费框架退避重试，不用缓存求值；编译结果与上下文快照只按不可变的键缓存（目标、版本与文档摘要；租户、组织单元与上下文版本），不决定使用哪个版本。只对状态为生效的目标、范围内的事实类型与启用领域内的事实求值；启用领域外的关联视图项不进入求值输入（读为未知，`DOMAIN_NOT_ENABLED`）。匹配记录 `det_matches` 以（目标、求值单位、事实版本）为主键、不含代次：同一事实版本最多求值一次，激活或回滚后的重复投递不按新代次重算，已有信号保留原策略版本与解释（PRD 第7段用例 16）。每条信号固定策略标识、版本与代次、模板版本、启用领域集合、上下文版本、视图快照哈希（快照按哈希保存）、触发事件与 `evaluated_at`（业务时间，触发事件时间），`created_at` 为写入时间。未知原因按“数据未接入 > 品类未能可靠识别 > 低于证据门槛 > 未关联本企业 > 字段缺失”取一个。缺省与缺口：同一求值单位的后续版本记为 `not_triggered`（A02 结果视图没有声明重新求值的触发字段，重新求值未实现）；客户、禁合作与竞争对手清单为空；`HISTORY_INSUFFICIENT` 不产生；规则到信息领域的生产对应未接入，B-PERMIT 在结果视图上按事件类型判为不满足而不是“数据未接入”。两个端口的生产适配器（按视图声明向数据所有者 domain-intelligence 与 entity-graph 组装视图、search 的订阅反查，[策略 DSL 5.1](../11-policy-dsl.md#51-变量)）未接入：`detection.Module()` 使用 `api.NotWired`，事件处理失败并重试、转入死信；测试使用 `api/facttest` 的内存夹具，事实全部虚构。

**实现状况（候选负责人，C-14；PRD 第6段 F1 第2条，契约 0.4.0 草案）**：生成信号时按目标的分派规则算出候选负责人，与所用的分派配置版本一同写入信号（`det_signals.candidate_owner`、`assignment_version`，迁移 `00004_candidate_owner.sql`）。规则、负责人与参与人来自批次开始时同一次 `GetActivePolicies` 读取（customer-context 0.7.0 的 `assignment`）；按顺序取第一条命中的规则（区域按 `region.within`、品类按 `category.any_match` 的同一口径，`kernel/policy` 的 `RegionWithin` / `CategoryCovered`，匹配信号的地点与品类），被分派人已不是该读取中的负责人或参与人的规则跳过；没有分派配置（目标从未配置分派规则，生效策略读取不出现 `assignment`；或旧版本的提供方）时两列为空，读取时说明为“分派规则不可用”（`rules_unavailable`），不当作“没有候选”；已配置但规则为空或都不命中时为“没有候选”（`none`，记录所用的分派配置版本，版本从 1 开始）。候选只是建议，不授予访问，也不参与求值与处置。只经内部端口的 `getSignalCandidateOwner`（`GET /internal/v1/detection/signals/{signalID}/candidate-owner`，绑定 `detection.SignalCandidates`，本地与远程）以调用者的用户主体判断：不可见、不存在与他租户 404 `signal_not_found`，能查看信号而没有 `can_assign` 403（候选人标识按级别白名单只对能分派跟进的成员可见）；只返回生成时记录的值，不重新求值规则，使用方（pursuit 的创建面板）以候选本人再检查。规则变更后已有信号的候选不重算，候选也不在 `getSignal`、卡片与通知中展示（[A02 验收缺口 C-23](../../05-delivery/09-a02-end-to-end-acceptance.md#42-身份与渠道c)）。证据：`services/kernel/integration/assignment_test.go`（`TestAssignmentRules`）、`services/subsystems/detection/internal/app/assignment_test.go`。

## 9. 安全要点

- 每个信号绑定租户；订阅反查结果在进入租户处理前校验租户与策略归属；
- 信号上的特征快照不得包含其他租户的信息。

候选与匹配记录（包括 `no_signal` 的匹配统计与 `excluded` 信号）至少保留到漏报诊断所需的时间范围，否则 `DiagnoseMiss` 对更早的时点只能回答“暂不能诊断”；保留期随漏报功能确定。

## 11. 关注

关注（`Watch`）回答“这个对象有什么变化”，信号回答“这对某个目标是否值得处理”。两者分开：关注到的变化写入 `WatchUpdate`，不必生成商业信号；同一变化同时满足某个目标的策略时，策略照常生成信号，两边互相引用。

| 种类 | 规则 |
|---|---|
| 目标内关注 | 关注挂在目标下，变化可进入该目标的简报区；持续型推进的对象由推进维护 |
| 个人关注 | 不挂目标，只属于创建者；变化只进入其本人“关注变化”区与通知，不进入任何目标的信号、排序或评估；数量受租户权益限制 |
| 保存检索转订阅 | 把一次检索的条件保存为订阅：固定结构化条件与全文查询，记录检索语义版本（查询解析与分析器版本）；只对创建之后新记录或新版本的事实提示，索引重建或检索语义升级不重复提示已见过的事实；语义升级后命中范围可能变化时，提示用户确认是否升级到新版本 |
| MCP 工具 | `gateway` 以 `create_watch`、`list_watch_updates`、`remove_watch` 暴露，作用域 `watches:read` / `watches:write`，租户与所有者来自令牌 |

权限：关注以所有者身份执行，每条变化在写入和通知时都按所有者当前的权限与来源权利过滤；所有者失去成员资格时关注停用。关注不使用其他成员的租户数据，个人关注对其他成员不可见。

## 12. 信号处理与反馈

用户对信号的处理分两部分：**处理状态**（含人工处置）属于信号、团队共享；**反馈**按人保存，表达“这条信号对本目标是否有用”。两者都经 `TriageSignal` / `TriageSignals` 进入，本子系统写入 `SignalTriage` 并发布 `signal.triaged`；`learning` 消费该事件维护租户反馈集，本子系统不同步调用 `learning`。用户可见的流程、文案与默认做法见 [PRD 第3段](../../01-product/prd/03-signal-inbox-feedback.md)（F4–F6、F9）。

| 方面 | 规则 |
|---|---|
| 处理状态 | `open` 待处理、`triaged` 已调整处置、`converted` 已转跟进（由有效关联集合派生）、`dismissed` 已忽略、`closed` 已关闭（求值单位进入终态，或事实被撤回，[domain-intelligence 第 11 节](domain-intelligence.md#11-事实撤回与恢复)）。`converted` 只由推进的关联决定：每个信号保存有效关联集合（关联标识与推进标识），`signal_linked` 加入、`signal_unlinked` 移除，以关联标识幂等，已移除的关联标识保留为墓碑，同一关联晚到的加入或移除不再生效（不只依赖事件 `id` 去重）。集合从空变为非空时记录转换基线（转换前的处理状态与人工处置），未关闭的信号转为 `converted`（系统写入，状态版本递增）并记下写入后的状态版本 V；已关闭的信号只记录关联。集合非空时再加入只记账，不改状态与基线。移除只移除该关联；集合变空时，若状态版本仍为 V 则恢复基线，否则保持当前值，然后清除基线。因此不依赖跨推进的事件顺序：撤销 P1 后立即创建 P2、P2 的加入先于 P1 的移除到达时，基线仍是 P1 转换前的值，P2 撤销后信号回到该值。推进关闭不改变 `converted`（[PRD 第6段](../../01-product/prd/06-pursuit-follow-up.md) F3） |
| 人工处置 | 只在跟进候选、复核候选、观察之间调整，必须写原因；系统处置、得分与轨迹不变；不触发策略的 `route`。排除信号与已关闭信号不能调整处置 |
| 版本 | 内容版本：重新求值追加 `SignalUpdate` 时递增。状态版本：处理状态或人工处置任一改变时递增。反馈版本：按（信号、成员）计，本人反馈每次改变时递增 |
| 无变化与冲突 | 一个动作按部分（本人反馈、处理状态、人工处置）逐项判断：请求的效果已成立（值相同）的部分为无变化，不写记录、不校验版本；其余部分须所带版本等于当前版本，否则整个动作返回 `CONFLICT` 与当前值。处理状态都是 `triaged` 但人工处置不同不算无变化；信号已忽略不妨碍写入本人反馈。信号已关闭时，动作中的处理状态部分为不适用（`NOT_APPLICABLE`），反馈部分照常写入，一项结果按部分汇总；已转跟进的信号上，忽略、恢复与调整处置的处理状态部分同样为不适用，反馈照常写入（[PRD 第6段](../../01-product/prd/06-pursuit-follow-up.md) F3 第2条） |
| 内容版本绑定 | 每次动作记录所见内容版本；与当前内容版本不一致时拒绝（`SIGNAL_UPDATED`）。重新求值不清除已有反馈与处理状态；已忽略信号遇到更新时是否回到待处理按 PRD 第3段第13节第4项 |
| 反馈 | 结果为相关、不相关、排除正确、排除有误，原因标签取自应用领域包；每人每条信号以最新记录为当前反馈，修改与撤回都追加记录 |
| 渠道来源 | 渠道卡片上的动作由 `channels` 以点击者身份调用 `TriageSignal` / `UndoSignalTriage`：操作标识为卡片按钮的动作令牌标识，每次调用是一次尝试并带自己的幂等键；`SignalTriage` 与 `signal.triaged` 记录渠道（如企业微信）。规则与 Web 完全相同，不另设渠道专用的判断 |
| 授权与隔离 | 每次提交实时检查目标上的 `can_feedback`，批量逐项检查；不属于本租户或不存在的信号以同一原因码失败，不返回其内容 |
| 操作与尝试 | 每个用户意图（单条提交或批量中的一项）有固定的操作标识，`SignalTriage` 以操作标识唯一约束，同一操作最多提交一次。每次请求是一次尝试，带自己的幂等键，遵守[集成契约 2.2](../03-integration-contracts.md#22-约定)的同键同结果：同键重发返回该尝试的结果，已提交而响应丢失时返回已提交的结果。尝试先按操作标识查找已提交的结果，有则直接返回，不再执行 |
| 最终结果与重试 | 成功、无变化与确定失败（`FORBIDDEN`、`SIGNAL_UPDATED`、`SIGNAL_CLOSED`、`NOT_APPLICABLE`、`CONFLICT`、参数校验失败）固定在操作标识上，不再重试；确认未提交的暂时失败（`INTERNAL`、`UNAVAILABLE`）只是该尝试的结果，可用新的幂等键、同一操作标识再次尝试。批量取消后不再发起新的尝试，未尝试的项为未处理，继续时发起首次尝试 |
| 批量 | 选中时固定信号标识清单（上限按 PRD 第3段第13节第3项），之后筛选变化不扩大范围；逐项结果为成功 / 无变化 / 确定失败 / 暂时失败 / 未处理；一项按其各部分汇总：任一部分改变即为成功（并记录哪些部分改变，例如处理状态无变化、反馈已写入），没有任何部分改变才为无变化 |
| 撤销 | 撤销是新的操作。对原操作实际改变的每个部分追加补偿，未改变的部分不补偿：反馈原来没有则撤回，原来有则恢复原值及其所基于的内容版本；忽略恢复为原处理状态；恢复重新忽略；调整处置恢复原人工处置，原来没有则回到待处理。补偿前核对该部分当前版本等于原操作写入后的版本，不等（之后有人改过，包括本人）返回 `CONFLICT`；原操作之后内容版本已变化返回 `SIGNAL_UPDATED`；信号已关闭返回 `SIGNAL_CLOSED`。撤销批次时范围为已提交且至少一个部分改变的全部项，每项只补偿其改变的部分；一项内各部分的补偿在同一事务中提交；撤销不能再撤销 |
| 不改策略与事实 | 处理与反馈不改变策略、系统处置与排序分，也不修改公共事实；策略调整只经 customer-context 的草案、仿真与审批 |
