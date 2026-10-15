# 反馈学习与评估子系统（learning）

> 状态：评审中 · 版本：1.0 · 更新：2026-10-12 · 平面：客户智能

## 1. 使命

让平台的判断随时间变得更准：采集反馈与结果，维护评估集，量化质量，仿真策略变化，并以“提案”而非“直接修改”的方式推动策略、模板和模型的改进。

## 2. 职责与非职责

**职责**

- 反馈采集：用户对信号、判断、研究报告的反馈及原因标签（来自所有渠道）；
- 标注任务：为评估集安排标注（客户或妙洽分析师），管理一致性；
- 评估集：租户级反馈集及租户使用的应用领域基线引用；公共基线随扩展包分发，由对应能力所有者维护；
- 策略仿真：给定策略草案，在历史数据上回放，输出新增、移除、处置变化、代表样本和分层指标；
- 质量评估：召回率、精确率、处置混淆矩阵、引用覆盖、人工否决率，按租户、来源、目标、阶段分层；
- 模型与提示回归：提供授权反馈数据集、编排回归与汇总结果；专业指标、执行器及发布门禁属于对应能力所有者；
- 漂移监测：反馈分布、否决率、数据质量的变化；
- 检索相关性数据：汇集本租户的查询、点击、否决和研究工具记录，形成租户反馈集；公共改进只输出经明确授权与治理审批、去标识化的不可变评估快照，由 search/evidence 导入并拥有，不提供公共实例直接回查租户数据的接口；
- 改进提案：生成策略调整建议（发给 `customer-context` 成为草案）与平台改进建议（发给 `operations`）。

**非职责**

- 不直接修改策略、模板或模型配置；
- 不使用某租户的数据改进其他租户的策略。平台级改进只能使用公共域数据及经明确授权、去标识化和治理审批的评估快照/聚合统计，不得直接使用原始租户数据。

## 3. 内部组件

| 组件 | 说明 |
|---|---|
| `feedback` | 反馈与原因标签 |
| `labeling` | 标注任务与质量控制 |
| `datasets` | 评估集与版本 |
| `simulator` | 策略回放仿真（Temporal 工作流，直接调用共享 `kernel/policy` 编译与求值；视图组装按固定版本的声明投影，不调用 detection，也不经过订阅反查；逻辑求值时间取被回放事件的时间，见 [策略 DSL](../11-policy-dsl.md#43-确定性与记录)） |
| `evaluator` | 指标计算与报告 |
| `drift` | 漂移监测 |
| `proposals` | 改进提案 |

## 4. 拥有的数据（租户域；公共基线由对应能力所有者维护）

`Feedback`、`LabelTask`、`EvaluationDataset` / `DatasetVersion`、`EvaluationRun`、`SimulationRun`、`QualityReport`、`Proposal`。

## 5. 接口

| 接口 | 类型 | 说明 |
|---|---|---|
| `RecordFeedback` | 命令 | 反馈（幂等、有限枚举） |
| `SimulatePolicy` | 命令 | 对策略草案仿真（异步） |
| `GetSimulationReport` | 查询 | 仿真结果 |
| `RunEvaluation` | 命令 | 编排所有者的专业回归并汇总；不发布模型/模板/框架 |
| `GetQualityReport` | 查询 | 分层质量指标 |

## 6. 事件

**发布**：`learning.feedback.recorded.v1`、`learning.simulation.completed.v1`、`learning.evaluation.completed.v1`、`learning.drift.detected.v1`、`learning.policy.proposal_created.v1`。

**消费**：`detection.signal.triaged.v1`、`assessment.assessment.created.v1`、`assessment.decision.recorded.v1`、`pursuit.outcome.recorded.v1`、`customer_context.policy.drafted.v1`（自动触发仿真）。

## 7. 扩展点

- 反馈原因标签体系（应用领域包）；
- 指标定义（应用领域包）；
- 应用领域包提供基线集；learning 持有租户使用的基线版本引用，专业公共基线由对应能力所有者维护。

## 8. 伸缩与可靠性

- 仿真与评估为批处理，资源与在线链路隔离；本子系统数据库查询走自己的只读副本与统计汇总表，外部数据通过公开接口或版本化快照获取，不跨库查询；
- 评估结果可复现：固定数据集版本与被评估对象版本。

## 9. 安全要点

- 原始租户反馈与结果只用于该租户；仅明确授权、去标识化并经治理审批的快照可交给能力所有者用于公共评估，公共实例不得回查原始数据；
- 跨租户聚合指标用于获批的平台运营与公共能力评估，不用于其他租户的策略；用途、样本及接收方都必须受明确授权与治理审批约束。

## 10. 评估责任契约

| 对象 | 样本所有权 | 专业指标与执行 | 发布/激活决定 |
|---|---|---|---|
| 客户策略效果与仿真 | learning 的租户数据集 | learning，求值复用 kernel/policy | customer-context，经 identity 审批 |
| 检索索引 | search 公共基线/获批快照，learning 租户反馈集 | 对应数据域的 search 实例 | search 别名门禁 |
| 文档解析/增强器 | evidence 公共语料/获批快照，信息领域包规则 | evidence 与对应领域包 | evidence 解析/增强版本门禁 |
| 模型/提示基础能力 | ai-platform 基线与获授权样本 | ai-platform | ai-platform 路由/提示版本门禁 |
| 研究模板/判断框架 | 领域包基线及 learning 授权租户反馈集 | research / assessment | research / assessment 各自版本门禁 |

learning 的 RunEvaluation 负责调度对应所有者的公开接口并汇总，不获得其发布权限。报告固定样本版本、数据域、授权/治理依据、被评估版本、指标定义与执行器版本。公共快照只包含获批的公共对象引用、去标识化标注或聚合指标，不导出租户原始文档、策略、上下文或身份；原始反馈仍归租户域。公共 evidence/search 只接收已发布公共评估快照；公共实例不得同步依赖 learning。撤回授权时，governance 编排各所有者删除或撤回受影响快照与派生制品。
