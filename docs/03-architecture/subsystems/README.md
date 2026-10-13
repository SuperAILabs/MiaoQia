# 子系统规格

> 状态：评审中 · 版本：1.0 · 更新：2026-10-12 · 负责人：首席架构师

每个子系统是一个拥有独立职责、数据所有权和接口契约的业务能力单元，也是将来独立部署的最小粒度。

## 1. 索引

| 平面 | 子系统 | 代码包 | 数据库 | 规格 |
|---|---|---|---|---|
| 数据供应 | 来源采集 | `miaoqia.acquisition` | `acq` | [acquisition.md](acquisition.md) |
| 数据供应 | 证据与内容理解 | `miaoqia.evidence` | `evd` | [evidence.md](evidence.md) |
| 数据供应 | 领域情报 | `miaoqia.domain_intelligence` | `dom` | [domain-intelligence.md](domain-intelligence.md) |
| 数据供应 | 实体与关系图谱 | `miaoqia.entity_graph` | `ent` | [entity-graph.md](entity-graph.md) |
| 数据供应 | 检索与索引 | `miaoqia.search` | （OpenSearch） | [search.md](search.md) |
| 客户智能 | 客户上下文与策略 | `miaoqia.customer_context` | `ctx` | [customer-context.md](customer-context.md) |
| 客户智能 | 监测与匹配 | `miaoqia.detection` | `det` | [detection.md](detection.md) |
| 客户智能 | Agent 研究 | `miaoqia.research` | `res` | [research.md](research.md) |
| 客户智能 | 判断与决策 | `miaoqia.assessment` | `asm` | [assessment.md](assessment.md) |
| 客户智能 | 推进与结果 | `miaoqia.pursuit` | `pur` | [pursuit.md](pursuit.md) |
| 客户智能 | 反馈学习与评估 | `miaoqia.learning` | `lrn` | [learning.md](learning.md) |
| 交互 | 接入网关 | `miaoqia.gateway` | `gwy` | [gateway.md](gateway.md) |
| 交互 | 渠道与会话 | `miaoqia.channels` | `chn` | [channels.md](channels.md) |
| 平台 | 身份与权限 | `miaoqia.identity` | `iam` | [identity.md](identity.md) |
| 平台 | 订阅计量计费 | `miaoqia.commerce` | `com` | [commerce.md](commerce.md) |
| 平台 | 审计与合规 | `miaoqia.governance` | `gov` | [governance.md](governance.md) |
| 平台 | AI 平台 | `miaoqia.ai_platform` | `aip` | [ai-platform.md](ai-platform.md) |
| 平台 | 运营工作台 | `miaoqia.operations` | `ops` | [operations.md](operations.md) |

“数据库”列是该子系统独占的逻辑数据库（首期为同一 PostgreSQL 集群中的独立数据库与独立账号），见 [数据架构](../02-data-architecture.md)。

## 2. 规格模板

每份子系统规格使用相同结构：

1. **使命**：一句话说明子系统存在的理由；
2. **职责与非职责**：明确边界，避免职责蔓延；
3. **内部组件**：子系统内部的模块划分；
4. **拥有的数据**：唯一可写的数据及其所属域（公共/租户）；
5. **接口**：对外命令与查询（契约名，详细定义在 `contracts/`）；
6. **事件**：发布与消费的事件；
7. **扩展点**：扩展包可以插入的位置；
8. **伸缩与可靠性**：负载特征、伸缩方式、故障与降级；
9. **安全要点**：子系统特有的安全与合规要求；
10. **开放问题**：尚待决定的设计点。

## 3. 数据所有权总表

任何数据只能由下表所列子系统写入：

| 数据 | 所有者 | 其他子系统获取方式 |
|---|---|---|
| 来源、来源权利、抓取记录、原始制品 | `acquisition` | 接口查询、事件 |
| 快照、文档、解析修订、证据 | `evidence` | 接口查询、事件 |
| 领域事实、领域事件 | `domain-intelligence` | 接口查询、事件、检索投影 |
| 实体、关系、合并历史 | `entity-graph` | 接口查询、图查询、事件 |
| 检索索引 | `search`（派生数据，可重建） | 检索接口 |
| 客户档案、目标、策略、知识资产 | `customer-context` | 接口查询、事件 |
| 监测订阅、信号 | `detection` | 接口查询、事件 |
| 研究案例与报告 | `research` | 接口查询、事件 |
| 判断、决策 | `assessment` | 接口查询、事件 |
| 推进、任务、结果 | `pursuit` | 接口查询、事件 |
| 反馈、评估集、仿真结果 | `learning` | 接口查询、事件 |
| 通知、投递、偏好 | `channels` | 接口查询 |
| 租户、成员、角色、授权、审批 | `identity` | 接口查询、事件、令牌声明 |
| 订阅、权益、用量、账单 | `commerce` | 接口查询、事件 |
| 审计事件、分级目录、保留策略 | `governance` | 接口查询 |
| 模型目录、提示版本、模型调用记录 | `ai-platform` | 接口查询 |
| 复核任务、服务工单 | `operations` | 接口查询、事件 |
