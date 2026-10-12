# 推进与结果子系统（pursuit）

> 状态：评审中 · 版本：1.0 · 更新：2026-10-12 · 平面：客户智能 · 首期部署单元：`platform`

## 1. 使命

把批准的决策变成有负责人、有节奏、有记录的推进工作，并收集结果与原因，形成复盘与学习的闭环。

## 2. 职责与非职责

**职责**

- 推进实例：按机会路径定义创建推进，管理阶段与状态；
- 阶段编排：阶段进入时触发默认研究、生成任务模板、设置决策门；
- 任务与分派：任务、负责人、截止时间、协作者、提醒；
- 领域事件响应：与推进相关的领域变化（更正、延期、结果发布）引起提醒或阶段变化；
- 路径衔接：按应用领域包定义的衔接规则建议新推进（如投标落败 → 进入供货）；
- 结果与复盘：结果、原因、对手信息、复盘记录；
- 外部系统同步：与 CRM、项目管理工具的双向同步（通过集成适配器）。

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
| `sync_adapters` | CRM、项目管理工具同步 |

## 4. 拥有的数据（租户域）

| 实体 | 说明 |
|---|---|
| `Pursuit` | `pur_`：目标对象、目标、机会路径版本、阶段、状态、负责人、关联决策与信号 |
| `StageTransition` | 阶段变化与原因 |
| `Task` | 任务、负责人、截止、状态 |
| `Outcome` | 结果类型、金额、原因代码、说明、证据 |
| `Retrospective` | 复盘记录 |
| `ExternalLink` | 与外部系统记录的映射 |

## 5. 接口

| 接口 | 类型 | 说明 |
|---|---|---|
| `CreatePursuit` | 命令 | 从决策、信号或手动创建 |
| `AdvanceStage` / `ClosePursuit` | 命令 | 阶段推进与关闭 |
| `Assign` / `CreateTask` / `CompleteTask` | 命令 | 任务管理 |
| `RecordOutcome` | 命令 | 结果记录 |
| `ListPursuits` / `GetPursuit` | 查询 | 看板与详情 |

## 6. 事件

**发布**：`pursuit.pursuit.created.v1`、`pursuit.stage.entered.v1`、`pursuit.task.assigned.v1`、`pursuit.task.due.v1`、`pursuit.outcome.recorded.v1`、`pursuit.pursuit.closed.v1`。

**消费**：`assessment.decision.recorded.v1`、`detection.pursuit_related_event.v1`、`research.case.completed.v1`。

## 7. 扩展点

- 机会路径定义（阶段、任务模板、衔接规则、结果 Schema）；
- 外部系统同步适配器 SPI。

## 8. 伸缩与可靠性

- 状态变化事务性写入并发布事件；
- 时限类提醒使用持久化定时器，保证在进程重启后不丢失。

## 9. 安全要点

- 外部协作者只能访问被邀请的推进的受限视图；
- 结果数据只用于本租户的学习与评估。
