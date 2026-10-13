# 技术选型

> 状态：评审中 · 版本：1.0 · 更新：2026-10-12 · 负责人：首席架构师

选型原则：成熟、可在中国大陆托管或自建、有活跃社区、团队可招聘、避免单一厂商锁定；按终态需求一次选定。标注“ADR”的选型有对应的决策记录。

## 1. 应用

| 领域 | 选型 | 理由 | 备选 |
|---|---|---|---|
| 后端语言 | **Python 3.12+**（ADR-0006） | 数据处理、文档解析与 AI 生态最完整；团队有前期实践经验 | Go（仅在出现明确性能瓶颈的组件中考虑） |
| Web 框架 | FastAPI | 类型驱动、OpenAPI 原生、异步 | Litestar |
| 数据校验 | Pydantic v2 | 与 JSON Schema 双向映射 | — |
| ORM / 数据访问 | SQLAlchemy 2.x | 成熟、支持 RLS 会话变量与多数据库 | — |
| 迁移 | Alembic | 与 SQLAlchemy 配套 | — |
| 包与工作区 | uv workspace | 单仓多包、快速、可复现锁定 | — |
| 依赖约束检查 | import-linter | 在 CI 中强制子系统依赖规则 | — |
| 前端 | **TypeScript + React + Vite** | 企业应用主流生态 | — |
| 前端组件库 | Ant Design | 中文企业应用成熟组件 | — |
| 前端数据层 | TanStack Query + 由 OpenAPI 生成的客户端 | 与契约一致 | — |
| MCP | 官方 MCP Python SDK（Streamable HTTP） | 标准实现 | — |

## 2. 数据与消息

| 领域 | 选型 | 理由 |
|---|---|---|
| 事务数据库 | **PostgreSQL 16+**（ADR-0007） | RLS、分区、JSONB、逻辑复制、生态成熟 |
| 对象存储 | S3 兼容（云厂商对象存储；私有部署使用 MinIO） | 标准接口 |
| 检索 | **OpenSearch**（ADR-0007） | 中文分词插件、k-NN 向量、混合检索、percolator 订阅反查 |
| 图查询读模型 | 待基准测试后选定（NebulaGraph / Neo4j / Apache AGE） | 接口已固定为 `GraphQuery` |
| 事件总线 | **Kafka**（ADR-0004） | 持久、可重放、分区有序、托管服务普及 |
| 发件箱中继 | 自研轻量中继（轮询 + 通知） | 与子系统数据库一一对应，便于拆分；规模化后可替换为 CDC（Debezium） |
| 事件 Schema | JSON Schema + AsyncAPI，CI 兼容性检查 | 与接口契约统一 |
| 缓存 | Redis | 缓存、限流、短期幂等 |
| 工作流 | **Temporal**（ADR-0005） | 持久化、可恢复、定时器、人工中断、确定性重放 |

## 3. 文档与 AI

| 领域 | 选型 | 说明 |
|---|---|---|
| 浏览器渲染 | Playwright（Chromium） | 动态页面采集 |
| HTML 解析 | lxml、selectolax | 高性能 |
| PDF | PyMuPDF / pdfplumber | 文本与版面 |
| OCR 与版面 | PaddleOCR / PP-Structure（私有部署） | 中文效果好，可境内私有化 |
| Office | python-docx、openpyxl 等；必要时 LibreOffice 沙箱转换 | — |
| 模型访问 | 自研模型网关（`ai-platform`），供应商适配器 | 统一路由、计量与治理 |
| 托管模型 | 境内已备案的大模型服务（多供应商） | 具体清单由合规确认 **[待核验]** |
| 私有模型推理 | vLLM（GPU 节点池） | 敏感数据与专属部署 |
| 嵌入与重排 | 中文开源嵌入与重排模型，私有部署 | 公共域一次计算多租户复用 |
| Agent 运行时 | 自研（基于 Temporal 工作流） | 避免 Agent 框架锁定，完全掌控工具权限与可观测（ADR-0010） |

## 4. 身份与安全

| 领域 | 选型 | 说明 |
|---|---|---|
| 身份提供者 | **Keycloak**（ADR-0012） | OIDC/OAuth 2.1、联合身份、可自建 |
| 授权 | 关系型授权服务（OpenFGA）+ 属性条件 | ReBAC 适合资源归属与协作关系 |
| 密钥 | 云 KMS + 密钥管理服务 | 租户数据密钥、外部凭证 |
| 供应链 | 镜像签名、SBOM、依赖扫描 | — |

## 5. 运行与运维

| 领域 | 选型 |
|---|---|
| 容器编排 | Kubernetes（云厂商托管版） |
| 部署 | Helm + Argo CD（GitOps） |
| 基础设施即代码 | Terraform |
| 可观测性 | OpenTelemetry；Prometheus + Grafana；日志存储（Loki 或云日志）；追踪（Tempo 或 Jaeger） |
| CI | GitHub Actions |
| 本地开发 | Docker Compose（全部基础组件）+ 进程角色启动脚本 |

## 6. 评估后不引入的组件

| 组件 | 不引入的原因 | 重新评估的条件 |
|---|---|---|
| ClickHouse 等列式分析库 | 数据规模在 PostgreSQL 分区、汇总表与只读副本的能力范围内，多一套存储增加运维和数据同步成本 | 出现面向客户的交互式多维分析，且汇总表与只读副本无法满足 |

## 7. 云与区域

生产部署在中国大陆区域的主流公有云（阿里云或腾讯云，ADR 待定），优先使用托管的 PostgreSQL、Kafka、对象存储、Redis 与 Kubernetes；Temporal、Keycloak、模型推理自建。所有组件均有私有部署方案。
