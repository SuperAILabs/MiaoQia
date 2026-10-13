# 部署拓扑

> 状态：评审中 · 版本：2.0 · 更新：2026-10-13 · 负责人：首席架构师、平台工程负责人

## 1. 核心决策

**逻辑架构按终态设计，部署从少量单元开始，演进通过配置完成。**（[ADR-0002](../04-decisions/0002-composable-deployment.md)）

- 18 个子系统的边界、数据所有权、接口与事件契约从第一天按终态实现；
- 基础设施从第一天按生产级自建（组件见 [技术选型](08-technology-stack.md)，原则见 [ADR-0015](../04-decisions/0015-self-hosted-open-source-infrastructure.md)），不使用替代方案过渡；
- 应用代码打包为三个部署单元：`acquisition`、`data`、`platform`；每个子系统属于哪个单元以 [子系统索引](subsystems/README.md) 为准；
- 任一子系统可以通过部署配置从所在单元拆出，独立部署，代码与契约不变。

三个单元的划分依据是**故障域、伸缩方式、安全边界与语言的根本差异**，而不是开发便利：

| 维度 | `acquisition` | `data` | `platform` |
|---|---|---|---|
| 语言 | Python | Python | Go |
| 数据域 | 公共域 | 公共域 | 租户域、平台域；`search` 的公共域索引 |
| 网络 | 大量出站访问外部网站，需要出口管理 | 不访问外部网站（模型 API 除外） | 不访问任意外部网站（模型与渠道除外） |
| 负载 | I/O 密集、突发、浏览器渲染消耗大 | 文档处理、解析、增强、实体解析，CPU 密集、批量 | 事务、查询、匹配、事件处理、工作流 |
| 故障 | 来源变化、反爬、超时频繁 | 解析失败可重放，不影响在线服务 | 需要稳定、低延迟 |
| 安全 | 处理不可信外部内容的第一道边界 | 处理不可信文档内容；不接触租户数据 | 处理租户数据 |
| 发布节奏 | 来源处理器频繁更新 | 解析器与增强器随领域包更新 | 按版本节奏发布 |

## 2. 部署单元

每个单元一个镜像，以不同进程角色启动；进程角色通过部署清单选择加载哪些子系统。

### 2.1 `acquisition` 单元

| 进程角色 | 内容 | 伸缩依据 |
|---|---|---|
| `acq-scheduler` | 采集计划与回填工作流（Temporal worker，任务队列 `acquisition-schedule`） | 固定副本 |
| `acq-fetcher` | HTTP/API 抓取执行器 | 任务队列深度 |
| `acq-browser` | 浏览器渲染池（Playwright） | 渲染任务队列深度、内存 |
| `acq-admin` | 来源注册、权利、健康的内部管理接口（经 `gateway` 暴露给运营控制台） | 固定副本 |

网络：独立的出口网关与出口地址池；入站仅允许来自内部网络的管理请求。

### 2.2 `data` 单元

| 进程角色 | 加载的子系统 | 职责 | 伸缩依据 |
|---|---|---|---|
| `data-api` | evidence、domain-intelligence、entity-graph | 供上层查询的接口（领域事实、统计、实体、证据解析） | 请求速率、延迟 |
| `data-consumer` | evidence、domain-intelligence、entity-graph | 事件处理：解析修订、领域事实更新、实体解析 | 消费延迟 |
| `document-worker` | evidence | 文档处理、解析、增强（OCR 与版面经模型 API） | 文档队列深度 |
| `data-workflow-worker` | evidence、domain-intelligence、entity-graph | 重放、批量增强、语料快照、回填 | 工作流任务队列深度 |

客户授权文档（租户知识资产）的解析由 `document-worker` 的**单元级实例**完成：随租户单元部署，使用单元级凭证与存储，不与公共域实例混用。

### 2.3 `platform` 单元

| 进程角色 | 加载的子系统 | 职责 | 伸缩依据 |
|---|---|---|---|
| `api` | gateway + 本单元全部子系统的接口实现 | 处理同步请求（REST、BFF、MCP） | 请求速率、延迟 |
| `consumer` | search、detection、assessment、pursuit、learning、channels、commerce、governance 等的事件消费者 | 事件处理、索引投影、读模型更新、匹配 | 消费延迟 |
| `workflow-worker` | research、learning、customer-context、pursuit、governance、search 等的 Temporal worker | 研究运行、仿真、回填、索引构建、删除编排、推进时限 | 工作流任务队列深度 |
| `outbox-relay` | 全部子系统（含 Python 子系统）的发件箱中继 | 发件箱 → Kafka | 发件箱积压 |
| `scheduler` | 周期性任务（摘要、统计、健康检查） | 定时任务 | 固定副本（主备） |

```yaml
# deploy/platform/roles.yaml（示意）
roles:
  api:
    subsystems: [gateway, identity, commerce, governance, customer_context, detection,
                 research, assessment, pursuit, learning, channels, search, ai_platform, operations]
    expose: [http]
  consumer:
    subsystems: [search, detection, assessment, pursuit, learning, channels, commerce, governance]
    consume: all_registered
  workflow-worker:
    task_queues: [research, learning-simulation, detection-backfill,
                  search-index-build, governance-deletion, pursuit-timers]
```

### 2.4 基础组件的部署约定

组件选型以 [技术选型](08-technology-stack.md) 为准，本节只规定部署方式：

