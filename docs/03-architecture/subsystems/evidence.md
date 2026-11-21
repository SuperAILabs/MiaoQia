# 证据与内容理解子系统（evidence）

> 状态：评审中 · 平面：数据供应

## 1. 使命

把原始制品转化为不可变、可定位、可复核的证据和规范化内容，并通过规则与 AI 增强（抽取、标签、分类）持续提升内容的可检索性；管理来源版本、解析版本与增强版本，使平台中每一个结构化事实都能回到原文。

## 2. 职责与非职责

**职责**

- 快照判定：对同一来源对象，判断原始制品是否构成新的来源快照（规范化内容哈希）；
- 文档处理：HTML 清洗与正文定位、PDF 文本与版面提取、Office 文档解析、图片与扫描件 OCR、压缩包与附件拆分、表格识别；
- 规范化文档：统一的文档表示（段落、表格、页码、坐标、分块），作为证据定位的坐标系，存放于对象存储（数据分层 L1）；
- 结构化记录集：接口响应、数据下载等结构化来源的统一记录表示（记录键 + 原样字段），作为结构化证据的坐标系，与规范化文档并列存放在 L1；
- 解析编排：调用信息领域包的解析器，生成解析修订；
- 字段溯源：每个解析字段的证据位置、方法、置信度；证据分文档、记录、派生三种形态（[ADR-0011](../../04-decisions/0011-immutable-evidence-dual-versioning.md#决策)），派生证据由 evidence 登记，领域事实投影器在验证后引用；
- 质量评估：完整性、一致性、可疑值检测；
- 内容增强：运行增强器（文档分类、品类分类、标的抽取、实体提及、业务标签、摘要），产出版本化的增强结果（数据分层 L3）；
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
| `record_pipeline` | 结构化处理流水线：字符集与格式解码 → 记录键生成 → 结构化记录集 |
| `extraction_orchestrator` | 选择领域包解析器、执行、生成解析修订 |
| `provenance` | 证据定位器（文档、页、段、字符区间、表格单元；记录键与字段路径）、派生证据登记与字段溯源 |
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
| `StructuredRecordSet` | 结构化记录集：同样存对象存储（`normalized/…`），数据库只存元数据与对象键；取值无损表示：数字保留来源十进制原文为字符串、不经浮点，来源 null 在对象中以省略字段表示、在数组中保留为 null 以保持元素位置，`value_hash` 按此表示计算；结构见 `contracts/schemas/evidence/structured-record.schema.json`（草案） |
| `EvidenceRef` | 可被引用的证据（`ev_` 前缀），三种形态：文档证据（定位器与快照版本，`evidence-ref.schema.json`）、记录证据（记录键与字段路径，`record-evidence-ref.schema.json`）、派生证据（转换版本、参考数据版本与输入证据，`derivation-evidence-ref.schema.json`）；记录证据为草案，派生证据仅启用本地测试转换 |
| `QualityAssessment` | 质量状态与警告 |
| `Enricher` / `EnricherVersion` | 增强器定义、版本、标签体系版本、评估结果、状态（候选/生效/退役） |
| `EnrichmentRun` | 增强运行登记：增强器版本、数据范围、进度、成本、结果对象前缀 |
| `EnrichmentResult` | 增强结果（存对象存储 `enrichment/…`，不进入数据库） |
| `CorpusSnapshot` | 语料快照登记 |

正文、附件文本、分块与增强结果都**不**存入 PostgreSQL，见 [数据架构](../02-data-architecture.md#3-公共域的分层持久化)。

## 5. 接口

| 接口 | 类型 | 说明 |
|---|---|---|
| `GetSnapshot` / `ListSnapshots` | 查询 | 来源对象的版本历史 |
| `GetExtraction` | 查询 | 指定快照的当前或指定解析修订 |
| `ResolveEvidence` | 查询 | 证据引用 → 原文片段、上下文、可视化定位（供界面高亮）；记录证据返回来源记录与字段取值；派生证据返回转换、参考数据版本与逐项输入，可逐层展开到文档或记录证据。按调用方用途与全部输入来源的权利逐维取最严格的展示级别（全文 / 摘要 / 仅引用与链接 / 不展示），只返回该级别允许的内容，并返回所用级别与原因码（`rights.restricted`）；同时返回快照保存时间、来源是否只允许保存元数据，以及来源网站的可达状态（来自 acquisition 来源健康）（[PRD 第4段](../../01-product/prd/04-object-page-evidence.md) F6） |
| `RecordDerivation` | 命令 | 领域事实投影器登记派生证据：校验输入证据存在、链无环、转换已登记，返回 `ev_`；同样内容幂等返回同一标识 |
| `GetDocument` | 查询 | 规范化文档与签名访问地址 |
| `RequestReextraction` | 命令 | 对指定范围用指定解析器版本重放 |
| `SubmitManualCorrection` | 命令 | 运营人员的人工修正：生成新的解析修订（方法=人工），不改变旧修订 |
| `GetEnrichment` | 查询 | 指定快照或分块在指定增强器版本下的增强结果 |
| `RunEnrichment` | 命令 | 对指定范围运行指定增强器版本（异步、受成本预算约束） |
| `PromoteEnricherVersion` | 命令 | 经评估通过后将增强器版本设为生效（触发索引重建） |

## 6. 事件

载荷唯一定义见[`contracts/events/evidence/events.asyncapi.yaml`](../../../contracts/events/evidence/events.asyncapi.yaml)。复用公共事件原语，稳定制品引用不含签名URL或凭据；事件正反例及引用兼容检查通过不代表生产发布/消费、事务发件箱或重放已实现。

**发布**：`evidence.snapshot.created.v1`、`evidence.extraction.revised.v1`（载荷含快照、修订、领域包标识、质量状态）、`evidence.quality.flagged.v1`、`evidence.artifact.refetch_requested.v1`、`evidence.enrichment.completed.v1`（单个快照的增强完成）、`evidence.enricher.promoted.v1`（增强器新版本生效）。

公共实例消费单公告 `acquisition.fetch.completed.v2`、分组成员 `v3` 以及浏览器配对观察 `v4`，兼容历史 `v1`。证据读取按当前来源权利确认展示等级。租户文档使用租户范围的命令、事件和消费者。[来源权利](source-rights-runtime.md)

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
- 证据可见性继承来源权利条目，多来源组合时按[数据架构第 12 节](../02-data-architecture.md#12-来源权利与数据权益)逐维取最严格的结果。

## 10. 开放问题

1. OCR与版面API供应商按M1真实文档评测选定，前期不建设私有推理。

SDK语义验证覆盖快照/管线归属、块/表格坐标与quote_hash；人工规范化扫描样本标记方法与范围，不充当生产OCR成绩。

规范化文档的格式已确定，唯一定义为 `contracts/schemas/evidence/normalized-document.schema.json`；文档类制品（含其中的表格）的解析器与证据引用以该坐标系工作，不另行选择平行格式。结构化来源（接口、数据下载）使用结构化记录集，它不是文档的平行格式：两者按制品类型二选一，不对同一快照同时产出。

## 11. 数据域实例与领域校验

公共实例只处理公共来源；租户实例随 Cell 部署，只处理客户授权文档，不发布公共 evidence 事件，不共享公共缓存、语料、凭证或重放范围。租户知识资产的业务所有权属于 customer-context，evidence 租户实例拥有对应处理制品与修订；两者通过接口/事件协作。

通用 SDK 的文档校验覆盖规范化文档、定位、哈希及证据引用完整性；`sdk.derivation.evaluate` 校验固定事实版本、字段路径与字段证据匹配、无环输入图、文档叶子、已登记转换和不可变可取得的参考数据，并按 RFC 8785 重算输出哈希。每条派生最多64个输入，整图最多64层、1024节点；未知转换、未知形态、缺输入和篡改均拒绝。记录证据叶子尚不支持，明确拒绝；结构化记录键唯一与取值哈希仍是首个结构化来源启用前的门禁。字段语义和领域一致性由信息领域包 `semantic_validator` 提供，扩展注册属于本子系统的 extensions 层。公共文档管线与 corpus 语料校验执行文档证据和领域语义校验。evidence 负责增强器专业指标、评估执行与版本生效门禁；公共语料与公共评估快照由本子系统维护，不回查租户 learning 数据。

冻结语料通过 SDK 的一个 `corpus_source` 校验器供 checker 与 replay 共用。`artifact_origin` 为闭合的 `public_http|synthetic_fixture`，缺省保留旧公开 HTTP 语料行为；`source_kind` 继续表示正文分类（例如司法判决），不承担制品来源判定。公开制品保留真实 URL 与 fetched_at；合成制品使用 manifest 绑定的独立本地 source_key、无 URL、带时区 observed_at，禁止 fetched_at 或伪造公开抓取声明。通用夹具信封校验闭合字段、manifest/原件哈希、样本身份和可见合成标记，领域专用名称、来源及标签规则由所属包适配器验证。已含通用可见合成标记的内容不能通过删除夹具元数据、补 URL/fetched_at 重标为公开 HTTP 制品；这是内容真实性控制，不证明来源或标签授权。此登记不产生来源许可，当前权利仍由读取装配判定。

## 12. 实现状况

采集准入和自动解析分开配置。待验收字段质量的来源可先归档、核验原件和提供授权冻结批次。首批已有采集处理器的新增来源均显式声明解析准入，包括只发现列表的来源和需要登录授权的来源。新疆采购与西藏工程的旧观察保留在待解析队列，预验需要经过原站正文绑定。`documents.yaml` 中设为 `live_parse: false` 的来源由 evidence 将原件观察事件与消费收件箱在同一事务提交到待解析表，保留交付顺序与原件引用。暂存期间该来源不生成新的规范化快照或字段事实。未声明该选项的既有来源沿用实时解析。

实时消费和待解析回放均读取采集已归档的原件，在校验引用及字节哈希后创建规范化对象和解析修订。线上 evidence 对 `raw/` 使用读取权限。直接投放的离线语料经验证后由投放进程写入原件并创建规范化对象。

附件充当公告正文时，解析准入须覆盖对应来源、公告类型与实际文件结构。附件选择固定到公告观察和该版附件键，并核对来源原件及可验证的正文角色。字段证据须定位原文件的页、段落或表格单元，绑定附件长度、哈希和解析版本。缺件、格式不支持或证据不足的字段保持未知。仅含元数据的详情不能替代附件内容。排队回放按观察原序，保留未经解析的原件和缺口。

采购解析器能从规范化文档开头辨认出属于当前公告的独立主标题时，以该标题判定公告类型和结果阶段，并将标题字段的证据定位到同一标题。首级标题只有“招标公告”或“采购公告”等已知类型标记时，由该标记确定路由，标题字段仍沿用后续居中标题行的拼接和引用规则。正文标签及正文引用的旧公告标题不得覆盖这些判定或替换标题证据。主标题难以辨认或文档采用既有非独立标题格式时，沿用原有回退规则。缺乏可确认的类型证据时保持未知。

广材网 M2 的附件型与内嵌型样本均已完成授权冻结读回。附件型详情缺少内嵌正文，两份标称 XLS 的附件实际包含 Excel 导出的 HTML 表格，公告正文角色尚未核实。内嵌型样本从 `noticeName` 和 `context` 生成规范化文档。该样本缺少可确认公告类型的证据，类型和字段保持未知。详情 JSON 为公告观察的主原件，附件另存并绑定自己的字段出处。`live_parse` 保持 `false`。[M2 验收记录](../../05-delivery/12-public-source-probe.md#广材网-m2-招采商机复查)

中国化学工程招标的详情适配优先使用原件中的公告专用标题节点。招标提取器以规范化首级公告标题定位完整标题，保留正文内括号中可直接定位的公告编号。列表标题只用于独立对照。该来源的自动解析按独立字段评估准入。[逐源验收](../../05-delivery/12-public-source-probe.md#中国化学工程招标复查)

分组响应的支持与 `acquisition.fetch.completed.v3` 一起验收。原件是完整响应，适配器按观察固定的选择器只提取一条公告，重验公告键、日期和成员摘要，再生成该公告的规范化文档、字段证据和检索分块。证据绑定同时保留完整原件哈希和成员验证信息。冻结批次为分组项单立版本，测试服务经授权读取成员投影。完整组原件的校验与恢复由有相应权限的原件所有者执行。既有单公告原件、快照和冻结 v1 清单沿用原契约。

数据负责人可在公共 evidence 环境对已排队、声明 `live_parse: false` 的来源执行只读预验。命令按入队顺序读取一批固定归档观察，使用正式管线的原件校验、适配器、规范化和字段证据校验。

```sh
mq-evidence-replay --packs packs --assess-pending-source "$source_id" --limit 8
```

为 `MIAOQIA_EVIDENCE_DB_DSN` 配置仅获 `evd_pending_live_events` 读取权限的 evidence 运维连接。VPS 发布包提供 `mq_evidence_assess_ro`。使用 `MIAOQIA_EVIDENCE_OBJECT_STORE_DIR` 只读打开现有归档，文件目录须已存在，S3 身份只授原件和浏览器投影的读取权限。[发布装配](../../../deploy/vps/data-plane/README.md#发布顺序)

命令默认 8 条、最多 16 条。返回观察事件 ID、版本、原件及规范化哈希、解析器版本、质量状态、事实、证据及错误计数。报告只含诊断元数据。规范化成功时的 `field_evidence` 区分 `no_matching_extractor`、`no_valid_evidence` 和 `available`。只有质量校验通过的修订且其事实引用了已验证的证据，才报告 `available`。没有样本、任一条发生读取或解析失败、任一条缺少有效字段证据时退出码为 1。预验独立处理各条观察，待解析队列、快照、对象存储和发件箱无需写入。字段准确率仍用已授权的样本逐项核对，测试服务继续通过采集冻结批次获取材料。采购公告的独立字段验收须逐条核对公告类型、适用的结果阶段、完整标题及其证据位置，确认所引标题属于当前公告。列表标题可供独立对照。

通过独立解析验收后，将该来源设为 `live_parse: true`，更新全部公共 `data-consumer` 副本，再用相同版本的领域包按来源回放。回放过程使用公共 evidence 的应用账号和共享对象存储配置，无需重抓原件。

```sh
mq-evidence-replay --packs packs --pending-source "$source_id" --limit 100
```

每次调用按提交顺序处理至上限，返回 JSON `processed`。重复调用直到返回 `0`。同源新观察排在积压之后，其他来源继续处理。每条观察独立提交，沿用实时入口的原件哈希、版本、权利标签和发件箱规则。处理失败时命令退出，该观察及后续观察保持待处理，修复后可从该条继续。

公共实例的最小文档管线，代码位于 `data/subsystems/evidence/`：

- `snapshotter`：同一来源对象（来源 + 来源对象键）的规范化内容与最新快照不同即为新快照，序号递增，内容回到历史取值同样是新快照；原件按内容寻址存 `raw/<sha256>`，规范化文档存 `normalized/<管线>/<版本>/<snp>.json`，增强运行结果、派生证据与参考数据分别存 `enrichment/run|derivation|reference/<名称>/<版本>/<sha256>.json`（[数据架构 3.2](../02-data-architecture.md#32-对象存储的组织)），对象不可覆盖；
- `document_pipeline`：HTML 使用 SDK 的规范化（`doc.html-dev`，与语料评测同一坐标系）；PDF 只接受经复核的规范化表示，PDF 文本层、OCR 与版面随 M1；
- `extraction_orchestrator`：按包清单加载解析器与领域校验（入口须位于包内），同一快照同一解析器版本只解析一次；证据或领域语义校验不通过、或任一事实被解析器标为 `suspect` 的修订记为 `suspect`（按最严格状态汇总），下游不物化；
- `provenance`：文档证据引用入库，快照、修订与证据引用由触发器保持不可变；
- 事件：`evidence.snapshot.created.v1`、`evidence.extraction.revised.v1`，与数据同事务经发件箱发布；公共管线拒绝租户上下文；
- 实际采集入库：按稳定公告键及观察序号接收变化，读回并校验原件哈希。重复或低于已接受序号的事件不推进快照。采集方提供的存量关联证据与旧快照核对后，可沿用既有快照和证据链。历史 `v1` 在对应地址进入观察处理前兼容接收。对象存储由 `MIAOQIA_EVIDENCE_OBJECT_STORE_DIR` 配置，与 acquisition 指向同一份归档。领域包通过 `provides.documents` 声明正文适配器，分组响应使用 `selection_adapter`。浏览器来源声明 `browser_projection: true`、原件媒体类型和独立的 `live_parse`，从核验通过的惰性文档生成规范化内容。隔离环境已验证 Chromium 同次捕获的 `v4` 进入待解析队列，随后显式重放并从对象存储回读规范化快照。浏览器来源发布仍按采集准入执行。[交付收据](../../../deploy/vps/data-plane/browser/receipt-provider-ingest.jsonl)
- 回放：`mq-evidence-replay` 投放冻结语料（核对原件哈希，权利标签追加 `corpus:<样本>`），不构成来源启用；
- 读取：`api.Extractions`（解析修订）在数据单元内以本地绑定注入 domain-intelligence。

文档证据 `api.EvidenceResolver` 的本地绑定为 `evidence.resolver(database, store, display=...)`，以及互斥的 keyword-only `authorize(groups,purpose)` 可信分组模式（[闭包与绑定](source-rights-runtime.md#固定版本可信来源闭包)）。原回调 `display(rights_tags, purpose)` 通过当前请求上下文核验主体、租户、单元与用途，按当前来源权利返回 `none`、`citation_only`、`excerpt` 或 `full_text`；未装配、用途为空或策略级别未知时拒绝。`none` 与不存在同形；`citation_only` 只返回来源和快照信息，不读对象；`excerpt` 只返回最多512码点的引用片段与定位；`full_text` 返回完整引用与所在块的上下文。正文读取先复用 `normalized_hash` 核对整份规范化文档与快照保存的哈希，再复用 SDK 的快照、管线、定位与 `quote_hash` 校验，损坏或缺失时失败，不返回未经核验的正文。

本地绑定不缓存权限，不生成原件下载 URL，不推定来源可达。data-api 已有内部 listener、TLS/身份 admission、deadline 约束与固定证据和公告分块的来源闭包收集；固定 mock 公告测试通过实际 mTLS 读取事实、证据与分块，部署环境的身份、生产来源权利和原件恢复按各自前置条件验收。未实现：`GetDocument` 等远程查询，结构化记录集/记录证据、生产增强器、质量规则、语料快照与重放工作流。文件系统与 S3 兼容对象存储已接入，部署提供商兼容性按[部署拓扑](../07-deployment-topology.md#6-环境)验收。

`evidence.enrichments(...)` 本地装配提供不可变参考数据登记、封闭recipe运行登记与 `api.Enrichments.get(run_key)`。结果、派生证据及参考内容保存在既有对象存储的 `enrichment/<sha256>.json`，PG存固定输入引用、对象键/哈希、完整分组来源闭包及运行元数据；`evd_reference_data`、`evd_derivations`、`evd_enrichments` 不可更新或删除。对象先写，登记与现有 `evidence.enrichment.completed.v1` 发件箱同事务；回滚可留下未引用对象，重跑按指纹复用证据ID和结果。当前事件唯一键为快照、真实解析修订及recipe，当前recipe明确拒绝多事实修订，不默选序号。

默认没有启用的recipe。显式测试模式只有 `test.buyer-provenance@0.1.0`、`test.winner-provenance@0.1.0`、`test.buyer-provenance@0.1.1`，以 `test.identity@0.1.0` 和不可变 `test-identity-policy@1` 保持原名取值，替换对应名称容器的 `prov`，只证明派生通道。`test.buyer-provenance@0.1.1` 与 `test.buyer-provenance@0.1.0` 使用相同依据，供验证recipe-only运行不追加领域版本。它们不识别生产组织或品类，也不把模型推断标为确定性计算。未登记或未显式启用的recipe拒绝。

`api.Recipe` 描述固定字段、转换、参考版本/内容及输出重建函数，由部署单元通过 `evidence.load_recipe` 复用包内入口来源校验后显式注入；evidence核心不导入行业词表。`proc.building-topic@0.1.0` 只读 `/title/value`，由 IND-BUILDING-MATERIALS 提供，直接使用字节不变的 building-materials taxonomy/vocabulary YAML，参考指纹还固定控制配置、独立评测manifest及人工scope复核manifest。全部最大字面跨度跨分类支系消歧，not_when仅在两侧均完整的并列产品短语局部检查；否定/替代/省略歧义为unknown。所有hard gate通过且输入confidence≥0.8才有rule_score=1并发布，category/prov的confidence为min(1,input)，不是统计正确率。候选与未发布原因留在L3 diagnostics。

发布仅限 `building-topic-scope.json` 中两份官方原件：核对原source_id/URL/raw hash、标题及类型、reviewed与lifecycle引用哈希；标题原证据ID和字段内码点跨度保留于封闭recipe输出。scope_check与类别一同重算，写入不可变result及domain receipt.outputs；未复核原件默认unknown。任意调用方eligible标志不能授权。官方pair只支持CAT-PIPE-DI（自动标题输入0.95，输出0.95），不由“管材及管件”补FIT-DI，不造items/lot，不证明中标人实际供货。真实来源、组织识别和生产认证尚未启用。

派生/参考/结果读取先检查请求上下文、用途及注入的当前展示策略。旧 callback 先以空标签组执行全局认证/租户/用途校验，再检查每组 tags 并取最严格展示级别；structured 模式只提交完整可信来源组，远程 admission 由后续部署装配负责，不把空组当作内容许可。用于应用的完整结果必须全数 `full_text`，并重新核验源事实、recipe、参考/输入指纹及补丁与重算结果。派生解析 `none` 不展示；`citation_only` 仅返回证据ID和级别、不打开正文或派生对象；`excerpt`/`full_text` 才重验完整链并返回转换及固定输入引用，不伪造原文摘录。文档读取与派生叶子共用规范化对象完整性检查。旧文档Resolver与派生的展示回调保持兼容；结构化模式显式继承 `fact_requirements` 提供的父版本完整闭包，并通过 owner 元数据端口动态解析参考绑定，缺证明拒绝。派生分支需显式注入此本地服务，远程认证仍待装配。
