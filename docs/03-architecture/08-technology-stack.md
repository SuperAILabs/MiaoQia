# 技术选型

> 状态：评审中 · 版本：2.0 · 更新：2026-10-13 · 负责人：首席架构师

选型原则：成熟、开源、可离线自建、许可证允许向私有部署客户分发、团队可招聘、不依赖任何云厂商的专有服务；能自建的开源方案全部自建，只有托管价格优势明显且只经标准协议使用时才选择托管（ADR-0015）。每项选型的需求依据与候选对比见 [技术选型分析](11-technology-selection-analysis.md)。标注“ADR”的选型有对应的决策记录。

## 1. 语言与部署单元（ADR-0014）

| 范围 | 语言 | 子系统 | 部署单元 |
|---|---|---|---|
| 数据抓取与数据维护（公共域） | **Python 3.12+** | `acquisition` | `acquisition` |
| | | `evidence`、`domain-intelligence`、`entity-graph` | `data` |
| 平台服务（租户域、交互、平台平面） | **Go** | `search`、`customer-context`、`detection`、`research`、`assessment`、`pursuit`、`learning`、`gateway`、`channels`、`identity`、`commerce`、`governance`、`ai-platform`、`operations` | `platform` |
| 前端 | **TypeScript** | Web 应用、运营控制台 | 静态资源 |

扩展包：信息领域包为 Python 代码 + 声明；应用领域包与行业包为纯声明 + CEL 表达式。

## 2. 应用

### 2.1 Go（平台服务）

| 领域 | 选型 | 理由 |
|---|---|---|
| HTTP 与契约 | 标准库 `net/http` + ogen（由 OpenAPI 生成服务端与客户端） | 契约先行，生成代码不手改 |
| 数据访问 | pgx v5 + sqlc | SQL 优先、编译期类型安全、直接设置 RLS 会话变量 |
| 迁移 | goose | 每子系统独立迁移目录 |
| 事件 | franz-go | 纯 Go、功能完整的 Kafka 客户端 |
| 工作流 | Temporal Go SDK + `workflowcheck` | 参考实现，静态检查确定性 |
| 表达式与策略 | cel-go | 策略 DSL、特征函数、判断与路径条件：确定性、可类型检查、可限成本 |
| JSON Schema 校验 | santhosh-tekuri/jsonschema | 出入站白名单、研究产物 Schema 校验 |
| 授权 | OpenFGA Go SDK | 统一授权 SDK |
| MCP | 官方 MCP Go SDK（Streamable HTTP） | 标准实现 |
| 离线计算 | DuckDB（Go 绑定） | `learning` 指标与仿真汇总 |
| 依赖规则 | `internal/` 可见性 + golangci-lint `depguard` | CI 强制子系统依赖规则 |
| 静态检查 | gofmt、golangci-lint、govulncheck | — |

### 2.2 Python（数据抓取与维护）

| 领域 | 选型 | 理由 |
|---|---|---|
| Web 框架 | FastAPI | 内部接口与运营管理接口 |
| 数据校验 | Pydantic v2 | 与 JSON Schema 双向映射 |
| 数据访问 / 迁移 | SQLAlchemy 2.x / Alembic | 成熟、支持 RLS 会话变量 |
| 事件 | confluent-kafka-python | librdkafka 绑定 |
| 工作流 | Temporal Python SDK | 采集计划、回填、重放 |
| 包与工作区 | uv workspace | 单仓多包、快速、可复现锁定 |
| 依赖规则 / 静态检查 | import-linter / ruff、pyright | — |
| 批处理与语料 | Parquet + Apache Arrow + DuckDB | 语料快照、批量重算增强（ADR-0013） |

### 2.3 前端

| 领域 | 选型 | 理由 |
|---|---|---|
| 语言与构建 | TypeScript + React + Vite（pnpm 工作区） | 企业应用主流生态 |
| 组件库 | Ant Design | 中文企业应用成熟组件 |
| 数据层 | TanStack Query + 由 OpenAPI 生成的客户端 | 与契约一致 |
| 移动端 | 响应式 H5 | 产品体验方案已决定 |

## 3. 数据与消息（自建，ADR-0015）

