# AI 与 Agent 架构

> 状态：评审中 · 版本：1.0 · 更新：2026-10-12 · 负责人：AI 负责人

## 1. AI 在平台中的位置

AI 是贯穿多个子系统的能力，但始终受治理：所有模型调用经 [`ai-platform`](subsystems/ai-platform.md) 的模型网关；所有 Agent 行为经 [`research`](subsystems/research.md) 的受控工具。

| 场景 | 子系统 | AI 的作用 | 失败时的后果控制 |
|---|---|---|---|
| 文档版面与 OCR | evidence | 从扫描件、复杂 PDF 获取文本与结构 | 低置信度进入复核；原文始终可查 |
| 字段解析 | evidence | 规则无法覆盖的字段提取 | 带方法与置信度的字段溯源；规则优先 |
| 内容增强 | evidence | 文档与品类分类、业务标签、标的抽取、实体提及、摘要、分块向量，提升检索召回与精准度 | 只作检索增强，带置信度；版本化可回退；采纳为领域事实需通过质量评估 |
| 实体解析辅助 | entity-graph | 名称相似度、候选排序 | 自动合并阈值保守，可撤销 |
| 嵌入与检索 | search | 语义检索、重排 | 只影响召回排序，不影响事实 |
| 信号相关性 | detection / assessment | 语义相关性特征 | 作为特征之一，与硬门槛分离记录 |
| 研究 | research | 规划、工具选择、交叉验证、归纳 | 引用校验；事实/推断/未知项分离 |
| 判断归纳 | assessment | 汇总判断理由与缺口 | 只生成草案，人批准 |
| 策略调优 | customer-context | 自然语言 → 结构化策略差异 | 只生成草案，经仿真与审批 |
| 摘要与简报 | channels | 生成摘要 | 标注 AI 生成，附证据链接 |
| 起草辅助 | pursuit（后续） | 基于已批准知识资产起草材料 | 只使用已批准资产，人审定 |

## 2. 研究模板

研究模板把开放式 Agent 约束为有边界的研究流程。模板由应用领域包以声明式定义：

```yaml
template:
  id: RT-TENDER-WINNER
  version: 2.1.0
  question: "中标方画像：规模、历史项目、常用供应商与信用风险"
  subject_types: [organization, procurement_project]
  inputs:
    - name: winner
      type: organization_ref
  tools:                         # 允许的工具白名单
    - entity.get_profile
    - entity.get_relationships
    - domain.query_facts:tender_awards_by_org
    - domain.statistics:award_summary_by_org
    - credit.query_records       # 来自 INF-CRED 信息领域包
  steps:                         # 引导性步骤，Agent 可在白名单内调整顺序
    - 识别组织并核对强标识
    - 汇总近三年中标项目与金额
    - 识别关联的总包/分包/供应商关系
    - 查询司法信用与经营异常
    - 指出信息缺口
  budget:
    max_tool_calls: 30
    max_model_cost: 3.0          # 以平台成本单位计
    timeout: 10m
  model_profile: research.standard
  output_schema: contracts/schemas/research/winner_profile.v2.json
  report_rules:
    every_fact_requires_evidence: true
    allow_inference: true
    require_unknowns_section: true
```

研究运行时在 Temporal 工作流中执行：每次模型调用、每次工具调用都是可重试、可记录的活动；运行可从检查点恢复。

## 3. 受控工具

| 属性 | 说明 |
|---|---|
| 名称与版本 | 如 `entity.get_relationships@1` |
| 输入输出 Schema | 严格白名单；输出包含证据引用 |
| 权限 | 所需作用域；按研究发起者的身份检查 |
| 数据分级 | 输出数据的最高分级，决定后续可用的模型路由 |
| 成本 | 计入研究预算 |
| 副作用 | 研究工具对租户数据与领域事实只读。唯一例外是**数据获取请求**（如 `acquisition.request_coverage`）：请求对公共来源补采或回填历史数据，由 `acquisition` 按预算与来源权利异步执行，不直接写入任何事实 |

