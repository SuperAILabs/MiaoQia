# 证据与内容理解子系统（evidence）

> 状态：评审中 · 版本：3.0 · 更新：2026-10-14 · 平面：数据供应

## 1. 使命

把原始制品转化为不可变、可定位、可复核的证据和规范化内容，并通过规则与 AI 增强（抽取、标签、分类、向量化）持续提升内容的可检索性；管理来源版本、解析版本与增强版本，使平台中每一个结构化事实都能回到原文。

## 2. 职责与非职责

**职责**

- 快照判定：对同一来源对象，判断原始制品是否构成新的来源快照（规范化内容哈希）；
- 文档处理：HTML 清洗与正文定位、PDF 文本与版面提取、Office 文档解析、图片与扫描件 OCR、压缩包与附件拆分、表格识别；
- 规范化文档：统一的文档表示（段落、表格、页码、坐标、分块），作为证据定位的坐标系，存放于对象存储（数据分层 L1）；
- 解析编排：调用信息领域包的解析器，生成解析修订；
- 字段溯源：每个解析字段的证据位置、方法、置信度；
- 质量评估：完整性、一致性、可疑值检测；
- 内容增强：运行增强器（文档分类、品类分类、标的抽取、实体提及、业务标签、摘要、分块向量），产出版本化的增强结果（数据分层 L3）；
- 语料快照：按月生成 Parquet 语料快照，供批量重算增强、评估与模型调优；
- 重放：解析器或增强器升级后对历史数据批量重新处理。

**非职责**

- 不定义领域语义和生命周期（属于 `domain-intelligence`）；
- 不做实体解析（属于 `entity-graph`）；
- 不修改原始制品或已发布的快照。

M0探针消费者先验证acquisition载荷，再核对来源、subject和前序hops；真实跨语言链路及非法载荷死信见[运行时验收](../../05-delivery/05-m0-runtime-acceptance.md)。其他文档/抽取事件的生产实现另行验收。

## 3. 内部组件

| 组件 | 说明 |
|---|---|
| `snapshotter` | 规范化与快照判定 |
| `document_pipeline` | 文档处理流水线：格式识别 → 转换 → 版面分析 → OCR → 规范化文档 |
| `extraction_orchestrator` | 选择领域包解析器、执行、生成解析修订 |
| `provenance` | 证据定位器（文档、页、段、字符区间、表格单元）与字段溯源 |
| `quality` | 质量规则执行与质量状态 |
| `enrichment` | 增强管线：增强器注册、成本分级执行（规则 → 小模型 → 大模型）、按内容哈希去重、结果写入对象存储、运行登记 |
| `corpus` | 语料快照生成与管理 |
| `replay` | 解析器与增强器升级后的批量重放（Temporal 工作流，热数据优先） |

## 4. 拥有的数据（公共域；客户授权文档在租户域）

| 实体 | 说明 |
|---|---|
| `SourceSnapshot` | 来源对象的不可变版本：来源对象键、序号、规范化哈希、原始制品引用、观察时间 |
| `NormalizedDocument` | 规范化文档与分块：正文存对象存储（`normalized/…`），数据库只存元数据与对象键；结构见 `contracts/schemas/evidence/normalized-document.schema.json` |
| `ExtractionRevision` | 一次解析：快照、解析器标识与版本、结果、质量、创建时间 |
| `FieldEvidence` | 字段 → 证据定位、方法（规则/模型/人工）、置信度 |
| `EvidenceRef` | 可被引用的证据片段（`ev_` 前缀），包含定位器与快照版本；结构见 `contracts/schemas/evidence/evidence-ref.schema.json` |
| `QualityAssessment` | 质量状态与警告 |
| `Enricher` / `EnricherVersion` | 增强器定义、版本、标签体系版本、评估结果、状态（候选/生效/退役） |
| `EnrichmentRun` | 增强运行登记：增强器版本、数据范围、进度、成本、结果对象前缀 |
| `EnrichmentResult` | 增强结果（存对象存储 `enrichment/…`，不进入数据库） |
| `CorpusSnapshot` | 语料快照登记 |