- PostgreSQL：高可用集群 + 只读副本；每个子系统独立逻辑数据库与账号；统计与批量查询走只读副本；Temporal 使用独立集群；
- Kafka、OpenSearch、PostgreSQL 跨可用区部署（见第 7 节）；
- Keycloak、OpenFGA、Temporal 自建高可用；
- 可观测性：每个节点部署 OpenTelemetry Collector。

## 3. 运行时组装

```mermaid
flowchart LR
  subgraph ACQ["acquisition（Python）"]
    S1[acq-scheduler]
    F1[acq-fetcher]
    B1[acq-browser]
    A1[acq-admin]
  end
  subgraph DAT["data（Python）"]
    DA[data-api]
    DC[data-consumer]
    DW[document-worker]
    DWF[data-workflow-worker]
  end
  subgraph PLT["platform（Go）"]
    API[api]
    CON[consumer]
    WW[workflow-worker]
    OR[outbox-relay]
    SCH[scheduler]
  end
  subgraph INFRA["基础组件"]
    PG[(PostgreSQL<br/>每子系统独立库)]
    OBJ[(对象存储)]
    K[[Kafka]]
    T[[Temporal]]
    OS[(OpenSearch)]
    R[(Valkey)]
    KC[Keycloak]
  end
  ACQ --> OBJ & PG
  DAT --> OBJ & PG
  K --> DC & CON
  API -- 远程接口 --> DA
  API -- 内部管理接口 --> A1
  API --> PG & OS & R & KC
  CON --> PG & OS
  WW & DWF & S1 <--> T
  OR --> K
  OR -. 读取各库 outbox .-> PG
  PLT --> OBJ
```

关键设计：

- **子系统在同一进程中也不共享数据库连接与事务**：每个子系统使用自己的数据库账号与连接池；
- **同语言子系统间同步调用经接口包**：同进程时为本地调用，但仍经过上下文传递、授权检查、超时与可观测埋点（与远程调用行为一致）；**跨语言调用一律为远程调用**；
- **事件始终经 Kafka**：即使生产者与消费者在同一进程，也不使用进程内事件总线，保证拆分前后语义一致；
- **Temporal 任务队列按子系统划分**：拆分子系统时只需让新服务监听其任务队列。

## 4. 拆分子系统的流程

拆分是运维操作，不是重构：

1. 在部署清单中为该子系统创建新的部署（复用同一镜像或构建只含该子系统的镜像）；
2. 新部署的 `api` 角色暴露该子系统的远程接口（由契约生成）；
3. 其他进程的接口绑定从本地实现切换为远程客户端（配置项）；
4. 事件消费组与 Temporal 任务队列切换到新部署；
5. 需要独立数据库实例时，通过逻辑复制把该子系统的逻辑数据库迁移到新实例并切换连接；
6. 契约测试与端到端测试验证后，从原单元的角色清单中移除该子系统。

**拆分触发条件**（满足任一即可评估拆分）：

| 触发条件 | 典型候选 |
|---|---|
| 资源特征与其他子系统显著不同，影响共享进程 | `research`（长时、模型密集）、`evidence` 文档处理 |
| 发布节奏需要独立 | `gateway`（外部接口）、信息领域解析器 |
| 安全或合规要求隔离 | `identity`、`governance` |
| 团队边界形成，需要独立交付 | 任何子系统 |
| 单子系统负载需要独立数据库实例 | `domain-intelligence`、`entity-graph`、`detection` |

## 5. 租户单元

```text
                  ┌───────────── 共享 ─────────────┐
                  │ acquisition · 数据供应平面子系统 │
                  │ 平台平面子系统 · gateway          │
                  └──────────────┬──────────────────┘
          ┌──────────────────────┼──────────────────────┐
     Cell-01（共享）        Cell-02（共享）        Cell-E1（专属）
   客户智能平面子系统      客户智能平面子系统      客户智能平面子系统
   租户库 · 租户索引 ·     租户库 · 租户索引 ·     独立实例
   租户主题 · 租户文档     租户主题 · 租户文档
   解析实例               解析实例
```

- 首期只部署一个共享单元 `cell-01`，但单元标识、路由与主题命名从第一天存在；
- `gateway` 根据令牌中的租户查找所属单元并路由；
- 新增单元为部署操作：复制客户智能平面的部署清单与数据存储；
- 租户在单元之间迁移通过租户数据导出导入工作流完成。

## 6. 环境

| 环境 | 用途 | 数据 |
|---|---|---|
| `dev` | 开发者本地与共享开发 | 合成数据与公共样本 |
| `ci` | 持续集成临时环境 | 测试夹具 |
| `staging` | 预发布、契约与端到端测试、性能测试 | 公共域数据副本、合成租户 |
| `prod` | 生产 | 真实数据 |
| `private-<customer>` | 私有部署 | 客户环境 |

本地开发通过容器编排一键启动全部基础组件与三个部署单元，保证开发环境与生产拓扑一致。

## 7. 地域与合规

- 生产部署在中国大陆区域，数据境内存储与处理；
- 至少两个可用区，PostgreSQL、Kafka、OpenSearch 跨可用区部署；
- 私有部署包包含客户智能平面与平台平面，公共域数据以授权方式增量同步。

## 8. 发布

- 基于 Kubernetes，声明式部署（Helm + GitOps）；
- 滚动发布；数据库迁移遵循“扩展—迁移—收缩”，保证新旧版本并存；
- 事件与接口契约的不兼容变更必须提供并行版本；
- 功能开关与租户灰度由 `commerce` 权益与配置控制。
