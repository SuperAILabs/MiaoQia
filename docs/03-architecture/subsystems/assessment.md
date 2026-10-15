# 判断与决策子系统（assessment）

> 状态：评审中 · 版本：1.0 · 更新：2026-10-12 · 平面：客户智能

## 1. 使命

把领域事实、信号、研究结果和客户上下文转化为租户私有的、可解释的判断，并把需要人批准的结论组织成决策草案，记录最终决策。

## 2. 为什么独立于研究

判断的输入不只来自研究：大量信号只需轻量的规则判断即可处置；同一研究报告也可能被不同目标、不同时点重复判断。判断与决策还承担审批与责任记录。因此判断是一个独立的上下文，研究报告是它的输入之一。

## 3. 职责与非职责

**职责**

- 判断框架执行：按目标的判断框架计算各维度（相关性、可信度、可行性、风险、价值、优先级）；
- 分层记录：硬门槛结果、规则特征、模型评分、AI 归纳分别记录；
- 研究请求：框架判定信息不足时请求 `research` 发起研究；
- 决策草案：如 bid/no-bid、是否跟进供货、是否投入前期资源；
- 决策记录：审批通过后形成不可变决策，记录批准人与理由；
- 重新判断：领域事实变化、策略变化或新研究完成时生成新的判断版本。

**非职责**

- 不执行审批流程（由 `identity` 的审批服务执行，本子系统提交并消费结果）；
- 不管理推进与任务（属于 `pursuit`）。

## 4. 内部组件

| 组件 | 说明 |
|---|---|
| `framework_registry` | 判断框架（应用领域包提供，版本化） |
| `evaluator` | 执行框架：确定性门槛与特征、模型评分（经 `ai-platform`）、AI 归纳 |
| `feature_snapshot` | 判断使用的特征快照 |
| `decision_manager` | 决策草案、审批提交、决策记录 |

## 5. 拥有的数据（租户域）

| 实体 | 说明 |
|---|---|
| `Assessment` | `asm_`：目标对象引用、目标、框架版本、策略版本、上下文快照、研究报告引用、`hard_gates[]`、`dimensions{}`、`facts[]`、`inferences[]`、`missing_information[]`、`risk_factors[]`、`recommended_actions[]`、`rank_score`、模型版本 |
| `FeatureSnapshot` | 特征值与来源 |
| `DecisionDraft` | 决策草案与审批请求引用 |
| `Decision` | `dec_`：结论、理由、批准人、时间、依据的判断版本 |

`rank_score` 只用于同一策略下的排序，不代表事实正确率、可信度或成交概率。

## 6. 接口

| 接口 | 类型 | 说明 |
|---|---|---|
| `Assess` | 命令 | 对目标对象按目标框架执行判断 |
| `GetAssessment` / `ListAssessments` | 查询 | 判断与历史版本 |
| `DraftDecision` | 命令 | 生成决策草案 |
| `SubmitDecision` | 命令 | 提交审批 |
| `GetDecision` | 查询 | 决策记录 |

## 7. 事件

**发布**：`assessment.assessment.created.v1`、`assessment.research.requested.v1`、`assessment.decision.drafted.v1`、`assessment.decision.recorded.v1`。

**消费**：`detection.signal.detected.v1`、`detection.signal.updated.v1`、`research.case.completed.v1`、`identity.approval.decided.v1`、`customer_context.policy.activated.v1`。

## 8. 扩展点

- 判断框架：维度、门槛、评分规则、所需研究模板、决策类型（应用领域包）；
- 评分模型可插拔（规则、统计模型、学习排序），由本子系统执行专业回归与启用门禁，learning 提供授权数据集和评估编排。

## 9. 伸缩与可靠性

- 轻量判断为同步或事件驱动，低延迟；涉及模型的判断异步执行；
- 判断可按固定版本重放，用于复盘与评估。

## 10. 安全要点

- 决策必须由具备该决策类型授权的人批准；AI 只生成草案；
- 判断结果不得进入其他租户的任何处理。

## 11. 输入快照与评估权责

信号触发判断默认复用信号的固定输入与特征；更新数据或研究后重评生成新版本，禁止静默替换旧输入。`input_mode`、差异解释、决策/审批刷新约束以 [策略 DSL 的判断输入与版本解释权](../11-policy-dsl.md#9-判断输入与版本解释权) 为准。

assessment 拥有判断框架/评分模型的专业指标、回归执行与启用门禁；learning 提供授权范围内的反馈数据集、编排评估及汇总效果，不替代 assessment 的发布决定。