工具由 `ToolAdapter` 把子系统公开查询接口组合而成，不直接访问数据库。工具执行时的租户与主体来自研究案例的发起者，Agent 生成的参数不能改变它们。

## 4. 研究报告结构

```json
{
  "schema_version": "research-report.v1",
  "research_case_id": "rsc_…",
  "template": {"id": "RT-TENDER-WINNER", "version": "2.1.0"},
  "subject_ref": {"type": "organization", "id": "org_…", "version": 7},
  "pinned": {
    "context_snapshot_id": "ctxs_…",
    "model_versions": ["…"],
    "prompt_versions": ["…"],
    "tool_versions": ["…"],
    "data_as_of": "2026-10-12T08:00:00Z"
  },
  "facts":           [{"statement": "…", "evidence_refs": ["ev_…"], "confidence": "high"}],
  "inferences":      [{"statement": "…", "based_on": ["fact#1", "fact#3"], "confidence": "medium"}],
  "unknowns":        [{"question": "…", "why_unknown": "…", "how_to_resolve": "…"}],
  "recommendations": [{"action": "…", "rationale": "…"}],
  "typed_output":    { },
  "summary": "…",
  "ai_generated": true
}
```

引用校验器在报告生成后执行：每条 `facts` 必须能解析到证据且与证据内容一致（规则 + 模型交叉检查），不通过的条目降级为推断或移除并记录原因。

## 5. 不可信内容与提示注入防护

- 所有外部文档、网页、附件和客户上传文档视为**不可信数据**；
- 进入模型的不可信内容以结构化字段传递并明确标注，与系统指令严格分离；
- 模型输出的工具调用只能在模板白名单内，参数经 Schema 校验；
- 不可信内容不能触发新工具、扩大权限、改变租户或预算；
- 输出经 Schema 校验与敏感信息检测后才写入；
- 持续集成中维护提示注入测试集（包括嵌在公告附件中的指令）。

## 6. 人机协作

| 机制 | 用途 |
|---|---|
| 澄清请求 | Agent 在信息不足时暂停研究，向用户提问（`research.case.awaiting_input`） |
| 复核 | 运营或客户人员复核研究报告，复核结果进入评估集 |
| 审批 | 判断与决策、策略变化、对外动作由有权限的人批准 |
| 反馈 | 用户对报告每一条的认可与否决，带原因 |

## 7. 评估体系

| 层级 | 评估对象 | 方法 |
|---|---|---|
| 组件 | 解析器、实体解析、嵌入、重排 | 标注集上的准确率、召回率 |
| 研究模板 | 报告的事实正确率、引用覆盖、未知项识别、成本、耗时 | 黄金集 + 人工评分 + 模型辅助评分 |
| 判断框架 | 处置与判断与人工结论的一致性 | 混淆矩阵、否决率 |
| 端到端 | 商业目标的结果指标 | 见 [质量与验收](../05-delivery/02-quality-and-acceptance.md) |

任何模型、提示、模板或框架的变更必须先在 `learning` 中完成回归评估，指标不劣化才能发布。

## 8. 成本控制

- 研究按模板设置预算，按租户与套餐设置并发与月度上限；
- 公共域的解析与嵌入结果复用于所有租户，只计算一次；
- 模型路由按任务选择性价比合适的模型；
- 每租户、每模板、每模型的成本在 `commerce` 中可见。

## 9. 合规

- 默认只使用境内处理且已完成备案或登记的模型服务 （合规已由法务确认，具体模型清单在技术选型专题中确定）；
- 面向用户展示的 AI 生成内容按生成合成内容标识要求进行标识，并保留生成元数据；
- 模型备案/登记、内容安全、用户申诉的义务由 `governance` 登记与跟踪；
- 各数据分级可使用的模型见 [安全与租户](06-security-and-tenancy.md#5-数据分级与处理约束)。
