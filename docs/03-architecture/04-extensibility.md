# 可扩展性设计

> 状态：评审中 · 版本：2.0 · 更新：2026-10-13 · 负责人：首席架构师

## 1. 设计目标

妙洽要服务多个信息领域、多个应用领域、多个行业和多种商业目标。可扩展性设计的目标是：**新增这些内容时不修改平台核心代码，不改变子系统边界，不破坏已有租户**。

扩展机制与 [业务层级](../02-business/01-business-architecture.md) 一一对应：

| 业务层级 | 扩展包 | 主要插入的子系统 |
|---|---|---|
| 信息领域 | 信息领域包（Information Domain Pack） | `acquisition`、`evidence`、`domain-intelligence`、`entity-graph`、`search`、`research`（工具） |
| 应用领域 + 商业目标 + 机会路径 | 应用领域包（Application Domain Pack） | `customer-context`、`detection`、`research`、`assessment`、`pursuit`、`learning`、`channels`、`gateway`、Web |
| 行业（客户细分维度） | 行业包（Industry Pack） | `customer-context`、`entity-graph`、`search`、`detection` |

此外，平台在技术层面还有若干内部适配器接口（渠道适配器、模型供应商、外部系统同步、受控工具适配），由平台团队实现，不对扩展包开放。

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

扩展包由声明式定义文件（YAML/JSON）组成。清单与定义文件没有独立的 manifest Schema，由 `mqc check` 的扩展包一致性规则（`tools/contracts/src/mqc/packs.py`，P0–P7）对照 `contracts/` 校验：信息领域包检查 `pack.yaml` 引用的 Schema、事件文件与领域校验入口存在且合规，生命周期状态与触发值取自所引用 Schema 的枚举；应用领域包的策略模板按 `contracts/policy/policy.schema.json` 校验，并检查视图来源、引用声明、规则编号、事件类型与样例处置。行业包尚未纳入检查。扩展包按种类允许的形式不同（ADR-0006）：

| 种类 | 形式 | 运行位置 |
|---|---|---|
| 信息领域包 | 声明 + Python 代码（实现第 3 节的代码型 SPI），以 Python 包分发 | Python 子系统 |
| 应用领域包 | 纯声明 + CEL 表达式 | Go 子系统 |
| 行业包 | 纯声明 | 按插入点加载 |

声明式优先：能用声明表达的不写代码。

### 2.2 信息领域包

| 提供项 | 插入点 | 形式 |
|---|---|---|
| 来源定义 | `acquisition.source_registry` | 声明 |
| 来源处理器 | `acquisition` `SourceHandler` SPI | 代码 |
| 解析器 | `evidence` `Extractor` SPI | 代码（可调用模型网关） |
| 质量规则 | `evidence.quality` | 声明 + 代码 |
| 增强器与标签体系 | `evidence.enrichment` `Enricher` SPI | 声明（标签体系、规则）+ 代码（分类器、抽取器） |
| 领域 Schema 与迁移 | `domain-intelligence.fact_store` | JSON Schema（存放于 `contracts/schemas/domains/<领域>/`，供三种语言生成代码）+ 迁移脚本 |
| 聚合规则 | `domain-intelligence.aggregator` | 声明 + 代码 |
| 生命周期状态机 | `domain-intelligence.lifecycle_engine` | 声明 |
| 变化检测与领域事件 | `domain-intelligence.change_detector` | 声明 + 事件 Schema |
| 实体提及与关系抽取 | `entity-graph` | 声明 + 代码 |
| 检索映射 | `search.mappings` | 声明 |
| 统计与查询模板 | `domain-intelligence` | 声明（SQL 模板受审查） |
| 研究工具 | `research.tool_registry` | 声明（基于平台接口组合） |
| 个人信息清单 | `governance` | 声明 |

### 2.3 应用领域包

应用领域包不含可执行代码：特征函数、处置规则、判断框架条件、机会路径衔接条件、策略模板中的条件均以 CEL 表达式声明，由 Go 子系统求值。

| 提供项 | 插入点 |
|---|---|
| 客户细分模板（价值链角色、默认目标） | `customer-context` |
| 商业目标实例 | `customer-context.objectives` |
| 策略模板与可用字段、函数 | `customer-context.policy_engine` |
| 特征函数、处置规则、信号合并规则 | `detection.matcher` |
| 研究模板与类型化产物 Schema | `research.template_registry` |
| 判断框架 | `assessment.framework_registry` |
| 机会路径与衔接规则 | `pursuit.playbook_registry` |
| 反馈原因标签、效果指标与基线引用 | `learning`；专业基线与发布门禁归对应能力所有者 |
| 通知卡片模板 | `channels` |
| API 视图、MCP 工具、Skills | `gateway` |
| Web 视图描述 | Web 前端（视图注册表） |

