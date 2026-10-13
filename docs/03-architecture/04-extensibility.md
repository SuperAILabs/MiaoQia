# 可扩展性设计

> 状态：评审中 · 版本：1.0 · 更新：2026-10-12 · 负责人：首席架构师

## 1. 设计目标

妙洽要服务多个信息领域、多个应用领域、多个行业和多种商业目标。可扩展性设计的目标是：**新增这些内容时不修改平台核心代码，不改变子系统边界，不破坏已有租户**。

扩展机制与 [业务层级](../02-business/01-business-architecture.md) 一一对应：

| 业务层级 | 扩展包 | 主要插入的子系统 |
|---|---|---|
| 信息领域 | 信息领域包（Information Domain Pack） | `acquisition`、`evidence`、`domain-intelligence`、`entity-graph`、`search`、`research`（工具） |
| 应用领域 + 商业目标 + 机会路径 | 应用领域包（Application Domain Pack） | `customer-context`、`detection`、`research`、`assessment`、`pursuit`、`learning`、`channels`、`gateway`、Web |
| 行业（客户细分维度） | 行业包（Industry Pack） | `customer-context`、`entity-graph`、`search`、`detection` |

此外，平台在技术层面提供若干独立 SPI：连接器、渠道适配器、模型供应商、外部系统同步。

## 2. 扩展包结构

### 2.1 通用 manifest

```yaml
pack:
  id: tendering-procurement          # 全局唯一
  kind: information_domain           # information_domain | application_domain | industry
  version: 1.4.0                     # 语义化版本
  platform_api: ">=1.0 <2.0"         # 兼容的平台扩展 API 版本
  requires:
    - entity-core@^1.0
  owners: [tendering-domain-team]
  provides: { … }                    # 按 kind 列出提供的扩展点
```

扩展包以 Python 包分发（代码部分）+ 声明式定义文件（YAML/JSON，受 `contracts/packs/` Schema 校验）。声明式优先：能用声明表达的不写代码。

### 2.2 信息领域包

| 提供项 | 插入点 | 形式 |
|---|---|---|
| 来源定义 | `acquisition.source_registry` | 声明 |
| 来源处理器 | `acquisition` `SourceHandler` SPI | 代码 |
| 解析器 | `evidence` `Extractor` SPI | 代码（可调用模型网关） |
| 质量规则 | `evidence.quality` | 声明 + 代码 |
| 增强器与标签体系 | `evidence.enrichment` `Enricher` SPI | 声明（标签体系、规则）+ 代码（分类器、抽取器） |
| 领域 Schema 与迁移 | `domain-intelligence.fact_store` | JSON Schema + 迁移脚本 |
| 聚合规则 | `domain-intelligence.aggregator` | 声明 + 代码 |
| 生命周期状态机 | `domain-intelligence.lifecycle_engine` | 声明 |
| 变化检测与领域事件 | `domain-intelligence.change_detector` | 声明 + 事件 Schema |
| 实体提及与关系抽取 | `entity-graph` | 声明 + 代码 |
| 检索映射 | `search.mappings` | 声明 |
| 统计与查询模板 | `domain-intelligence` | 声明（SQL 模板受审查） |
| 研究工具 | `research.tool_registry` | 声明（基于平台接口组合） |
| 个人信息清单 | `governance` | 声明 |

### 2.3 应用领域包

| 提供项 | 插入点 |
|---|---|
| 客户细分模板（价值链角色、默认目标） | `customer-context` |
| 商业目标实例 | `customer-context.objectives` |
| 策略模板与可用字段、函数 | `customer-context.policy_engine` |
| 特征函数、处置规则、信号合并规则 | `detection.matcher` |
| 研究模板与类型化产物 Schema | `research.template_registry` |
| 判断框架 | `assessment.framework_registry` |
| 机会路径与衔接规则 | `pursuit.playbook_registry` |
| 反馈原因标签、指标、基线评估集 | `learning` |
| 通知卡片模板 | `channels` |
| API 视图、MCP 工具、Skills | `gateway` |
| Web 视图描述 | Web 前端（视图注册表） |

### 2.4 行业包

| 提供项 | 插入点 |
|---|---|
| 品类分类与跨分类映射 | `entity-graph`（`Category`） |
| 产品、规格、品牌词表与同义词 | `search`、`entity-graph` |
| 行业标签体系与品类分类增强器 | `evidence.enrichment` |
| 能力与资质模板 | `customer-context.profile` |
| 默认排除与关注规则 | `customer-context.policy_engine` |

