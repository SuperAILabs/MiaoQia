# ADR-0014：服务端语言：数据抓取与维护用 Python，平台服务用 Go；前端 TypeScript

> 状态：已批准 · 日期：2026-10-13 · 决策者：项目负责人、首席架构师 · 替代：ADR-0006

## 背景

ADR-0006 提议后端全部使用 Python。项目负责人认为不合适：数据抓取与数据维护适合 Python，平台服务应在 Go 与 TypeScript 中对比选定。平台服务的热路径（每日百万级信号匹配、交互式检索 P95 ≤ 500 毫秒、万级租户接入、渠道长连接、模型流式代理）与研究长流程，对并发模型、Kafka 客户端、Temporal SDK 与表达式求值要求很高；数据供应平面则依赖文档解析、OCR、NLP 与实体解析生态，并承载信息领域包的代码。

## 决策

1. 公共域数据处理子系统（`acquisition`、`evidence`、`domain-intelligence`、`entity-graph`）使用 **Python**；其余子系统使用 **Go**；前端使用 **TypeScript**，服务端不使用 TypeScript（BFF 由 Go 的 `gateway` 实现）。子系统与语言、部署单元的对应以 [子系统索引](../03-architecture/subsystems/README.md) 为准。
2. 语言边界与“公共域 / 租户域”边界一致：Python 部署单元不处理租户数据。`evidence` 解析客户授权文档时，以租户单元为范围单独部署实例。
3. 应用领域包与行业包为**纯声明 + CEL 表达式**，不含可执行代码；策略 DSL 的表达式语言为 CEL。信息领域包保留 Python 代码型 SPI。
4. 跨语言调用一律经远程接口（由 OpenAPI 生成的客户端）；同语言内保留本地 / 远程两种绑定。发件箱中继只实现一份（Go）。

边界子系统的归属依据：

| 子系统 | 语言 | 依据 |
|---|---|---|
| `search` | Go | 全平台在线检索与订阅反查热路径；持有租户订阅查询与租户私有索引；映射与词表均为声明式 |
| `entity-graph` | Python | 实体解析依赖相似度、规则与模型候选排序；领域包提供 `RelationExtractor` 代码 |
| `domain-intelligence` | Python | 领域包提供 `Aggregator` 代码、领域 Schema 与迁移 |
| `learning` | Go | 处理租户反馈；策略仿真复用 `detection` 的同一份 CEL 求值，避免双实现 |
| `operations` | Go | 内部人员访问租户数据的工作台；数据维护动作调用 Python 子系统的命令接口完成 |
| `research` | Go | Agent 运行时自研并构建在 Temporal 上，Go SDK 是参考实现 |

## 备选

平台服务 Go 与 TypeScript 的对比（1–5 分，5 最好）：

| 维度 | 权重 | Go | TypeScript | 依据 |
|---|---|---|---|---|
| Temporal SDK | 高 | 5 | 4 | Go SDK 是参考实现，有 `workflowcheck` 静态检查；TS SDK 成熟但以 V8 沙箱实现确定性，开销更大 |
| Kafka 客户端 | 高 | 5 | 3 | franz-go 纯 Go、功能完整；Node 的 KafkaJS 长期缺乏维护，librdkafka 绑定需原生构建 |
| 策略与规则求值 | 高 | 5 | 3 | cel-go 是 CEL 参考实现：确定性、可类型检查、可限成本；JS 实现成熟度低 |
| 匹配热路径 | 高 | 5 | 3 | CPU 密集确定性计算，Go 多核并行、尾延迟稳定；Node 需 worker_threads |
| 高并发 I/O 与长连接 | 中 | 5 | 4 | 两者都适合，goroutine 对流式代理与长连接更直接 |
| PostgreSQL 与 RLS | 中 | 5 | 4 | pgx + sqlc 编译期类型安全；TS 的 Prisma 对会话变量支持不佳 |
| 契约代码生成 | 中 | 4 | 5 | TS 类型生成最完整；Go 对 `oneOf` 等联合类型较笨重 |
| 领域建模表达力 | 中 | 3 | 5 | Go 无和类型 |
| LLM 与 MCP 生态 | 中 | 4 | 5 | TS 最丰富；Go 有官方 MCP 与模型 SDK，且模型网关自研、供应商普遍兼容 OpenAI 协议 |
| 部署与私有化 | 中 | 5 | 3 | 静态二进制、镜像小、内存低 |
| 供应链安全 | 中 | 5 | 2 | Go 依赖少、有校验和数据库；npm 依赖树深，等保与私有部署审计负担大 |
| 国内后端招聘 | 中 | 5 | 3 | — |
| 编码 Agent 产出 | 中 | 5 | 4 | 语言面小、gofmt 统一、编译期检查严格，生成代码差异小、易评审 |

Go 在权重最高的维度全面领先；TypeScript 的优势（类型表达力、前端共享）在契约先行后可由代码生成弥补。

其他备选：

- **全部 Python**（ADR-0006）：热路径受 GIL 与进程模型限制，CEL 实现成熟度低，私有部署开销大；
- **全部 Go**：失去文档与 NLP 生态，信息领域包作者门槛高；
- **Java / Kotlin**：运行时开销与启动时间不利于多进程角色与私有部署，相对 Go 无额外优势。

## 后果

- 正面：热路径性能与资源效率提升；安全边界即语言与部署边界；应用领域包不运行任意代码，SPI 的确定性与资源限制由 CEL 保证；
- 代价：维护 Go 与 Python 两套 `kernel`（Python 侧只覆盖数据供应平面所需）；契约代码生成覆盖三种语言；联合类型契约在 Go 中需靠生成与约定；
- 部署单元变为三个，见 ADR-0002 与 [部署拓扑](../03-architecture/07-deployment-topology.md)。
