# 接入网关子系统（gateway）

> 状态：评审中 · 版本：1.6 · 更新：2026-10-26 · 平面：交互

## 1. 使命

为所有外部客户端提供统一、安全、版本化的接入：Web 应用、客户系统、合作伙伴、AI 助手（MCP）和 Webhook 订阅者使用同一套业务服务、权限与审计。

## 2. 职责与非职责

**职责**

- 公共 API：REST/OpenAPI，资源化、版本化；
- BFF（Backend for Frontend）：为 Web 应用聚合和裁剪数据，不包含业务规则；只缓存按调用方裁剪后的响应（键含租户、用户、权利与权益版本），不跨租户复用，缓存命中也现场执行授权、覆盖、权利与配额检查（[安全与租户第3节](../06-security-and-tenancy.md#3-隔离层次)）；
- MCP 服务器：把业务能力以 MCP 工具与资源暴露给 AI 助手，作为 OAuth 资源服务器；
- 出站 Webhook：客户订阅事件、签名、重试、失败告警；
- 认证接入：校验令牌、派生租户与主体、注入请求上下文；M0实现校验issuer/audience、RS256签名、Bearer访问令牌类型与exp/iat/nbf，丢弃外部上下文头；真实HTTPS Keycloak及安全反例见[运行时验收](../../05-delivery/05-m0-runtime-acceptance.md)；
- 租户路由：经identity.SessionResolver的本地/远程绑定核对当前租户、成员与cell-01；未知/停用/退出/错误cell拒绝，目录故障失败关闭；已签名的认证保证（amr=mfa、OIDC auth_time 与会话来源 miaoqia_session_source）仅在进程内传给 identity 判断强认证时效（[identity 第8节](identity.md#8-安全要点)），不接受外部上下文头或MFA头；会话来源为渠道免登（`channel`）的令牌须带 `miaoqia_channel_session`（缺失 401 `channel_session_invalid`），每次请求改经 identity 本地 `ChannelSessionResolver` 核验该免登会话未作废、未过期且成员有效，远程绑定不提供这项核验时失败关闭（[identity 第5节实现状况](identity.md#5-接口)）。见[M0身份验收](../../05-delivery/07-m0-identity-acceptance.md)；
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
| `web_session` | 浏览器会话：OIDC 授权码 + PKCE 登录、服务端保存令牌、会话 Cookie、强认证升级、渠道免登落地与后端通道登出（第11节） |

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

## 10. 对象页与证据面板 BFF

**实现状况（PRD 第4段对象页与证据的服务端切片，契约 [`contracts/apis/gateway/openapi.yaml`](../../../contracts/apis/gateway/openapi.yaml) 0.1.0 草案，事件 [`contracts/events/gateway/events.asyncapi.yaml`](../../../contracts/events/gateway/events.asyncapi.yaml) 0.1.0 草案）**：

- **接口**：`GET /v1/objects/{objectID}`（对象页：对象、口径、信号上下文、分区清单、计量回执与水印）、`GET /v1/objects/{objectID}/sections/{section}`（逐分区返回状态 `ok` / `empty` / `unavailable` / `history_unavailable` / `not_connected`、原因码、口径与内容）与 `GET /v1/evidence/{evidenceID}`（证据面板）。对象标识只接受 `pp_`、`prj_`、`org_`，分区按对象类型固定；
- **检查顺序**：每个请求现场执行，顺序固定：身份（只接受用户主体，经 identity `Authorizer` 判断 `tenant#can_search_public`，拒绝 403，授权服务不可用 503）→ 信号上下文 → 对象解析（按口径）→ 覆盖范围（不足时 403 `entitlement.coverage_insufficient`，只带对象名称）→ 安全上限 → 读取 → 计量。任何一步不通过即拒绝，之后的步骤不执行；拒绝的请求不计量。不缓存响应；
- **信号上下文**：带 `signal_id` 时经 detection 的 `SignalExplainer` 绑定（远程时为内部接口 `GET /internal/v1/detection/signals/{signalID}/explanation`，以用户主体调用，按目标判断 `can_view_signals`）读取固定的求值输入。他租户、不存在与无权查看的信号同样按“不带信号”处理，`signal_time` 退回当前视角，不提示其存在；detection 不可用时当前视角只缺上下文条（`signal_context_unavailable`），`signal_time` 503。`signal_time` 的记录截止时点是信号的 `evaluated_at`（对外精度到秒，本地与远程绑定一致），并固定视图快照中的对象版本；统计分区历史不可用，视图快照已过保存期时依赖快照的分区（概要、标包、登记信息、人员）历史不可用；
- **分页与安全上限**：列表每页至多 20 条，游标为 gateway 签发的不透明值：整个载荷（绑定的租户、调用者、对象、分区、口径与筛选，偏移，数据平面的游标）连同其 HMAC-SHA256 签名一起编码，签名密钥为 `MIAOQIA_GATEWAY_CURSOR_KEY`（base64，至少 32 字节，同一部署的全部 api 实例相同；只有 `MIAOQIA_ENV=development` 可以缺省，此时每个进程随机生成）。签名不符、绑定不一致或位置无效时 400 `invalid_cursor`，在读取与计量之前判断，调用方不能借重置偏移或替换内层游标越过上限；翻到 200 条为止（最后一页标 `capped`），更深的位置 422 `limit.safety_cap`，在读取与计量之前判断；
- **“含我方品类”**：取调用者组织单元的本租户档案品类（customer-context `ContextReader`），只作为请求参数传给数据平面，不写入公共数据；
- **证据**：展示级别取证据及其全部输入中最严格的一档，按最终级别以字段白名单填入内容，不从数据平面结果整体复制：全文含片段、上下文、定位、记录键与原值；摘要只有被引用的片段与定位元数据（定位、记录键、转换说明、快照保存时间、来源），不含上下文与记录原值；仅引用只有来源名称、标题、链接、可达状态与转换说明；不展示只留来源链接；未知级别按不展示；只允许保存元数据的来源在任何级别都不返回正文；证据所属对象同样检查覆盖；
- **计量**：对象页与分区按 `object_view`、证据按 `evidence_view` 经 commerce 的 `ViewMetering` 绑定（远程时为内部接口 `POST /internal/v1/commerce/view-meters`）计量，客户端的 `Idempotency-Key` 作为尝试键（未给出时由网关生成）。额度用完 429 `quota.exhausted`；commerce 不可用时放行（回执 `deferred`）并在本子系统发件箱写入 `gateway.view_meter.deferred.v1`，写入失败则 503，不放行未登记的查看；
- **端口与缺省**：数据平面读取（`api.PublicData`）、证据解析（`api.EvidenceResolver`）与覆盖判断（`api.Entitlements`）没有生产实现，`gateway.Module()` 使用 `api.NotWired`：对象页、分区与证据面板 503，不以空数据冒充没有记录。测试使用 `api/publictest` 的内存夹具，内容全部虚构；
- **未实现**：版本检查（PRD 第4段第10节）、字段级状态与时间轴（由数据平面提供，本切片透传）、清单标记、跟进、报告漏报与报告数据问题、埋点。

## 11. Web 会话与页面聚合（设计）

Web 应用的浏览器端流程与 BFF 契约见 [Web 专项设计](../14-web-application.md)，本节只列 gateway 的职责，尚未实现（[贯通验收线 W-04](../../05-delivery/09-a02-end-to-end-acceptance.md#44-界面w)）：

- **会话**：`web_session` 完成 OIDC 授权码 + PKCE 登录，刷新令牌加密保存在本子系统数据库（接入凭证），浏览器只持有 `__Host-mq_sid` 会话 Cookie；带 Cookie 的请求取出会话的访问令牌交给现有令牌校验，派生的请求上下文与 Bearer 请求相同，之后的租户路由与成员核对不变。免登会话（`channel` 来源）直接引用 identity 的 `chs_`，每次请求经本地 `ResolveChannelSession` 核验，远程绑定时失败关闭（Web 专项设计 WD2，2026-10-26 确认）。同一请求同时带 Cookie 与 `Authorization` 时拒绝；
- **浏览器端点**：`/auth/login`、`/auth/callback`、`/auth/step-up`、`/auth/logout`、`/auth/backchannel-logout`、`/auth/channel/sso`、`/auth/channel/callback`、`/auth/channel/oauth`（[Web 专项设计第7.4节](../14-web-application.md#74-网关-auth-端点)）；带会话 Cookie 的非 GET 浏览器请求要求 CSRF 头与同源 `Origin`；登录、免登与强认证事务以事务 Cookie 绑定发起的浏览器；`/auth/backchannel-logout` 是 IdP 服务器调用，不适用浏览器 CSRF 规则，以完整校验的 `logout_token` 认证（签名、`iss`、`aud`、时间、`events`、`sid`/`sub`、`jti` 防重放）；
- **页面聚合**：`/v1/web/*` 只组合各子系统接口，逐分区返回状态，不缓存，不实现业务规则（[Web 专项设计第7节](../14-web-application.md#7-bff-契约)）；对象页增加版本检查 `GET /v1/objects/{objectID}/versions`；
- **配置**：`MIAOQIA_GATEWAY_SESSION_KEY`（刷新令牌加密密钥，非开发环境缺省不启动）、Keycloak 保密客户端 `miaoqia-web` 的凭证与回调地址。