## 3. SPI 规范

所有代码型 SPI 遵循统一约束：

1. **接口稳定**：SPI 定义在 `miaoqia.sdk` 中，按平台扩展 API 版本管理；
2. **无副作用访问**：扩展代码只能通过 SPI 传入的上下文对象访问平台能力（例如解析器只能读取规范化文档、调用模型网关），不能直接访问数据库、网络或文件系统；
3. **确定性优先**：匹配特征函数、聚合规则必须是确定性纯函数；
4. **资源限制**：每次调用有超时与内存上限；
5. **可观测**：调用自动带追踪与指标；
6. **可测试**：每个扩展包必须附带测试夹具与评估样本，持续集成中运行。

主要 SPI：

| SPI | 子系统 | 方法要点 |
|---|---|---|
| `SourceHandler` | acquisition | `discover(plan) → refs`、`fetch(ref) → artifacts` |
| `Connector` | acquisition | 新访问方式（授权 API、文件投递、客户系统） |
| `Extractor` | evidence | `extract(document, ctx) → fields + provenance` |
| `Enricher` | evidence | `enrich(document_or_chunks, ctx) → tags/classes/mentions/vectors + confidence`；声明成本等级与适用范围 |
| `Aggregator` | domain-intelligence | `candidates(fact) → keys`、`decide(fact, candidate) → link` |
| `RelationExtractor` | entity-graph | `extract(fact) → mentions + relations` |
| `FeatureFunction` | detection | `compute(fact_view, context_view) → value` |
| `ToolAdapter` | research | 把平台接口组合为受控工具 |
| `ChannelAdapter` | channels | `capabilities()`、`send(rendered) → delivery`、`receive(callback) → inbound_message \| action`、`verify(callback)`、`resolve_identity(user)` |
| `SyncAdapter` | pursuit | 与 CRM/项目管理系统同步 |
| `ModelProvider` | ai-platform | 新模型供应商接入 |

## 4. 加载、版本与发布

- **注册**：扩展包在部署时由各子系统的注册表加载，注册表记录包版本与定义版本；
- **多版本共存**：机会路径、判断框架、研究模板、解析器等定义在运行时可多版本共存；已运行的推进、研究与判断固定其使用的版本；
- **启用范围**：应用领域包与行业包的启用以租户权益控制（`commerce`），可灰度到指定租户；
- **兼容检查**：持续集成验证扩展包与平台扩展 API 的兼容性、Schema 有效性、评估集表现；
- **回滚**：定义版本可回退；解析器回退产生新的解析修订而不是删除旧修订。

## 5. 新增内容的操作清单

### 5.1 新增一个信息领域

1. 完成来源与权利评估，登记来源权利；
2. 定义领域 Schema、生命周期、事件、实体关系、个人信息清单；
3. 实现来源处理器与解析器，提供评估样本；
4. 定义检索映射、统计模板与研究工具；
5. 达到该信息领域的覆盖与质量门槛（见 [质量与验收](../05-delivery/02-quality-and-acceptance.md)）后对应用领域开放。

### 5.2 新增一个应用领域

1. 定义客户细分与适用的商业目标；
2. 确认所需信息领域已就绪；
3. 编写机会路径、研究模板、判断框架、策略模板、反馈标签、指标与评估集；
4. 定义视图、卡片、API 与 MCP 工具；
5. 按 [应用领域准入标准](../02-business/04-application-domains.md#3-应用领域的准入标准) 发布。

### 5.3 新增一个行业

1. 定义品类分类与映射、词表、能力与资质模板、默认规则；
2. 在至少一个应用领域中验证匹配质量。

### 5.4 新增一个商业目标类型

商业目标类型属于平台层，新增需经产品与架构评审，更新 [商业目标目录](../02-business/02-commercial-objectives.md)；应用领域包随后实例化。

## 6. 不允许的扩展方式

- 在平台核心代码中写入特定信息领域、行业或客户的分支逻辑；
- 扩展包直接访问其他子系统数据库；
- 为某个领域新建一套平行的证据、实体、权限或通知体系；
- 通过“通用超级表”或无约束 JSON 字段绕过领域 Schema。
