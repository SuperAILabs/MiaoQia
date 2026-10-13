# 反馈学习与评估子系统（learning）

> 状态：评审中 · 版本：1.0 · 更新：2026-10-12 · 平面：客户智能

## 1. 使命

让平台的判断随时间变得更准：采集反馈与结果，维护评估集，量化质量，仿真策略变化，并以“提案”而非“直接修改”的方式推动策略、模板和模型的改进。

## 2. 职责与非职责

**职责**

- 反馈采集：用户对信号、判断、研究报告的反馈及原因标签（来自所有渠道）；
- 标注任务：为评估集安排标注（客户或妙洽分析师），管理一致性；
- 评估集：租户级与应用领域级的版本化评估集；
- 策略仿真：给定策略草案，在历史数据上回放，输出新增、移除、处置变化、代表样本和分层指标；
- 质量评估：召回率、精确率、处置混淆矩阵、引用覆盖、人工否决率，按租户、来源、目标、阶段分层；
- 模型与提示回归：模型、提示、研究模板、判断框架变更前的回归评估；
- 漂移监测：反馈分布、否决率、数据质量的变化；
- 检索相关性数据：汇集检索查询、点击与反馈、信号否决和研究工具的检索记录，形成检索相关性评估集，供 `search` 评估索引版本、供 `evidence` 评估增强器；
- 改进提案：生成策略调整建议（发给 `customer-context` 成为草案）与平台改进建议（发给 `operations`）。

**非职责**

- 不直接修改策略、模板或模型配置；
- 不使用某租户的数据改进其他租户的策略。平台级（跨租户）的改进只能使用公共域数据和明确授权的去标识化统计。

## 3. 内部组件

| 组件 | 说明 |
|---|---|
| `feedback` | 反馈与原因标签 |
| `labeling` | 标注任务与质量控制 |
| `datasets` | 评估集与版本 |
| `simulator` | 策略回放仿真（Temporal 工作流，复用 `detection.DryRunPolicy` 的匹配逻辑） |
| `evaluator` | 指标计算与报告 |
| `drift` | 漂移监测 |
| `proposals` | 改进提案 |

## 4. 拥有的数据（租户域；应用领域级评估集在公共域）

`Feedback`、`LabelTask`、`EvaluationDataset` / `DatasetVersion`、`EvaluationRun`、`SimulationRun`、`QualityReport`、`Proposal`。

## 5. 接口

| 接口 | 类型 | 说明 |
|---|---|---|
| `RecordFeedback` | 命令 | 反馈（幂等、有限枚举） |
| `SimulatePolicy` | 命令 | 对策略草案仿真（异步） |
| `GetSimulationReport` | 查询 | 仿真结果 |
| `RunEvaluation` | 命令 | 对模型/模板/框架变更执行回归 |
| `GetQualityReport` | 查询 | 分层质量指标 |

## 6. 事件

**发布**：`learning.feedback.recorded.v1`、`learning.simulation.completed.v1`、`learning.evaluation.completed.v1`、`learning.drift.detected.v1`、`learning.policy.proposal_created.v1`。

**消费**：`detection.signal.triaged.v1`、`assessment.assessment.created.v1`、`assessment.decision.recorded.v1`、`pursuit.outcome.recorded.v1`、`customer_context.policy.drafted.v1`（自动触发仿真）。

## 7. 扩展点

- 反馈原因标签体系（应用领域包）；
- 指标定义（应用领域包）；
- 评估集（应用领域包提供基线集）。

## 8. 伸缩与可靠性

- 仿真与评估为批处理，资源与在线链路隔离，查询走 PostgreSQL 只读副本与统计汇总表；
- 评估结果可复现：固定数据集版本与被评估对象版本。

## 9. 安全要点

- 租户反馈与结果只用于该租户；
- 跨租户聚合指标只用于平台运营，且必须去标识化并经治理审批。