### 2.4 行业包

| 提供项 | 插入点 |
|---|---|
| 品类分类与跨分类映射 | `entity-graph`（`Category`） |
| 产品、规格、品牌词表与同义词 | `search`、`entity-graph` |
| 行业标签体系与品类分类规则（声明） | `evidence.enrichment` |
| 能力与资质模板 | `customer-context.profile` |
| 默认排除与关注规则 | `customer-context.policy_engine` |

## 3. 扩展点规范

### 3.1 代码型 SPI（信息领域包，Python）

所有代码型 SPI 遵循统一约束：

1. **接口稳定**：SPI 定义在 Python `sdk`（`data/sdk`，包 `miaoqia.sdk`）中，按平台扩展 API 版本管理；解析器只读取规范化文档，字段必须经 `ExtractionContext` 取得证据引用；
2. **无副作用访问**：扩展代码只能通过 SPI 传入的上下文对象访问平台能力（例如解析器只能读取规范化文档、调用模型网关），不能直接访问数据库、网络或文件系统；
3. **资源限制**：每次调用有超时与内存上限；
4. **可观测**：调用自动带追踪与指标；
5. **可测试**：每个扩展包必须附带测试夹具与评估样本，持续集成中运行。

| SPI | 子系统 | 方法要点 |
|---|---|---|
| `SourceHandler` | acquisition | `discover(plan) → refs`、`fetch(ref) → artifacts` |
| `Connector` | acquisition | 新访问方式（授权 API、文件投递、客户系统） |
| `Extractor` | evidence | `extract(document, ctx) → fields + provenance` |
| `Enricher` | evidence | `enrich(document_or_chunks, ctx) → tags/classes/mentions/vectors + confidence`；声明成本等级与适用范围 |
| `Aggregator` | domain-intelligence | `candidates(fact) → keys`、`decide(fact, candidate) → link` |
| `RelationExtractor` | entity-graph | `extract(fact) → mentions + relations` |

### 3.2 表达式扩展点（应用领域包与行业包，CEL）

| 扩展点 | 子系统 | 表达式内容 |
|---|---|---|
| 特征函数 | detection | 由领域事实视图与客户上下文视图计算特征值 |
| 处置与信号合并规则 | detection | 条件 → 处置 / 合并键 |
| 策略模板与条件 | customer-context | 策略模板、参数 Schema、视图声明与规则条件（见 [策略 DSL](11-policy-dsl.md)） |
| 判断框架条件 | assessment | 门槛、评分项、缺口判定 |
| 机会路径衔接条件 | pursuit | 阶段进入与路径衔接条件 |

CEL 是非图灵完备的表达式语言：求值确定、无副作用、可静态类型检查，并可设置成本上限，因此天然满足确定性与资源限制要求。可用变量与函数由 `contracts/policy/` 中的 CEL 环境声明定义；平台函数库的新增函数需经架构评审，扩展包不能注册任意代码。

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

## 7. 领域校验扩展

SDK 是平台通用能力（见代码库结构），扩展包依赖 SDK，SDK 不反向静态依赖领域包或包含其业务分支。信息领域包在 `pack.yaml` 的 `provides.semantic_validator` 登记 `模块:函数`，函数签名为 `validate(document, facts, evidence) -> list[str]`。语料组装工具按 Schema 所有权加载包的 src 入口，先校验通用证据完整性，再执行领域规则；登记错误、来源不属于登记包、签名不兼容或规则失败必须失败退出；entrypoints 共享模块先做不执行领域代码的静态登记校验，corpus 导入后再调用同一模块校验实际函数的来源、同步性与签名（不跟随装饰器的 __wrapped__）；装饰器与入口重绑定不能免除加载后校验。mqc 只证明静态登记合规，不代证导入后的实际函数合规。入口必须为同步函数并返回 list[str]，空列表才表示通过，None 不表示通过。领域 Schema 缺少所有者登记也必须失败。纯声明包只执行 Schema 校验，代码型包必须显式登记。采购/企业语义规则分别位于对应 packs，新增规则扩展既有校验器，不复制语料工具。

这是当前 corpus 校验路径；生产 evidence 的 extensions 注册与数据管线接入需另行实现并验收，不能把语料通过当作运行时执行证明。
