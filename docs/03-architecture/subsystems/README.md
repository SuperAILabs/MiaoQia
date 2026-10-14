# 子系统规格

> 状态：评审中 · 版本：1.1 · 更新：2026-10-13 · 负责人：首席架构师

每个子系统是一个拥有独立职责、数据所有权和接口契约的业务能力单元，也是将来独立部署的最小粒度。

## 1. 索引

本表是子系统的唯一登记处：平面、职责、语言（ADR-0006）、部署单元与数据库只在此定义，其他文档引用本表。子系统拥有的数据见第 3 节，进程角色见 [部署拓扑](../07-deployment-topology.md)。

| 平面 | 子系统 | 核心职责 | 语言 | 部署单元 | 数据库 |
|---|---|---|---|---|---|
| 数据供应 | [`acquisition`](acquisition.md) 来源采集 | 来源注册与权利、调度、抓取、原始归档、来源健康 | Python | `acquisition` | `acq` |
| 数据供应 | [`evidence`](evidence.md) 证据与内容理解 | 文档处理、快照、解析修订、字段溯源、质量、AI 内容增强 | Python | `data`（租户文档解析为单元级实例） | `evd` |
| 数据供应 | [`domain-intelligence`](domain-intelligence.md) 领域情报 | 领域事实与事件、生命周期、跨记录聚合、领域统计 | Python | `data` | `dom` |
| 数据供应 | [`entity-graph`](entity-graph.md) 实体与关系图谱 | 实体主数据、实体解析、关系图谱 | Python | `data` | `ent` |
| 数据供应 | [`search`](search.md) 检索与索引 | 索引投影、全文/语义/混合检索、订阅反查、租户知识检索 | Go | `platform` | （OpenSearch） |
| 客户智能 | [`customer-context`](customer-context.md) 客户上下文与策略 | 客户档案、目标、策略、知识资产 | Go | `platform` | `ctx` |
| 客户智能 | [`detection`](detection.md) 监测与匹配 | 监测订阅、信号匹配、处置分层 | Go | `platform` | `det` |
| 客户智能 | [`research`](research.md) Agent 研究 | 研究模板执行、Agent 运行、研究报告 | Go | `platform` | `res` |
| 客户智能 | [`assessment`](assessment.md) 判断与决策 | 判断框架执行、评估、决策与审批 | Go | `platform` | `asm` |
| 客户智能 | [`pursuit`](pursuit.md) 推进与结果 | 机会路径实例、阶段、任务、结果 | Go | `platform` | `pur` |
| 客户智能 | [`learning`](learning.md) 反馈学习与评估 | 反馈、评估集、策略仿真、质量分析 | Go | `platform` | `lrn` |
| 交互 | [`gateway`](gateway.md) 接入网关 | REST/OpenAPI、BFF、MCP、Webhook、限流 | Go | `platform` | `gwy` |
| 交互 | [`channels`](channels.md) 渠道与会话 | 多渠道双向接入：通知、渠道内对话、快捷动作、身份绑定 | Go | `platform` | `chn` |
| 平台 | [`identity`](identity.md) 身份与权限 | 租户、成员、角色、授权决策、审批 | Go | `platform` | `iam` |
| 平台 | [`commerce`](commerce.md) 订阅计量计费 | 套餐、权益、配额、计量、账单 | Go | `platform` | `com` |
| 平台 | [`governance`](governance.md) 审计与合规 | 审计、数据分级、保留删除、合规登记 | Go | `platform` | `gov` |
| 平台 | [`ai-platform`](ai-platform.md) AI 平台 | 模型网关、提示注册、嵌入、护栏、AI 计量 | Go | `platform` | `aip` |
| 平台 | [`operations`](operations.md) 运营工作台 | 人工复核队列、数据运营、托管服务、客户成功 | Go | `platform` | `ops` |

“数据库”列是该子系统独占的逻辑数据库（首期为同一 PostgreSQL 集群中的独立数据库与独立账号），见 [数据架构](../02-data-architecture.md)。代码包命名见 [代码库结构](../09-codebase-structure.md)。

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
