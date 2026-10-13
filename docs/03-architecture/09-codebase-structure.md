# 代码库结构

> 状态：评审中 · 版本：2.0 · 更新：2026-10-13 · 负责人：首席架构师

## 1. 单一代码仓库

妙洽采用单一代码仓库（monorepo）管理所有子系统、扩展包、契约、前端与部署配置。子系统的独立性由目录边界、独立数据库与自动化依赖检查保证，而不是由仓库隔离。子系统使用哪种语言以 [子系统索引](subsystems/README.md) 为准（ADR-0014）。

```text
miaoqia/
├── contracts/                     契约（唯一事实来源，与语言无关）
│   ├── schemas/                   JSON Schema：common/ 通用类型、domains/<信息领域>/ 领域对象、研究产物
│   ├── apis/<subsystem>/          子系统接口 OpenAPI
│   ├── events/<namespace>/        事件 AsyncAPI + Schema
│   ├── policy/                    策略 DSL Schema 与 CEL 环境声明
│   ├── packs/                     扩展包定义 Schema
│   └── identifiers.yaml           标识前缀登记
│
├── services/                      Go 模块：平台服务
│   ├── kernel/                    通用基础设施（数据访问与 RLS、发件箱、事件消费、工作流、
│   │                              配置、可观测、接口绑定、授权与审计 SDK、扩展包加载与 CEL 求值）
│   ├── subsystems/<name>/         每个 Go 子系统一个目录
│   ├── relay/                     发件箱中继（服务全部子系统库）
│   └── cmd/platform/              platform 部署单元入口与进程角色
│
├── data/                          Python 工作区（uv）：公共域数据处理
│   ├── kernel/                    miaoqia.kernel：数据访问与 RLS、发件箱写入、事件消费、
│   │                              Temporal worker、配置、可观测、请求上下文、授权客户端
│   ├── sdk/                       miaoqia.sdk：信息领域包 SPI
│   ├── subsystems/<name>/         每个 Python 子系统一个包
│   └── runtimes/{acquisition,data}/  两个 Python 部署单元入口与进程角色
│
├── packs/                         扩展包
│   ├── information_domains/       Python 代码 + 声明（如 procurement/ 即 INF-PROC）
│   ├── application_domains/       纯声明 + CEL（如 tendering/ 即 APP-TENDER）
│   └── industries/                纯声明
│
├── web/                           前端（TypeScript 工作区）
│   ├── apps/workspace/            客户 Web 应用
│   ├── apps/ops-console/          运营控制台
│   └── packages/                  组件、生成的 API 客户端、视图注册表
│
├── deploy/                        Helm charts、角色清单、环境配置、OpenTofu
├── tools/                         代码生成、契约检查、本地开发脚本
├── tests/
│   ├── contract/                  跨子系统黑盒契约测试（同一套用例验证 Go 与 Python 实现）
│   ├── e2e/                       端到端测试
│   ├── security/                  跨租户、未知字段、提示注入、审批绕过测试
│   └── evaluation/                评估集回归
└── docs/                          本文档体系
```

## 2. 子系统内部结构

两种语言使用相同的分层，Go 子系统把 `api` 以外的层放在 `internal/` 下，由编译器保证外部不可导入：

| 层 | 内容 | Go 位置 | Python 位置 |
|---|---|---|---|
| `api` | 公开接口：命令、查询、DTO、事件定义（其他子系统唯一可导入的部分） | `<name>/api/` | `<name>/api/` |
| `domain` | 领域模型与业务规则（无框架依赖） | `<name>/internal/domain/` | `<name>/domain/` |
| `application` | 用例编排、事务边界、事件发布 | `<name>/internal/app/` | `<name>/application/` |
| `infrastructure` | 数据访问、外部适配、事件消费者、工作流与活动 | `<name>/internal/infra/` | `<name>/infrastructure/` |
| `extensions` | 本子系统定义的扩展点注册表 | `<name>/internal/ext/` | `<name>/extensions/` |
| `migrations` | 本子系统数据库迁移 | `<name>/migrations/` | `<name>/migrations/` |

## 3. 依赖规则（CI 强制）

| 规则 | 说明 |
|---|---|
| R1 | 子系统 A 只能导入子系统 B 的 `api` 包，禁止导入其其余各层 |
| R2 | 依赖方向符合 [总体架构](01-architecture-overview.md#22-依赖规则)：交互 → 客户智能 → 数据供应；数据供应平面不得导入客户智能平面 |
| R3 | 所有子系统可依赖本语言的 `kernel` 与 `sdk`；`kernel` 与 `sdk` 不依赖任何子系统 |
| R4 | 扩展包只能依赖 `sdk` 与被扩展子系统的 `api` |
| R5 | `domain` 层不依赖任何框架与基础设施库 |
| R6 | 子系统不得访问其他子系统的数据库（运行时：独立数据库账号无权限；静态：禁止引用其他子系统的表模型） |
| R7 | 跨语言依赖只能经 `contracts/` 生成的远程客户端，不共享任何手写代码 |

执行方式：Go 以 `internal/` 可见性与 golangci-lint `depguard` 检查；Python 以 import-linter 检查；两者都辅以数据库权限。

## 4. 接口绑定

同语言内，`kernel` 提供接口绑定机制，调用方只依赖接口：

```go
// 调用方只依赖 api 包中的接口
import "github.com/superailabs/miaoqia/services/subsystems/search/api"

type ChunkSearchTool struct {
    search api.SearchQueries
}
```

部署配置决定接口绑定到本地实现还是由 OpenAPI 生成的远程客户端；跨语言时（例如 Go 的 `research` 调用 Python 的 `entity-graph`）只有远程客户端一种绑定。所有绑定都经过统一的调用管线：上下文传递、授权、超时、重试策略、追踪与指标。

## 5. 代码生成

| 来源 | Go | Python | TypeScript |
|---|---|---|---|
| `contracts/schemas` | 类型与校验 | Pydantic 模型 | 类型 |
| `contracts/apis` | 服务端骨架与客户端（ogen） | 服务端路由骨架与客户端 | 前端客户端 |
| `contracts/events` | 事件模型、发布与消费辅助 | 事件模型、发布与消费辅助 | — |
| `contracts/policy` | CEL 环境与类型声明 | — | 策略编辑器的类型与提示 |
| `contracts/packs` | 扩展包定义校验器 | 扩展包定义校验器 | — |

生成代码不手工修改；CI 校验生成物与契约一致。

## 6. 命名约定

| 对象 | 约定 | 示例 |
|---|---|---|
| Go 子系统包 | `services/subsystems/<lowercase>` | `services/subsystems/customercontext` |
| Python 子系统包 | `miaoqia.<snake_case>` | `miaoqia.entity_graph` |
| 数据库 | 三字母代码，见 [子系统索引](subsystems/README.md) | `ent` |
| 事件类型 | `<namespace>.<entity>.<verb>.v<n>` | `entity.merge.applied.v1` |
| 标识前缀 | 2–4 字母 + `_` | `org_`、`sig_` |
| 接口 | 名词资源 + 动词命令 | `GET /v1/signals`、`POST /v1/policies/{id}:activate` |
