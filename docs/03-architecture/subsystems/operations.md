# 运营工作台子系统（operations）

> 状态：评审中 · 版本：1.0 · 更新：2026-10-12 · 平面：平台

## 1. 使命

为妙洽内部团队（数据运营、研究分析师、客户成功、领域专家）提供结构化的工作台，使人工工作可调度、可度量、可追溯，并逐步沉淀为自动化能力。

## 2. 职责

- **人工复核队列**：解析异常、低置信度实体解析与项目聚合、质量告警、研究报告复核；
- **数据运营**：来源健康处理、来源上线与权利审查流程、解析规则维护任务；
- **托管研究服务**：为签约托管服务的租户执行研究、调优与简报，工作只在租户管理员授予的服务委托（[identity 第 7.8 节](identity.md#78-服务委托)）范围内进行；
- **客户成功**：实施进度、配置检查、健康度、续费风险；
- **工单与工时**：所有人工工作以工单记录，含耗时、操作对象、结果；
- **自动化候选**：统计高频重复人工操作，生成自动化建议；
- **链路探针**：发起不含业务数据的探针，经 `acquisition`、`evidence` 回到本子系统，端到端验证事件骨干、追踪、审计与授权，并作为事件链路的健康监测（需 `platform:miaoqia` 上的 `can_trigger_probe` 关系）。

M0探针的运行时验收见[完整链路](../../05-delivery/05-m0-runtime-acceptance.md)：HTTPS网关与mTLS授权后，记录/请求事件/审计同事务提交；审计失败回滚或返回运行错误。完成消费者先校验离线载荷Schema，再核对来源、subject与规定hops路径；重复完成不重复记账。

## 3. 拥有的数据

`ReviewTask`、`ReviewDecision`、`ServiceTicket`、`ServiceTimeLog`、`CustomerHealth`、`AutomationCandidate`。

## 4. 接口与依赖

本子系统是内部人员的业务工作台，作为内部客户端调用其他子系统的命令接口执行操作（如 `evidence.SubmitManualCorrection`、`entity_graph.MergeEntities`、`domain_intelligence.ConfirmAggregation`），不直接写其他子系统的数据。访问租户数据必须持有 `identity` 颁发的授权：托管服务用服务委托，排障用内部支持访问；签约本身不授予访问。

## 5. 事件

**消费**：`evidence.probe.forwarded.v1`、`evidence.quality.flagged.v1`、`acquisition.source.health_changed.v1`、`learning.drift.detected.v1`、各子系统的“需人工复核”事件。

**发布**：`operations.probe.requested.v1`、`operations.review.completed.v1`、`operations.service_time.logged.v1`。

## 6. 设计要点

- 复核决定回写到对应子系统后，同时成为评估与训练样本；
- 托管服务的每次操作都关联租户、工单和服务委托，审计记录实际操作者、代表的租户与授权依据，可向客户展示服务记录；委托终止后工单不能继续执行租户内操作；
- 工时数据进入 `commerce` 用于服务计价与毛利核算。
