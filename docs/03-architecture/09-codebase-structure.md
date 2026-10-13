# 代码库结构

> 状态：评审中 · 版本：1.0 · 更新：2026-10-12 · 负责人：首席架构师

## 1. 单一代码仓库

妙洽采用单一代码仓库（monorepo）管理所有子系统、扩展包、契约、前端与部署配置。子系统的独立性由目录边界、独立数据库与自动化依赖检查保证，而不是由仓库隔离。

```text
miaoqia/
├── contracts/                     契约（唯一事实来源）
│   ├── schemas/                   领域对象与研究产物 JSON Schema
│   ├── apis/<subsystem>/          子系统接口 OpenAPI
│   ├── events/<namespace>/        事件 AsyncAPI + Schema
│   ├── policy/                    策略 DSL Schema
│   ├── packs/                     扩展包定义 Schema
│   └── identifiers.yaml           标识前缀登记
│
├── platform/                      Python 工作区
│   ├── sdk/                       miaoqia.sdk：扩展 SPI、请求上下文、授权与审计 SDK
│   ├── kernel/                    miaoqia.kernel：通用基础设施（数据访问、发件箱、事件消费、
│   │                              工作流、配置、可观测、接口绑定与远程客户端生成）
│   ├── subsystems/
│   │   ├── acquisition/
│   │   ├── evidence/
│   │   ├── domain_intelligence/
│   │   ├── entity_graph/
│   │   ├── search/
│   │   ├── customer_context/
│   │   ├── detection/
│   │   ├── research/
│   │   ├── assessment/
│   │   ├── pursuit/
│   │   ├── learning/
│   │   ├── gateway/
│   │   ├── notification/
│   │   ├── identity/
│   │   ├── commerce/
│   │   ├── governance/
│   │   ├── ai_platform/
│   │   └── operations/
│   └── runtimes/
│       ├── acquisition/           acquisition 部署单元入口与进程角色
│       └── platform/              platform 部署单元入口与进程角色
│
├── packs/                         扩展包
│   ├── information_domains/
│   │   ├── procurement/           INF-PROC
│   │   ├── investment_projects/   INF-INVP
│   │   ├── organizations/         INF-ORG
│   │   └── …
│   ├── application_domains/
│   │   └── tendering/             APP-TENDER
│   └── industries/
│       ├── building_materials/
│       └── …
│
├── web/                           前端（TypeScript 工作区）
│   ├── apps/workspace/            客户 Web 应用
│   ├── apps/ops-console/          运营控制台
│   └── packages/                  组件、生成的 API 客户端、视图注册表
│
├── deploy/                        Helm charts、角色清单、环境配置、Terraform
├── tools/                         代码生成、契约检查、本地开发脚本
├── tests/
│   ├── contract/                  跨子系统契约测试
│   ├── e2e/                       端到端测试
│   ├── security/                  跨租户、未知字段、提示注入、审批绕过测试
│   └── evaluation/                评估集回归
└── docs/                          本文档体系
```

## 2. 子系统内部结构

每个子系统遵循相同的内部分层：

```text
subsystems/<name>/
├── api/              公开接口：命令、查询、DTO、事件定义（其他子系统唯一可导入的部分）
├── domain/           领域模型与业务规则（无框架依赖）
├── application/      用例编排、事务边界、事件发布
├── infrastructure/   数据访问、外部适配、事件消费者、工作流与活动
├── extensions/       本子系统定义的 SPI 注册表
├── migrations/       本子系统数据库迁移
└── tests/            单元与集成测试
```

## 3. 依赖规则（CI 强制）

| 规则 | 说明 |
|---|---|
| R1 | 子系统 A 只能导入子系统 B 的 `api` 包，禁止导入其 `domain`、`application`、`infrastructure` |
| R2 | 依赖方向符合 [总体架构](01-architecture-overview.md#22-依赖规则)：交互 → 客户智能 → 数据供应；数据供应平面不得导入客户智能平面 |
| R3 | 所有子系统可依赖 `kernel` 与 `sdk`；`kernel` 与 `sdk` 不依赖任何子系统 |
| R4 | 扩展包只能依赖 `sdk` 与被扩展子系统的 `api` |
| R5 | `domain` 层不依赖任何框架与基础设施库 |
| R6 | 子系统不得访问其他子系统的数据库（运行时：独立数据库账号无权限；静态：禁止引用其他子系统的表模型） |

规则以 import-linter 配置和数据库权限双重保证。

## 4. 接口绑定

`kernel` 提供接口绑定机制：

```python
# 调用方只依赖接口
from miaoqia.entity_graph.api import EntityGraphQueries

class WinnerProfileTool:
    def __init__(self, entities: EntityGraphQueries): ...
```

部署配置决定 `EntityGraphQueries` 绑定到本地实现还是由 OpenAPI 生成的远程客户端。两种绑定都会经过统一的调用管线：上下文传递、授权、超时、重试策略、追踪与指标。

## 5. 代码生成

| 来源 | 生成物 |
|---|---|
| `contracts/schemas` | Pydantic 模型、TypeScript 类型 |
| `contracts/apis` | 子系统远程客户端、服务端路由骨架、前端客户端 |
| `contracts/events` | 事件模型、发布与消费辅助代码 |
| `contracts/packs` | 扩展包定义校验器 |

生成代码不手工修改；CI 校验生成物与契约一致。

## 6. 命名约定

| 对象 | 约定 | 示例 |
|---|---|---|
| 子系统包 | `miaoqia.<snake_case>` | `miaoqia.entity_graph` |
| 数据库 | 三字母代码 | `ent` |
| 事件类型 | `<namespace>.<entity>.<verb>.v<n>` | `entity.merge.applied.v1` |
| 标识前缀 | 2–4 字母 + `_` | `org_`、`sig_` |
| 接口 | 名词资源 + 动词命令 | `GET /v1/signals`、`POST /v1/policies/{id}:activate` |
