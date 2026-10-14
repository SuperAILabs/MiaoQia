# 集成契约

> 状态：评审中 · 版本：1.0 · 更新：2026-10-12 · 负责人：首席架构师

契约是子系统之间唯一的耦合点。所有契约以机器可读文件存放在仓库的 `contracts/` 目录，是代码生成、文档生成和契约测试的唯一来源。

## 1. 契约类型

| 类型 | 格式 | 目录 | 用途 |
|---|---|---|---|
| 领域对象 Schema | JSON Schema 2020-12 | `contracts/schemas/` | 领域事实、租户对象、研究产物的结构 |
| 子系统接口 | OpenAPI 3.1 | `contracts/apis/<subsystem>/` | 命令与查询接口 |
| 事件 | AsyncAPI 3 + JSON Schema | `contracts/events/<namespace>/` | 领域事件与集成事件 |
| 策略 DSL | JSON Schema + CEL 环境与函数声明 | `contracts/policy/` | 策略结构、可用变量与函数，见 [策略 DSL](11-policy-dsl.md) |
| 扩展包清单 | JSON Schema | `contracts/packs/` | 扩展包 manifest 与定义文件 |
| 标识前缀 | YAML | `contracts/identifiers.yaml` | 前缀登记 |

## 2. 子系统接口

### 2.1 进程内与远程统一

每个子系统在代码中公开一个 `api` 包（命令、查询及其数据传输对象，见 [代码库结构](09-codebase-structure.md)）。调用方只依赖该接口：

- 同语言、同一进程部署时，接口由本地实现直接提供；
- 独立部署或跨语言调用时，接口由生成的远程客户端（HTTP）提供；
- 两种实现通过同一套契约测试。

因此拆分子系统只改变部署配置，不改变调用方代码。见 [部署拓扑](07-deployment-topology.md)。

### 2.2 约定

| 方面 | 约定 |
|---|---|
| 风格 | 面向资源的查询 + 显式命名的命令（`POST /…:activate` 风格） |
| 版本 | 路径主版本（`/v1/`）；新增字段为兼容变更；删除或语义变化必须新主版本 |
| 错误 | RFC 9457 Problem Details，带稳定错误代码 |
| 分页 | 游标分页 |
| 幂等 | 所有写操作接受 `Idempotency-Key`；子系统保证同键同结果 |
| 异步 | 长操作返回 `202` 与作业标识，作业统一查询 |
| 上下文 | 请求上下文（租户、主体、用途、追踪标识、预算）由框架传递，不出现在业务参数中：对外请求由 `gateway` 从令牌派生；内部调用经 `Miaoqia-Context` 头与 `traceparent` 头传递，只在内部监听端口接受 |
| 字段白名单 | 出入站 Schema 默认 `additionalProperties: false`；扩展只能进入命名空间化的 `extensions`，并按下游逐一净化 |
| 超时 | 每个接口声明超时与重试安全性 |

### 2.3 跨子系统一致性

- 一个请求内不跨子系统共享数据库事务（即使在同一进程中）；
- 需要跨子系统协调的业务以事件驱动或 Temporal 工作流（Saga）实现，带补偿；
- 查询其他子系统数据时，接受最终一致；关键决策前回到所有者查询权威版本。

## 3. 事件

### 3.1 事件信封

采用 CloudEvents 1.0 结构：

```json
{
  "specversion": "1.0",
  "id": "evt_01JA…",
  "source": "miaoqia/domain-intelligence",
  "type": "tendering.result.published.v1",
  "time": "2026-10-12T08:00:00Z",
  "subject": "pp_01JA…",
  "datacontenttype": "application/json",
  "dataschema": "contracts/events/tendering/result.published.v1.json",
  "miaoqiazone": "public",
  "miaoqiatenant": null,
  "miaoqiacell": null,
  "traceparent": "00-…",
  "data": { }
}
```