正文、附件文本、分块、增强结果和向量都**不**存入 PostgreSQL，见 [数据架构](../02-data-architecture.md#3-公共域的分层持久化)。

## 5. 接口

| 接口 | 类型 | 说明 |
|---|---|---|
| `GetSnapshot` / `ListSnapshots` | 查询 | 来源对象的版本历史 |
| `GetExtraction` | 查询 | 指定快照的当前或指定解析修订 |
| `ResolveEvidence` | 查询 | 证据引用 → 原文片段、上下文、可视化定位（供界面高亮） |
| `GetDocument` | 查询 | 规范化文档与签名访问地址 |
| `RequestReextraction` | 命令 | 对指定范围用指定解析器版本重放 |
| `SubmitManualCorrection` | 命令 | 运营人员的人工修正：生成新的解析修订（方法=人工），不改变旧修订 |
| `GetEnrichment` | 查询 | 指定快照或分块在指定增强器版本下的增强结果 |
| `RunEnrichment` | 命令 | 对指定范围运行指定增强器版本（异步、受成本预算约束） |
| `PromoteEnricherVersion` | 命令 | 经评估通过后将增强器版本设为生效（触发索引重建） |

## 6. 事件

载荷唯一定义见[`contracts/events/evidence/events.asyncapi.yaml`](../../../contracts/events/evidence/events.asyncapi.yaml)。复用公共事件原语，稳定制品引用不含签名URL或凭据；事件正反例及引用兼容检查通过不代表生产发布/消费、事务发件箱或重放已实现。

**发布**：`evidence.snapshot.created.v1`、`evidence.extraction.revised.v1`（载荷含快照、修订、领域包标识、质量状态）、`evidence.quality.flagged.v1`、`evidence.artifact.refetch_requested.v1`、`evidence.enrichment.completed.v1`（单个快照的增强完成）、`evidence.enricher.promoted.v1`（增强器新版本生效）。

**公共实例消费**：`acquisition.fetch.completed.v1`、`acquisition.source.rights_changed.v1`（更新公共证据的可见性标签）。本节发布清单属于公共实例；租户文档的命令/事件契约和消费者随知识资产管线实现，不能套用公共主题。

## 7. 扩展点

- **解析器 SPI**（`Extractor`）：信息领域包提供，输入规范化文档，输出领域字段与字段溯源；
- **文档处理插件**：新格式或专用 OCR 模型；
- **质量规则**：领域包提供；
- **增强器 SPI**（`Enricher`）：信息领域包提供分类器与抽取器（代码）；行业包提供标签体系与标签规则（声明）。

## 8. 伸缩与可靠性

- 文档处理为 CPU 密集型（OCR 与版面经模型 API，ADR-0015），独立进程角色并按队列深度伸缩；
- 解析器执行带超时与资源限制，单个文档失败不阻塞流水线，进入复核队列；
- 重放为后台低优先级工作流，限速以保护在线处理；
- 所有输出都可从原始制品重新生成，因此本子系统数据可在灾难时重建（成本高，仍需备份）。

## 9. 安全要点

- 文档处理在沙箱中运行，禁用宏、脚本与外部链接加载；
- 对压缩包设置解压大小与层级上限，防止解压炸弹；
- 文档中的文本是不可信输入，传给模型的内容带不可信标记，见 [AI 与 Agent](../05-ai-and-agent-architecture.md)；
- 证据可见性继承来源权利标签。

## 10. 开放问题

1. OCR与版面API供应商按M1真实文档评测选定，前期不建设私有推理。

SDK语义验证覆盖快照/管线归属、块/表格坐标与quote_hash；人工规范化扫描样本标记方法与范围，不充当生产OCR成绩。

规范化文档的格式已确定，唯一定义为 `contracts/schemas/evidence/normalized-document.schema.json`；解析器与证据引用以该坐标系工作，不另行选择平行格式。

## 11. 数据域实例与领域校验

公共实例只处理公共来源；租户实例随 Cell 部署，只处理客户授权文档，不发布公共 evidence 事件，不共享公共缓存、语料、凭证或重放范围。租户知识资产的业务所有权属于 customer-context，evidence 租户实例拥有对应处理制品与修订；两者通过接口/事件协作。

通用 SDK 校验规范化文档、定位、哈希及证据引用完整性；字段语义和领域一致性由信息领域包 `semantic_validator` 提供，扩展注册属于本子系统的 extensions 层。当前接入是 corpus 语料校验路径，不表示生产文档管线已执行这些规则。evidence 负责增强器专业指标、评估执行与版本生效门禁；公共语料与公共评估快照由本子系统维护，不回查租户 learning 数据。
