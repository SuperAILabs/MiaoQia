# 监测与匹配子系统（detection）

> 状态：评审中 · 版本：1.1 · 更新：2026-10-15 · 平面：客户智能

## 1. 使命

持续把外部世界的变化与每个租户的商业目标对齐：对每一个领域事实或事件，以高召回、低延迟的方式判断它对哪些租户的哪些目标可能有意义，并给出初步处置。

## 2. 职责与非职责

**职责**

- 监测订阅：由策略编译而来的监测条件，以及用户显式创建的关注（关注某组织、某项目、某市场）；
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
| `Watch` | 监测订阅：来源（策略/用户）、条件、目标、状态 |
| `Signal` | `sig_`：租户、目标、目标对象引用（含版本）、处置、命中规则、特征快照、排序分、策略版本、状态 |
| `SignalUpdate` | 信号对应对象发生变化的追加记录 |
| `MatchRun` | 回填运行记录 |

## 5. 接口

| 接口 | 类型 | 说明 |
|---|---|---|
| `ListSignals` / `GetSignal` | 查询 | 信号收件箱 |
| `CreateWatch` / `RemoveWatch` | 命令 | 用户关注 |
| `ExplainSignal` | 查询 | 命中/未命中的规则、证据、特征（可解释性） |
| `TriageSignal` | 命令 | 用户手动调整处置（同时产生反馈事件） |
| `RunBackfill` | 命令 | 回放匹配 |
| `DryRunPolicy` | 查询 | 给定完整输入快照的诊断求值；调用共享 kernel/policy，不是 learning 仿真的依赖 |

## 6. 事件

**发布**：`detection.signal.detected.v1`、`detection.signal.updated.v1`、`detection.signal.triaged.v1`、`detection.pursuit_related_event.v1`。

**消费**：领域事件（`domain.fact.upserted.v1` 及各领域事件）、`entity.merge.applied.v1`、`customer_context.policy.activated.v1`、`customer_context.objective.changed.v1`、`customer_context.profile.updated.v1` 与 `customer_context.entity_list.changed.v1`（按上下文版本提前刷新快照缓存）、`pursuit.pursuit.created.v1`（建立推进关联索引）。

## 7. 扩展点

- 应用领域包以 CEL 表达式提供特征函数、处置规则模板与信号合并规则（见 [可扩展性](../04-extensibility.md#32-表达式扩展点应用领域包与行业包cel)）；
- `DryRunPolicy`、在线匹配与 learning 仿真直接使用同一 `kernel/policy` 求值器。仿真遍历固定数据集，不依赖在线候选生成或订阅反查；输入投影须固定事实、上下文、视图、函数与逻辑时间版本。

## 8. 伸缩与可靠性

- 负载 = 领域事件速率 × 候选租户数；通过订阅反查把每个事件的候选集控制在实际可能命中的租户；
- 按租户单元（cell）分区消费，单元之间互不影响；
- 匹配为确定性计算：相同输入（事实版本、策略版本、上下文版本）产生相同输出，支持重放；
- 每批求值前读取 `customer-context.GetContextVersion`，按目标所属组织单元加载快照（缓存键含租户、组织单元与上下文版本），缓存较旧时先重新加载；读取失败时该批重试，不用可能过期的缓存求值；版本不高于缓存的事件被忽略（[PRD 第2段 第5节](../../01-product/prd/02-profile-and-objectives.md#5-业务与权限规则)）；
- 延迟目标：领域事件到信号生成 P95 < 1 分钟（规模化后的目标，具体见 [质量与验收](../../05-delivery/02-quality-and-acceptance.md)）。

## 9. 安全要点

- 每个信号绑定租户；订阅反查结果在进入租户处理前校验租户与策略归属；
- 信号上的特征快照不得包含其他租户的信息。