租户事件必须带 `miaoqiatenant` 与 `miaoqiacell`；公共域事件必须为空。

### 3.2 命名

`<namespace>.<entity>.<verb_past_tense>.v<major>`

| 命名空间 | 所属 |
|---|---|
| `acquisition`、`evidence`、`domain`、`entity`、`search` | 数据供应平面子系统 |
| `customer_context`、`detection`、`research`、`assessment`、`pursuit`、`learning` | 客户智能平面子系统 |
| `gateway`、`channels` | 交互平面子系统 |
| `identity`、`commerce`、`governance`、`ai_platform`、`operations` | 平台平面子系统 |
| `audit` | 审计事件流：全部子系统经审计 SDK 发布到 `platform.audit`，由 `governance` 消费 |
| `<domain_pack>`（如 `tendering`） | 信息领域包定义的领域事件，由 `domain-intelligence` 发布 |

### 3.3 主题与分区

| 主题 | 内容 | 分区键 |
|---|---|---|
| `public.<namespace>` | 公共域事件 | 聚合标识（保证同一对象有序） |
| `cell-<id>.<namespace>` | 租户域事件，按租户单元隔离 | 租户标识 + 聚合标识 |
| `platform.<namespace>` | 平台域事件 | 租户标识 |

### 3.4 发布与消费规则

- **发布**：业务写入与发件箱记录同一事务提交，由中继发布到 Kafka；不允许在事务外直接发送（发件箱与消费去重表定义见 `contracts/storage/kernel.sql`）；
- **消费**：消费者必须幂等（以事件 `id` 去重或以业务版本号判断）；
- **顺序**：只保证同一分区键内有序；消费者不得假设跨对象顺序；
- **重放**：所有主题保留足够时间以支持读模型重建；公共域领域事件长期保留（或可从权威数据重新生成）；
- **校验**：消费者校验信封与主题一致（公共主题只接受公共域事件，单元主题只接受该单元的租户事件），不符直接进入死信；
- **失败**：重试带退避，超过上限进入死信主题 `dlq.<子系统>` 并告警；死信可人工重放；
- **事件载荷**：携带足以让多数消费者无需回查的数据，但不携带大对象（用引用）和敏感原文。

### 3.5 Schema 演进

| 变更 | 是否兼容 | 做法 |
|---|---|---|
| 新增可选字段 | 兼容 | 次版本，无需新事件类型 |
| 新增枚举值 | 有条件兼容 | 消费者必须容忍未知枚举 |
| 删除字段、改变语义、改为必填 | 不兼容 | 新主版本事件类型（`.v2`），新旧并行发布一个过渡期 |

持续集成中对契约变更执行兼容性检查（`mqc compat`，规则见 [代码库结构](09-codebase-structure.md#5-代码生成)）。

## 4. 持久化工作流

- Temporal 用于需要持久化状态、定时、重试和人工中断的长流程：研究运行、策略仿真、回填、重放、删除编排、推进时限；
- 工作流按子系统划分任务队列（如 `research`、`learning-simulation`），只能调用本子系统的活动；需要其他子系统时，通过其公开接口；
- 工作流定义版本化，确保运行中的工作流在代码升级后可确定性重放。

## 5. 外部契约

| 契约 | 规范 |
|---|---|
| 公共 API | 由 `gateway` 发布的 OpenAPI，独立版本与弃用策略（至少提前 6 个月通知） |
| Webhook | 事件白名单、签名、重试与投递日志 |
| MCP | 工具与资源清单版本化；作用域与 OAuth 授权见 [gateway](subsystems/gateway.md) |

## 6. 契约测试

- 每个子系统接口有提供方契约测试，本地实现与远程客户端都必须通过；
- 每个事件消费者对其消费的事件 Schema 有消费方测试；
- 安全负向测试：在所有合法字段之外加入未知键、其他租户标识、密钥形态值等，验证被拒绝或净化，见 [研发规范](../05-delivery/03-engineering-standards.md)。
