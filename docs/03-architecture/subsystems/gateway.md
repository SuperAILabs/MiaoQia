# 接入网关子系统（gateway）

> 状态：评审中 · 版本：1.5 · 更新：2026-10-19 · 平面：交互

## 1. 使命

为所有外部客户端提供统一、安全、版本化的接入：Web 应用、客户系统、合作伙伴、AI 助手（MCP）和 Webhook 订阅者使用同一套业务服务、权限与审计。

## 2. 职责与非职责

**职责**

- 公共 API：REST/OpenAPI，资源化、版本化；
- BFF（Backend for Frontend）：为 Web 应用聚合和裁剪数据，不包含业务规则；只缓存按调用方裁剪后的响应（键含租户、用户、权利与权益版本），不跨租户复用，缓存命中也现场执行授权、覆盖、权利与配额检查（[安全与租户第3节](../06-security-and-tenancy.md#3-隔离层次)）；
- MCP 服务器：把业务能力以 MCP 工具与资源暴露给 AI 助手，作为 OAuth 资源服务器；
- 出站 Webhook：客户订阅事件、签名、重试、失败告警；
- 认证接入：校验令牌、派生租户与主体、注入请求上下文；M0实现校验issuer/audience、RS256签名、Bearer访问令牌类型与exp/iat/nbf，丢弃外部上下文头；真实HTTPS Keycloak及安全反例见[运行时验收](../../05-delivery/05-m0-runtime-acceptance.md)；
- 租户路由：经identity.SessionResolver的本地/远程绑定核对当前租户、成员与cell-01；未知/停用/退出/错误cell拒绝，目录故障失败关闭；已签名的认证保证（amr=mfa、OIDC auth_time 与会话来源 miaoqia_session_source）仅在进程内传给 identity 判断强认证时效（[identity 第8节](identity.md#8-安全要点)），不接受外部上下文头或MFA头。见[M0身份验收](../../05-delivery/07-m0-identity-acceptance.md)；
- 流量治理：限流、配额前置检查（向 `commerce` 查询）、请求大小与超时；
- 幂等：写操作的 `Idempotency-Key` 管理；
- 异步作业：长任务返回作业标识，统一查询接口。

**非职责**

- 不实现业务规则，不直接访问业务数据库；
- 不保存业务数据（只保存接入凭证、Webhook 订阅与幂等记录）。

## 3. 内部组件

| 组件 | 说明 |
|---|---|
| `public_api` | 对外 REST 资源路由到各子系统接口 |
| `bff` | Web 应用的聚合查询 |
| `mcp_server` | MCP 工具、资源、提示模板 |
| `webhooks` | 出站订阅与投递 |
| `edge` | 认证、租户上下文、限流、幂等、审计埋点 |
| `jobs` | 异步作业查询 |

## 4. MCP 能力设计

工具按读写与风险拆分，作用域最小化：

| 工具 | 作用 | 作用域 | 控制 |
|---|---|---|---|
| `search_subjects` | 检索租户可见的领域对象与实体 | `subjects:read` | 服务端从令牌派生租户 |
| `get_subject` | 领域事实、版本、证据 | `subjects:read` | 服务端从令牌派生租户 |
| `list_signals` | 信号收件箱 | `signals:read` | — |
| `create_watch` / `remove_watch` / `list_watch_updates` | 关注对象或保存检索的订阅、变化记录（[detection 第 11 节](detection.md#11-关注)） | `watches:write` / `watches:read` | 以令牌主体为所有者 |
| `start_research` | 发起研究 | `research:write` | 异步、预算、模板白名单 |
| `get_research` | 研究进展与报告 | `research:read` | 区分事实、推断、未知项 |
| `assess_subject` | 生成判断 | `assessments:write` | 只生成判断与草案 |
| `record_feedback` | 反馈 | `feedback:write` | 幂等、有限枚举 |
| `draft_policy_change` | 策略草案 | `policies:propose` | 只生成草案 |
| `simulate_policy` | 策略仿真 | `policies:simulate` | 异步、配额 |
| `request_policy_activation` | 提交激活审批 | `policies:activate:request` | 不直接激活 |
| `create_pursuit` / `update_pursuit` | 推进 | `pursuits:write` | 重复检测 |
| `draft_decision` | 决策草案 | `decisions:draft` | 不自动批准 |

资源：`subject://{type}/{id}`、`signal://{id}`、`research://{id}`、`assessment://{id}`、`pursuit://{id}`、`job://{id}`。

Skills 是组合这些工具的工作流说明（如“每日信号评审”“投标决策”“供货线索跟进”），不保存密钥、不复制业务规则，版本与 MCP 能力清单保持兼容。

## 5. 接口约定

见 [集成契约](../03-integration-contracts.md)：资源命名、分页、错误格式（RFC 9457）、版本策略、幂等、异步作业、字段白名单。

## 6. 事件

**消费**：所有可被客户订阅的租户事件（经白名单与净化）用于 Webhook 投递。

**发布**：`gateway.webhook.delivery_failed.v1`。

## 7. 扩展点

- 应用领域包可注册领域资源的 API 视图与 MCP 工具（基于平台已有接口组合，不允许直接访问数据）；
- 合作伙伴嵌入的品牌与域名配置。

## 8. 伸缩与可靠性

- 无状态，水平伸缩；
- 下游子系统故障时按接口返回明确错误，不级联阻塞；
- Webhook 投递有重试、退避、死信与客户可见的投递日志。

## 9. 安全要点

- 租户身份只来自已验证令牌，请求体与工具参数中的租户字段一律忽略或拒绝；
- MCP 客户端通过 OAuth 授权，令牌受众绑定本资源服务器；
- 所有出站数据经字段白名单净化，不透传内部元数据；
- Webhook 签名（HMAC）与目标地址校验，防止 SSRF；
- 执行公共数据暴露策略：结果上限、翻页深度、配额、水印、异常访问检测，见 [安全与租户](../06-security-and-tenancy.md#7-公共数据暴露控制)。
