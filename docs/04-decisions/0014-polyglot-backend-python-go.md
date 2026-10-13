# ADR-0014：服务端语言：数据抓取与维护用 Python，平台服务用 Go；前端 TypeScript

> 状态：提议 · 日期：2026-10-13 · 决策者：项目负责人、首席架构师 · 替代：ADR-0006

## 背景

ADR-0006 提议后端全部使用 Python。项目负责人认为不合适：数据抓取与数据维护适合 Python，平台服务应在 Go 与 TypeScript 中经对比选定。平台服务的热路径（每日百万级信号匹配、交互式检索 P95 ≤ 500 毫秒、万级租户接入、渠道长连接、模型流式代理）与长流程编排（研究运行）对并发模型、事件客户端、工作流 SDK 和表达式求值有很高要求。对比过程见 [技术选型分析](../03-architecture/11-technology-selection-analysis.md) 第 2 节。

## 决策

1. `acquisition`、`evidence`、`domain-intelligence`、`entity-graph` 使用 **Python 3.12+**。这四个子系统只处理公共域数据，依赖文档解析、OCR、NLP 与实体解析生态，并承载信息领域包的代码型 SPI。
2. 其余 14 个子系统（`search`、客户智能平面、交互平面、平台平面）使用 **Go**。
3. 前端使用 **TypeScript + React**；服务端不使用 TypeScript，BFF 由 Go 的 `gateway` 实现。
4. 应用领域包与行业包改为**纯声明 + CEL 表达式**，不含可执行代码；策略 DSL 以 CEL 作为表达式语言。信息领域包保留 Python 代码型 SPI。
5. 跨语言调用一律经远程接口（由 OpenAPI 生成的客户端）；同语言内保留本地 / 远程两种绑定。
6. 部署单元由两个变为三个：`acquisition`（Python）、`data`（Python）、`platform`（Go）。
7. 发件箱中继只实现一份（Go），读取全部子系统库。

## 备选

- **全部 Python**（ADR-0006）：热路径受 GIL 与进程模型限制，CEL 实现成熟度低，私有部署开销大；
- **平台服务用 TypeScript**：类型表达力与前端共享有优势，但 Kafka 客户端、CEL、CPU 密集匹配、私有化部署、供应链安全与国内招聘均弱于 Go；
- **全部 Go**：失去文档与 NLP 生态，信息领域包作者门槛高；
- **Java / Kotlin**：运行时开销与启动时间不利于多进程角色与私有部署，没有相对 Go 的额外优势。

## 后果

- 正面：热路径性能与资源效率提升；语言边界与“公共域 / 租户域”安全边界一致；应用领域包不再运行任意代码，SPI 的确定性与资源限制由 CEL 保证；
- 代价：维护 Go 与 Python 两套 `kernel`（Python 侧只覆盖数据供应平面所需）；契约代码生成需覆盖三种语言；Go 缺少和类型，联合类型契约需靠生成与约定；
- 批准后需更新：`03-architecture/04-extensibility.md`（扩展包形式）、`03-architecture/07-deployment-topology.md`（三个部署单元）、`03-architecture/09-codebase-structure.md`（仓库布局与依赖检查）、`03-architecture/03-integration-contracts.md`（跨语言绑定）、`subsystems/customer-context.md`（策略 DSL）、`05-delivery/03-engineering-standards.md`（CI 门禁）、`05-delivery/01-roadmap.md`（M0 代码库）。