| 领域 | 选型 | 说明 |
|---|---|---|
| 事务数据库 | **PostgreSQL 17+**（CloudNativePG，ADR-0007） | RLS、分区、只读副本、PITR |
| 对象存储 | **S3 标准协议**；生产使用 IaaS 供应商对象存储（价格例外，只用标准协议）；私有部署与 `staging` 使用 Ceph RGW（Rook） | 见分析 3.3 |
| 检索 | **OpenSearch 3.x**（自建，OpenSearch Operator；ADR-0007、ADR-0013） | 中文分词、k-NN、混合检索、percolator；版本化索引与别名切换 |
| 图查询读模型 | 基准测试后选定（PostgreSQL 递归查询 / Apache AGE / NebulaGraph） | 接口已固定为 `GraphQuery` |
| 事件总线 | **Kafka 4.x KRaft**（Strimzi，ADR-0004） | 持久、可重放、分区有序 |
| 发件箱中继 | 自研 Go 中继（轮询 + 通知），读取全部子系统库 | 与语言无关；规模化后可替换为 CDC（Debezium） |
| 事件 Schema | JSON Schema + AsyncAPI，CI 兼容性检查 | 与接口契约统一 |
| 缓存 | **Valkey**（Redis 协议） | BSD 许可；缓存、限流、短期幂等 |
| 工作流 | **Temporal**（自建，独立 PostgreSQL 集群；ADR-0005） | 持久化、可恢复、定时器、人工中断、确定性重放 |

## 4. 文档与 AI（ADR-0016）

| 领域 | 选型 | 说明 |
|---|---|---|
| 浏览器渲染 | Playwright（Chromium） | 动态页面采集 |
| HTML 解析 | lxml、selectolax | 高性能 |
| PDF | pdfplumber、pypdfium2 | 文本与版面；PyMuPDF（AGPL）未取得商业许可前不作为默认依赖 |
| OCR 与版面 | 文档解析/OCR API 与多模态模型 API；PaddleOCR（CPU）为可选自建方案 | 前期不建设 GPU |
| Office | python-docx、openpyxl 等；必要时 LibreOffice 沙箱转换 | — |
| 模型访问 | 自研模型网关（`ai-platform`，Go），OpenAI 兼容协议适配器为主 | 统一路由、计量与治理；私有部署客户可指向自有端点 |
| 模型供应商 | 境内已备案的多家模型 API | 合规已由法务确认；按评测选定主选与备选 |
| 嵌入与重排 | 供应商嵌入与重排 API | 公共域一次计算、多租户复用；模型版本记录在索引版本中 |
| 私有模型推理 | 不建设 | 重新评估条件见分析 4.4 |
| Agent 运行时 | 自研（基于 Temporal Go SDK） | 完全掌控工具权限与可观测（ADR-0010） |

## 5. 身份与安全

| 领域 | 选型 | 说明 |
|---|---|---|
| 身份提供者 | **Keycloak**（ADR-0012） | OIDC/OAuth 2.1、联合身份、自建高可用 |
| 授权 | **OpenFGA** + 属性条件 | ReBAC 适合资源归属与协作关系 |
| 密钥 | **OpenBao**（Transit 信封加密）+ External Secrets Operator | 租户数据密钥、外部凭证；替代云 KMS |
| 供应链 | Harbor（镜像签名与扫描）、SBOM、govulncheck、依赖扫描；依赖经自建代理缓存 | — |

## 6. 运行与运维

| 领域 | 选型 |
|---|---|
| 容器编排 | **RKE2**（自建 Kubernetes，SaaS 与私有部署同一发行版） |
| 入口 | Envoy Gateway（Kubernetes Gateway API） |
| 部署 | Helm + Argo CD（GitOps） |
| 基础设施即代码 | OpenTofu |
| 镜像仓库 | Harbor |
| 可观测性 | OpenTelemetry Collector；VictoriaMetrics + Grafana；Loki；Tempo（AGPL 组件随私有部署分发前经法务确认） |
| CI | GitHub Actions + 境内自建 runner |
| 本地开发 | Docker Compose（全部基础组件）+ 三个部署单元的进程角色启动脚本 |

## 7. 评估后不引入的组件

| 组件 | 不引入的原因 | 重新评估的条件 |
|---|---|---|
| ClickHouse 等列式分析库 | 数据规模在 PostgreSQL 分区、汇总表与只读副本的能力范围内，多一套存储增加运维和数据同步成本 | 出现面向客户的交互式多维分析，且汇总表与只读副本无法满足 |
| 云厂商 PaaS（托管数据库、消息、检索、缓存、Kubernetes、KMS） | 形成厂商锁定，私有部署无法复用同一清单 | 某项托管服务价格优势明显且只经标准协议使用（ADR-0015） |
| 私有模型推理与 GPU | 前期 API 足以覆盖，嵌入与生成调用量可控 | 见分析 4.4 |
| MinIO | 社区版已转为仅源码分发并停止积极维护 | — |
| TypeScript 服务端 | 见分析 2.3 | — |

## 8. 云与区域

云厂商只作为 IaaS 供应商（虚拟机、云盘、VPC、四层负载均衡、带宽、对象存储），生产部署在中国大陆区域，至少两个可用区。供应商按报价、区域与等保配套选择（C1），可在不同云与自有机房之间迁移，Kubernetes 以上的部署清单不变。
