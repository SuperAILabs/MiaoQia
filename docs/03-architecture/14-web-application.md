# Web 与 H5 专项设计

> 状态：评审中 · 版本：1.1 · 更新：2026-10-26 · 负责人：首席架构师、产品负责人、设计负责人、安全负责人 · 原型：[prototypes/00-web-shell.html](../01-product/prd/prototypes/00-web-shell.html)

本文是[阶段基线 M2-P12](../05-delivery/06-stage-baseline.md#4-m2-前置条件与能力交接)要求的 Web 专项设计，对应 [A02 真实贯通验收线](../05-delivery/09-a02-end-to-end-acceptance.md#44-界面w)缺口 W-01（第11节待决事项确认后关闭）：补齐浏览器端流程（登录、强认证、渠道免登与卡片落地）、具体 BFF 契约与保存视图的所有者，并把七段 PRD 中标为“随 Web”的行为落到页面、组件与接口上，为 L2 界面贯通的实现做准备。

各段的业务规则、状态与文案以[功能 PRD](../01-product/prd/README.md) 为准，本文不改写；接口语义以子系统规格为准，本文只规定浏览器如何使用它们，以及 gateway 需要新增的聚合与会话能力。产品层的交互范式与主导航见[产品体验](../01-product/04-product-experience.md)。按项目负责人 2026-10-15 的设计原则，信息架构与原型展示完整版本（全部模块与客户可能追求的目标），不受 M2 发布范围限制；实现批次在第10节单独给出。

## 1. 范围与结论

| 项 | 结论 | 详见 |
|---|---|---|
| 应用形态 | 一个响应式单页应用 `web/apps/workspace`，桌面与 H5（含企业微信内置浏览器）共用；与 API 同源部署 | 第2节 |
| 信息架构 | 完整主导航九个模块加全局“待我处理”与“问妙洽”；M2 开放模块可用，其余显示开放批次 | 第3节 |
| 浏览器会话 | gateway 持有令牌的服务端会话（BFF 令牌处理模式），浏览器只持有 `HttpOnly` 会话 Cookie，不接触访问令牌 | 第4节 |
| 强认证 | 动作被 `mfa_required` 拒绝时保留操作与已填内容，经 IdP 重新认证后继续；桌面用弹出窗口，H5 与内置浏览器用整页跳转 | 第4.4节 |
| 渠道免登 | gateway 会话直接引用 identity 的免登会话 `chs_`，每次请求经 `ResolveChannelSession` 核验，IdP 不再为免登铸造令牌 | 第4.5节 |
| 交互通用规则 | 九种必备状态的统一组件、错误码到文案的目录、写操作的操作标识与幂等键协议、时间与金额显示 | 第5节 |
| 统一列表与保存视图 | 统一列表组件；保存视图由 customer-context 拥有，只保存条件不保存结果，打开时按查看者权限执行 | 第6节 |
| BFF 契约 | 单一子系统的资源由浏览器经网关直接调用各子系统公开接口；跨子系统的页面聚合由 gateway BFF 的 14 个 `/v1/web/*` 端点提供，另加对象页版本检查 | 第7节 |
| 页面清单 | 17 组页面，逐一对应 PRD 段落、数据来源与“随 Web”行为 | 第8节 |
| 安全 | Cookie、CSRF、跳转白名单、CSP、原文渲染、水印与浏览器侧负向用例 | 第9节 |
| 实现批次 | L1 端到端测试通过后，按四个环节实现最小页面集（W-02、W-03），之后补齐第6段与工作台能力 | 第10节 |

## 2. 应用形态与工程结构

### 2.1 一个应用，三档布局

- **单一应用**：`web/apps/workspace`（TypeScript + React + Vite + Ant Design，[技术栈 2.3](08-technology-stack.md#23-前端)）同时服务桌面浏览器、手机浏览器与企业微信、飞书、钉钉内置浏览器。H5 不是另一个应用，而是同一路由在窄屏下的布局；
- **三档布局**：宽 ≥ 1200 像素为三栏（左导航、列表、右侧抽屉或面板）；768–1199 为两栏（导航收起为图标栏，抽屉覆盖列表）；< 768 为 H5 单栏（顶部目标下拉与底部导航，抽屉与面板改为全屏新页面）。各段 PRD 要求的 375 像素宽度下的关键任务都以 H5 档验收；
- **内置浏览器识别**：只用于选择渠道免登与绑定流程（第4.5、4.6节），按渠道网页授权能否完成判断，不以 User-Agent 作为任何授权依据。

### 2.2 同源部署

- 应用静态资源与 API 使用同一个源（例如 `https://app.<域名>`）：`/` 下是应用，`/v1/` 是网关对外接口，`/auth/` 是网关的浏览器会话端点（第4节），`/c/` 与 `/bind/` 是渠道落地路由。同源使会话 Cookie 是第一方 Cookie，并允许 `SameSite=Lax` 与严格的内容安全策略；
- 静态资源由 `api` 进程角色之前的反向代理（开发 VPS 上同一台 Caddy/Nginx）提供，带内容哈希文件名与长缓存；`index.html` 不缓存。应用不在构建产物中嵌入任何环境密钥；
- 开发环境：Vite 开发服务器把 `/v1/`、`/auth/` 代理到本地 `api` 角色，测试签发方（`services/internal/testissuer`）代替 Keycloak。

### 2.3 工作区包

| 包 | 内容 | 生成或手写 |
|---|---|---|
| `web/packages/contracts` | 由 `contracts/schemas` 生成的类型（现有） | 生成，`mqc generate --check` 校验 |
| `web/packages/api` | 由 `contracts/apis/*/openapi.yaml` 生成的类型与客户端，统一处理 Problem Details、幂等键、操作标识与重试（第5.3节） | 生成 + 少量手写运行时 |
| `web/packages/ui` | 妙洽设计令牌（基于 Ant Design 主题）、九种状态组件、字段状态、证据角标、统一列表组件（第6节） | 手写 |
| `web/packages/copy` | 错误码与原因码到中文文案和下一步的目录（第5.2节），各段 PRD 的固定文案 | 手写，带覆盖检查 |
| `web/apps/workspace` | 路由、页面与页面级状态 | 手写 |

API 客户端只从 OpenAPI 生成，不手写请求结构；`copy` 的覆盖检查在 CI 中核对：所有 OpenAPI 中声明的错误码在目录中都有文案，目录中没有未声明的码。

## 3. 信息架构

### 3.1 主导航（完整版本）

在[产品体验第3.1节](../01-product/04-product-experience.md#31-主导航)的九个模块之上，增加全局的“待我处理”。M2 开放的模块可用；其他模块在导航中可见、标明开放批次、不可进入（第11节 WD6），让客户看到完整的产品版图，与第2段目标目录列出后续目标的做法一致。

| 导航 | 内容 | 开放批次 | 主要来源 |
|---|---|---|---|
| 今日简报 | 简报历史、完整简报、投递记录 | M2（第5段） | channels |
| 信号 | 按目标的收件箱、详情、批量反馈、反馈汇总、我的漏报 | M2（第3段） | detection、learning |
| 检索 | 公告、项目、企业的受控检索，保存检索与订阅 | M3 | search、domain-intelligence |
| 关注 | 关注对象与变化时间线 | M3（随关注功能） | detection |
| 研究 | 研究案例、报告库、追问 | M3 | research |
| 跟进 | 我负责的、我协作的、团队的、已关闭；持续跟踪 | M2 最小切片（第6段）；看板与阶段随 M4 | pursuit |
| 决策 | 待审批与决策记录 | M4 | assessment |
| 设置 | 客户档案、目标、策略、成员与邀请、渠道；个人设置 | M2（第1、2、7段） | customer-context、identity、channels |
| 待我处理（全局，顶栏） | 待我审批、分派给我且即将到期的任务 | M2（第5–7段） | identity、pursuit |
| 问妙洽（全局，顶栏） | 自然语言提问与研究入口 | M3 开放，M4 完整 | research |

对象页不在导航中，从信号、跟进、检索与对象页之间的链接进入，URL 可直接打开。

### 3.2 路由

| 路由 | 页面（第8节编号） | 说明 |
|---|---|---|
| `/start` | P02 开始设置 | 激活后与管理员首页 |
| `/invite/:token` | P03 接受邀请 | 未登录可打开预览 |
| `/briefings`、`/briefings/:id` | P15 | 今日简报与历史 |
| `/signals/:objectiveId?tab=&…` | P13 | 收件箱；筛选与排序写入查询参数 |
| `/signals/:objectiveId/s/:signalId` | P13 详情 | 桌面为抽屉，H5 为新页面 |
| `/signals/:objectiveId/feedback`、`/signals/misses` | P13 反馈汇总、我的漏报 | |
| `/objects/:objectId?signal=&view=` | P14 | `view=current` 或 `signal_time`；`pp_`、`prj_`、`org_` |
| `/evidence/:evidenceId` | P14 证据面板（H5 全屏） | 桌面在对象页右侧打开 |
| `/pursuits`、`/pursuits/:id`、`/tracking/:id` | P16 | 第6段 |
| `/inbox` | P12 待我处理 | 审批与任务 |
| `/approvals/:id` | P12 审批页 | 进入前检查强认证 |
| `/settings/profile/*` | P09 客户档案 | 总览、本企业、品类区域、品牌、资质、企业清单 |
| `/settings/objectives`、`/settings/objectives/:id/*` | P10、P11 | 目标、策略、仿真、申请、版本历史 |
| `/settings/members`、`/settings/channels` | P04、P05 | 管理员 |
| `/me/channels`、`/me/briefing` | P06 | 个人设置 |
| `/c/:link` | P08 卡片落地 | 渠道卡片链接 |
| `/bind/:token` | P07 绑定确认 | 只在渠道内置浏览器完成 |
| `/unavailable` | P08 通用页 | “无法打开此内容” |
| `/search`、`/watch`、`/research`、`/decisions`、`/ask` | P17 未开放模块 | 显示开放批次 |

## 4. 浏览器会话与认证

### 4.1 会话模型：gateway 持有令牌

浏览器端不保存访问令牌或刷新令牌，由 gateway 的新组件 `web_session` 完成 OIDC 授权码流程并在服务端保存令牌（第11节 WD1）：

- **会话 Cookie**：`__Host-mq_sid`，`Secure`、`HttpOnly`、`SameSite=Lax`、`Path=/`，值为 32 字节随机数；gateway 只保存其 SHA-256 摘要。登录、强认证升级与会话来源变化时轮换 Cookie 值（防会话固定）；
- **会话记录**（gateway 自有数据库 `gwy`，接入凭证类数据，符合 gateway“只保存接入凭证”的职责）：会话标识摘要、租户、主体、会话来源（`password` / `channel`）、IdP 会话标识 `sid`、加密保存的刷新令牌（AES-256-GCM，密钥 `MIAOQIA_GATEWAY_SESSION_KEY`，与游标密钥同样在非开发环境缺省即不启动）、免登会话标识 `chs_`（只在 `channel` 来源）、创建时间、最近使用时间、空闲与绝对到期时间。访问令牌只缓存在进程内存，按到期前 60 秒刷新；
- **到期**：交互登录会话空闲 12 小时、绝对 7 天（第1段第10节）；免登会话的绝对到期取 identity 免登会话的到期（12 小时）。刷新令牌沿用原 `auth_time`，不延长强认证窗口；
- **请求认证**：带会话 Cookie 的请求，`web_session` 取出会话：`password` 来源取（必要时刷新）访问令牌，交给现有的令牌校验（issuer、audience、RS256、`exp`/`iat`/`nbf`、`amr`、`auth_time`），派生的请求上下文与 Bearer 请求完全相同；`channel` 来源调用 identity `ResolveChannelSession` 核验 `chs_`（第4.5节）。之后的成员、租户与 cell 核对沿用 [gateway 第2节](subsystems/gateway.md#2-职责与非职责)的租户路由，不放宽任何一项；
- **两种凭证互斥**：同一请求同时带 Cookie 与 `Authorization` 时 400 `ambiguous_credentials`。`Authorization: Bearer` 继续服务 API 客户端、MCP 与测试，不受本节影响；
- **IdP 侧退出**：gateway 注册 Keycloak 的后端通道登出（back-channel logout），IdP 会话结束（管理员重置、账号恢复、IdP 侧登出）时删除该 `sid` 的全部 Web 会话；刷新令牌被拒时会话立即作废，返回 401 `session_expired`。后端通道登出是 IdP 服务器对 gateway 的调用，不带 Cookie，也不适用浏览器的 CSRF 规则，以登出令牌本身认证（第4.2节）。

### 4.2 登录与退出

```text
应用 /login?return_to=/signals/obj_…
  → GET /auth/login?return_to=…            gateway 生成 state、nonce、PKCE(S256) 校验码，写入短时登录事务（10 分钟，单次），
                                            并设置事务 Cookie __Host-mq_tx（随机 32 字节，Secure、HttpOnly、SameSite=Lax、10 分钟）
  → 302 Keycloak 授权端点                   保密客户端 miaoqia-web，scope=openid，登录、TOTP、找回密码都在 IdP 页面完成
  → GET /auth/callback?code&state          先按事务 Cookie 取登录事务并核对 state，再换取令牌、校验 ID 令牌 nonce 与 sid，
                                            建立会话、设置会话 Cookie 并清除事务 Cookie
  → 302 return_to                           只接受同源相对路径且命中路由白名单，否则回到 /
```

- **事务绑定发起的浏览器**：登录事务以事务 Cookie 的摘要为键保存，state、PKCE 校验码与 nonce 只能从这个键取出。state 单次有效并不能防止“攻击者在自己的浏览器发起登录、认证为自己，再让受害者的浏览器打开尚未消费的回调地址”从而把攻击者的会话装进受害者浏览器（登录 CSRF）；绑定之后，回调浏览器没有对应的事务 Cookie、或事务 Cookie 与 state 不属于同一事务时一律拒绝（400 `login_transaction_mismatch`），不换取令牌、不设置会话，该事务作废。渠道免登（第4.5节）、绑定的网页授权（第4.6节）与强认证（第4.4节）的事务使用同一机制；强认证事务还绑定发起时的会话、主体与操作标识；
- **登录页由 IdP 提供**：账号密码、TOTP、找回密码、激活设置密码都使用 Keycloak 的托管页面（妙洽主题），应用不收集密码，不使用资源所有者密码模式（第11节 WD5）。连续失败锁定（5 次锁 15 分钟）由 IdP 执行；
- **退出**：`POST /auth/logout`（需 CSRF 头，第9节）删除会话、调用 IdP 结束会话端点，应用通过 `BroadcastChannel` 通知同一浏览器的其他标签页回到登录页并清除 `sessionStorage` 中的待续操作；
- **后端通道登出**：`POST /auth/backchannel-logout` 只接受 IdP 服务器按 OIDC 后端通道登出规范发来的 `logout_token`，不要求、也不接受浏览器 Cookie 与 CSRF 头。校验：签名来自配置的 Keycloak issuer 的公钥（RS256），`iss` 等于该 issuer，`aud` 含 `miaoqia-web`，`iat` 不早于 2 分钟前且不晚于 5 秒后、有 `exp` 的未过期，`events` 含 `http://schemas.openid.net/event/backchannel-logout`，含 `sid` 或 `sub`（至少一个），不含 `nonce`；`jti` 在 10 分钟内只接受一次（重放 400）。通过后删除该 `sid`（或该 `sub` 在本 issuer 下）的全部 Web 会话，返回 200；任何一项不通过 400，不删除会话。该端点只在网关对 IdP 网络可达的地址上开放；
- **会话失效的页面行为**：任何接口返回 401 `session_expired`、`unauthenticated` 时，应用把当前路由作为 `return_to` 跳转登录；页面上未提交的表单按第4.4节的待续操作保存，登录后恢复，但不自动提交；403 `invalid_tenant_context`（成员已停用或退出）显示“账号已停用或已退出该企业”，不重试。

### 4.3 激活与接受邀请

- **首位管理员激活**（第1段 F1）：激活邮件链接是 IdP 的操作令牌（设置密码 + 登记 TOTP 两个必需动作），完成后 IdP 跳回 `/auth/login`，登录后进入 `/start`；
- **接受邀请**（第1段 F2）：`/invite/:token` 先调用 `POST /v1/web/invitations:preview`（未认证端点，第7.3节），只返回租户名称、组织单元、角色与打码的邮箱，令牌不存在、已撤回、已过期或已被重发替换时分别返回原因码，页面按原因显示（不显示租户名称）。随后：
  - 已有账号：跳转登录（`login_hint` 为邀请邮箱），回到本页后由用户点“接受”，调用 identity `AcceptInvitation`；含管理员角色时若被 403 `totp_enrollment_required` / `mfa_required` 拒绝，按第4.4节先登记或验证 TOTP 再继续；
  - 没有账号：调用 `POST /v1/web/invitations:prepare-account`，由 identity 经 IdP 管理接口创建账号（邮箱已验证由邀请链接证明）并返回 IdP 设置密码的操作令牌地址，设置完成后回到本页继续接受。这一步依赖 C-03 的 IdP 管理接口，登记为新增的 identity 接口（第10.2节 W-05）；
  - 接受成功后若 IdP 中的租户归属尚未写入，页面显示“正在开通访问，请稍后重新登录”，按退避每 10 秒查询一次会话状态，最多 5 分钟。

### 4.4 强认证与待续操作

强认证动作（第1段 F3）由服务端判断：业务接口返回 403 `mfa_required` 或 `totp_enrollment_required`。应用不自行判断是否满足，只用会话中的 `strong_auth_valid_until`（第7.2节）提前提示：

1. **保存待续操作**：被拒的写请求连同它的操作标识、表单内容与目标路由保存为“待续操作”（内存，以及 `sessionStorage` 的 `mq.pending.<操作标识>`，只在本标签页存在，退出或 30 分钟后清除）。被拒是操作开始前的拒绝，操作标识上没有固定任何结果（第3段 F9），继续时用新的幂等键、同一操作标识；
2. **弹框说明**：“这一步需要验证身份（15 分钟内有效）”；`totp_enrollment_required` 时说明“首次执行需要先登记验证器”。取消则原动作不执行、已填内容保留；
3. **重新认证**：`GET /auth/step-up?op=…` 以 `prompt=login`、`max_age=0` 请求 IdP 交互认证（未登记时附 IdP 的“登记 TOTP”必需动作）；强认证事务按第4.2节绑定发起的浏览器，并绑定当前会话、主体与操作标识，回调时三者任一不符即拒绝。回调后更新会话的令牌与 `auth_time`（Cookie 轮换），并核对新令牌的主体与原会话相同，不同则拒绝并要求退出（防止借强认证切换账号）；
4. **继续原动作**（第11节 WD3）：
   - 桌面浏览器：认证在弹出窗口中完成，窗口经 `postMessage`（只接受同源消息）通知原页面，原页面自动以同一操作标识重新提交——用户已经点过最终的提交按钮，意图明确；
   - H5 与渠道内置浏览器（弹出窗口不可用）：整页跳转到 IdP，返回后从待续操作恢复表单与确认页，用户再点一次“继续提交”。整页跳转后不自动提交，避免把一次页面加载变成写操作；
5. **免登会话**：免登会话不带强认证。强认证时用户以账号密码与 TOTP 交互登录，会话来源由 `channel` 升级为 `password`（同一主体），之后按交互会话的规则到期。

进入审批页（P12）时若 `strong_auth_valid_until` 已过，页面先显示审批内容与“验证后可以批准或驳回”，不阻止阅读。

### 4.5 渠道免登与卡片落地

卡片链接只指向对象，不携带登录身份（第1段 F7、第5段 F6）。路由 `/c/:link` 的处理：

```text
企业微信内置浏览器打开 /c/lnk_…
  ├─ 已有本人会话 ───────────────────────────────► POST /v1/channels/card-links:open
  └─ 无会话：GET /auth/channel/sso?return_to=/c/lnk_…
       → 设置事务 Cookie（第4.2节）→ 302 渠道网页授权（静默）→ GET /auth/channel/callback?code&state（state 须属于本浏览器的事务）
       → channels ChannelSSO（核对绑定、签发 30 秒凭据）→ identity IssueChannelSession（chs_，12 小时）
       → gateway 建立 channel 来源的会话（引用 chs_），设置 Cookie → 302 /c/lnk_…
  card-links:open → 200 CardTarget → 应用跳转到信号详情、对象页（带 signal）、完整简报或审批
                  → 404 content_unavailable → /unavailable（通用页，不显示标题、归属、负责人）
                  → 409 account_mismatch → “链接是发给 A 的，当前登录的是 B”，提供“退出并以 A 登录”
                  → 410 card_link_expired → “链接已过期”与“登录后打开”
```

- **会话直接引用 `chs_`**（第11节 WD2）：gateway 不再需要 IdP 按免登会话铸造令牌。`channel` 来源的会话每次请求都经 identity 本地 `ResolveChannelSession` 核验会话存在、属于该租户与主体、未作废、版本高于撤销水位、来自成员最近一次退出之后、未过期（[identity 第5节实现状况](subsystems/identity.md#5-接口)），与现有对 `miaoqia_channel_session` 令牌的核验完全相同；identity 拆为远程绑定时失败关闭（503）。解绑与撤销的语义不变：撤销作废 `chs_` 后，下一次请求即 401 `channel_session_revoked`，应用转登录页并说明“企业微信账号已解绑或失效”；
- **普通浏览器**：无会话时跳转登录，`return_to` 为 `/c/:link`；
- **免登失败**：`/auth/channel/callback` 把 channels 与 identity 返回的原因码（`channel_oauth_unavailable`、`not_bound`、`binding_inactive`、`member_inactive`、`identity_unavailable` 等）放在登录页的查询参数中，登录页按文案目录说明原因，不把“不可用”说成“未绑定”；
- **链接令牌不留痕**：落地后立即以 `history.replaceState` 把地址替换为目标对象的路由，页面设置 `Referrer-Policy: no-referrer`，链接令牌不进入浏览器历史以外的任何位置；
- **计量**：打开对象页按第4段计入查看计量，卡片落地本身不计量。

### 4.6 渠道绑定确认（H5）

`/bind/:token` 只在渠道内置浏览器中完成（第1段 F5）。channels 的 `bindings:prepare` 与 `bindings:confirm` 都要求渠道网页授权码与带 `sid` 的交互登录（[channels 实现状况](subsystems/channels.md)）：

1. 打开页面：没有交互登录的会话（无会话或只有免登会话）时先登录，登录在同一内置浏览器中完成；
2. 经 `/auth/channel/oauth?purpose=bind&return_to=…` 静默取得渠道网页授权码，gateway 把授权码只交给这一次 `prepare` 调用（授权码一次性，不经浏览器脚本）；
3. 页面显示确认内容“企业微信账号（显示名、所属企业）将绑定到 妙洽账号（姓名、租户）”；
4. 点“确认”时再静默取一次授权码并调用 `confirm`，原样提交 `prepare` 返回的内容摘要；
5. 失败按 PRD 第1段第7节的“绑定确认”各状态显示；在普通浏览器中打开时只显示“请在企业微信中打开”。

## 5. 交互通用规则

### 5.1 九种必备状态

每个页面按所在 PRD 第7节的状态表实现九种状态；不适用的状态以 PRD 的说明为准。`web/packages/ui` 提供统一组件，页面不得自行拼写这些状态：

| 状态 | 组件 | 规则 |
|---|---|---|
| 正常 | — | — |
| 空态 | `EmptyState(reason)` | 区分“无结果 / 筛选无结果 / 来源中断 / 配置未启用 / 无权限”（[产品需求落地规格第2节](../01-product/05-product-requirements.md#2-统一业务与交互规则)），每种带下一步 |
| 加载 | `Skeleton` | 首屏 300 毫秒内不出现骨架，避免闪烁；分区各自加载 |
| 无权限 | `NoAccess` / `/unavailable` | 对象级用通用页“无法打开此内容：内容不存在，或你没有查看权限”，不显示标题、归属、负责人；页面级只读用 `ReadOnlyBanner` |
| 过期 | `StaleBanner` | “有 3 条新信号、2 条已更新，点此刷新”；版本冲突显示他人修改的差异与当前值，不覆盖 |
| 失败 | `ErrorRetry` | 说明原因与下一步，保留已填内容；暂时失败可重试，确定失败不提供同一操作的重试 |
| 部分成功 | `PartialResult` | 逐项结果（成功 / 无变化 / 失败 / 未处理），失败项可筛出；分区页面汇总“2 个分区暂不可用” |
| 来源中断 | `CoverageStrip` | 按信息领域显示数据截至时间、受影响区域与中断起点，“恢复后自动补齐” |
| 数据未知 | `FieldStatus` | 第4段 F6 的八种字段状态与第3段的未知原因码，不显示为空、零或“无” |

### 5.2 错误码与文案

- 页面从不显示内部错误码；Problem Details 的 `code` 由 `web/packages/copy` 映射为“原因 + 下一步”，原因码只进入埋点与支持工具；
- 文案按码族组织：认证与会话（`unauthenticated`、`session_expired`、`mfa_required`、`totp_enrollment_required`、`invalid_tenant_context`、`channel_session_*`）、授权（`forbidden`、`content_unavailable`）、不可用（`entitlement.coverage_insufficient`、`quota.exhausted`、`limit.safety_cap`、`rights.restricted`，按 [commerce 第8节](subsystems/commerce.md#8-不可用原因)：安全上限与来源权利不显示“升级套餐”）、并发与版本（`CONFLICT`、`SIGNAL_UPDATED`、`SIGNAL_CLOSED`、`NOT_APPLICABLE`、`version_conflict`）、暂时失败（`INTERNAL`、`UNAVAILABLE`、`identity_unavailable`、各 `*_unavailable`）、渠道（`account_mismatch`、`card_link_expired`、绑定的各拒绝码）；
- 同一个码在不同页面的文案可以不同（例如 `CONFLICT` 在收件箱显示“王磊于 09:12 已调整为跟进候选”），但都从目录取模板，模板变量只来自响应中允许展示的字段。

### 5.3 写操作协议

所有写操作遵循第3段 F9 与第6段 F11 的“操作与尝试”语义，客户端统一由 `web/packages/api` 实现：

| 环节 | 规则 |
|---|---|
| 操作标识 | 打开表单、点开动作菜单或进入确认页时生成（ULID）；批量操作的逐项操作标识由服务端按批次标识与信号标识生成 |
| 幂等键 | 每次发出请求生成新的 `Idempotency-Key`；网络超时、响应丢失时用同一幂等键重发（最多 2 次，间隔 1 秒、3 秒） |
| 暂时失败 | `INTERNAL`、`UNAVAILABLE` 且确认未提交：保留表单，用户点“重试”时用新的幂等键、同一操作标识 |
| 确定结果 | 成功、无变化、已存在与确定失败固定在操作标识上；之后的再次提交是新的操作，需用户重新查看 |
| 所见版本 | 请求携带用户看到的内容版本、状态版本或对象版本；`SIGNAL_UPDATED` 与版本冲突时刷新为新版本并高亮变化，用户重新确认 |
| 撤销 | 单条反馈 10 秒内、批量在结果页、撤销创建跟进 10 分钟内；撤销是新的操作，逐项显示结果 |
| 按钮 | 提交中禁用同一按钮，不禁用页面其他部分；重复点击不产生第二个操作 |

### 5.4 时间、金额与字段

- **时间**：保留原精度与时区；查看者时区与原时区不同时同时显示（“10-25 17:00 UTC+08:00 / 18:00 UTC+09:00”），只到日的显示“仅到日”，不补成某个时刻；每个列表、时间线与统计在标题旁写明时间轴（来源发布时间、平台记录时间、发现时间、获知时间等）；“当前”与“信号判断时”两种视角不在同一屏混排，页头始终显示当前视角与锚点；
- **金额**：注明金额类型（中标金额、合同金额、预算、最高限价、投资额、费率），未公布显示“未公布”，不跨类型、不跨标包、不跨取值性质相加；
- **AI 与推断**：AI 生成内容统一标识，推断标“推断”并列出依据，未知项单独成块（[产品体验第3.3节](../01-product/04-product-experience.md#33-统一的结论与证据展示规范)）。

### 5.5 刷新与变化提示

浏览器不接收服务端推送，变化通过轮询发现，且从不自动重排用户正在操作的列表：

| 位置 | 方式 | 频率 |
|---|---|---|
| 收件箱新信号与更新 | `GET /v1/web/objectives/{id}/inbox-context?since=` | 页面可见时每 60 秒；页面隐藏时停止 |
| 对象页版本检查 | `GET /v1/objects/{id}/versions`（第7.3节） | 页面打开期间每 60 秒（第4段第10节） |
| 仿真状态 | `GET /v1/learning/simulations/{id}` | 5 秒起，指数退避到 60 秒 |
| 批量作业 | `GET /v1/detection/signal-batches/{id}` | 2 秒起，退避到 15 秒 |
| 待我处理角标 | `GET /v1/web/session` 的计数 | 页面获得焦点时与每 5 分钟 |

### 5.6 本地存储

浏览器中不长期保存业务数据：`localStorage` 只保存界面偏好（列宽、密度、上次进入的目标标识）；`sessionStorage` 只保存待续操作（第4.4节）；退出登录时两者中与账号相关的项全部清除。

### 5.7 键盘与无障碍

收件箱支持 `j`/`k` 上下移动、`x` 选中、`o` 打开详情、`r` 相关、`u` 不相关、`e` 忽略；证据面板支持 `Esc` 关闭与上一处、下一处高亮切换。焦点在抽屉打开与关闭时转移与回归，对比度满足 WCAG 2.1 AA。这些纳入可用性测试（[产品需求落地规格第3节](../01-product/05-product-requirements.md#3-每个功能prd的必备产出)）。

## 6. 统一列表组件与保存视图

### 6.1 统一列表组件

所有工作台列表（信号、跟进、成员、企业清单、简报历史、审批，以及之后的研究、决策、检索结果）使用同一组件（[产品体验第2.2节](../01-product/04-product-experience.md#22-统一列表能力)）：

- **筛选与排序**：结构化条件写入 URL 查询参数，打开链接的人按自己的权限看到结果；
- **分页**：游标分页，每页条数与上限由接口决定（收件箱 50、对象页列表 20），翻页深度上限的提示由服务端 `capped` 标记驱动；
- **选择模型**：选择后固定为标识清单，筛选、排序与新数据不扩大范围；“选择全部 N 条符合筛选的”调用服务端固定选择（收件箱为 `GET /v1/detection/objectives/{id}/signal-selection`，N ≤ 200）；已选但不在当前视图的项单独提示；
- **批量栏**：显示数量、适用与不适用的条数、确认页的影响说明，提交后进入结果视图（逐项结果、只重试暂时失败项、继续未处理项、撤销本次）；
- **列与密度**：列显示与顺序、紧凑与宽松两种密度；H5 下列表行改为卡片，批量选择改为长按进入选择模式；
- **上下文**：从列表进入详情保留列表状态，上一条、下一条在已加载的页内移动，返回时回到原滚动位置与筛选。

### 6.2 保存视图的所有者

保存视图由 **customer-context** 拥有（第11节 WD4）：它是租户私有的工作配置，与档案、目标同属租户域，受行级安全保护；gateway 不保存业务数据，identity 只表达授权。

| 项 | 规则 |
|---|---|
| 实体 | `SavedView`（`svw_`，前缀随契约登记到 `contracts/identifiers.yaml`）：列表种类（`signal_inbox`、`pursuits`、`members`、`entity_list`…）、作用域（收件箱为目标标识，跟进为租户或组织单元）、名称、所有者、可见范围（个人 / 团队）、条件（按列表种类的 Schema 校验，`additionalProperties: false`）、排序、列配置、版本；`UserViewDefault`：成员在某列表种类与作用域上的默认视图 |
| 只存条件 | 视图只保存筛选、排序与列配置，不保存结果或标识清单；打开团队视图时按打开者自己的权限执行，看到的结果可以与创建者不同 |
| 个人视图 | 只有所有者可见、可改；每个列表种类与作用域最多 50 个 |
| 团队视图 | 收件箱的团队视图在目标上共享：创建与修改需要目标上的 `can_edit`（目标负责人与范围内的决策者），可见需要 `can_view_signals`；跟进的团队视图在组织单元上共享，可见按单元继承。每个作用域最多 20 个；修改带版本，冲突时返回当前值 |
| 失效 | 条件中引用的目标停用、成员或单元被删除时视图保留，打开时提示失效的条件并按其余条件执行；目标删除时其上的视图一起删除 |
| 审计 | 团队视图的创建、修改、删除写审计；个人视图不写 |
| 接口 | `ListSavedViews` / `CreateSavedView` / `UpdateSavedView`（带 `expected_version`）/ `DeleteSavedView` / `SetDefaultView`，契约随第一个使用保存视图的 Web 实现 PR 写入 `contracts/apis/customer-context/`（[契约覆盖第9节](13-contract-readiness.md#9-随功能追加的契约)） |

第3段把“团队共享的保存视图”列为不在 M2，随统一列表组件交付：个人视图随 L2 的收件箱交付，团队视图随统一列表组件的第二批（第10节）。

## 7. BFF 契约

### 7.1 分工原则

| 情况 | 调用方式 |
|---|---|
| 资源只属于一个子系统（信号列表与反馈、档案、目标、策略草案、仿真、邀请、渠道绑定） | 浏览器经网关直接调用该子系统已有的公开接口（`/v1/detection/…`、`/v1/customer-context/…` 等），认证、租户路由与授权与 API 客户端完全相同 |
| 一个页面需要组合多个子系统，或需要 identity 的逐资源判断才能决定显示什么 | gateway BFF 的 `/v1/web/…` 聚合端点 |
| 浏览器会话、强认证与渠道落地 | gateway `web_session` 的 `/auth/…` 端点（重定向，不是 JSON 资源） |

BFF 聚合端点的共同规则：

- **只组合、不判断**：不实现业务规则，不直接访问任何业务数据库；每个下游调用以调用者的请求上下文发出，授权在所有者处执行；
- **逐分区状态**：响应由若干分区组成，每个分区带 `status`（`ok` / `empty` / `unavailable` / `not_connected` / `forbidden`）与 `reason`，一个下游失败只影响它的分区，与对象页 BFF 的约定一致（[gateway 第10节](subsystems/gateway.md#10-对象页与证据面板-bff)）；`forbidden` 分区不含任何内容；
- **不缓存**：M2 的聚合端点不缓存响应；将来缓存时只缓存按调用者裁剪后的结果，键含租户、用户、权利与权益版本，命中后仍执行全部检查（[安全与租户第3节](06-security-and-tenancy.md#3-隔离层次)）；
- **超时与降级**：整体超时 3 秒，单个下游 1.5 秒，超时的分区为 `unavailable`（可重试）；
- **字段白名单**：只返回页面需要的字段，Schema `additionalProperties: false`；
- **契约**：OpenAPI 随首个使用该端点的 Web 实现 PR 写入 `contracts/apis/gateway/openapi.yaml`（追加，升次版本），与生成代码、契约测试和越权反例一起评审（[契约覆盖第9节](13-contract-readiness.md#9-随功能追加的契约)）。本节是这些 OpenAPI 的设计依据，字段名在写入契约时可调整，语义不变。

### 7.2 会话

**`GET /v1/web/session`**：导航、强认证提示与“开始设置”的依据（第1段第8节的 gateway BFF 行）。

| 分区 | 内容 | 来源 |
|---|---|---|
| `member` | 主体、姓名、租户名称、角色、单元角色与单元名称、会话来源（`password` / `channel`） | identity 会话与成员 |
| `assurance` | TOTP 是否已登记（`enrolled` / `not_enrolled` / `unknown`，IdP 不可用时为 `unknown`）、最近强认证时间、`strong_auth_valid_until` | 会话中的已签名声明；IdP 登记状态 |
| `modules` | 主导航各模块的开放状态与批次；租户发布开关（例如第6段 F9 的“跟进”入口） | 部署配置与 commerce 套餐 |
| `channels` | 本人在各渠道的绑定状态（未绑定 / 已绑定 / 解绑处理中） | channels `ListBindings` |
| `setup` | 仅管理员：开始设置的未完成项（渠道、至少两位管理员、客户档案、首个目标） | identity、channels、customer-context |
| `pending` | 待我审批数、分派给我且即将到期的任务数 | identity `ListPendingApprovals`、pursuit `ListMyTasks` |

`assurance` 只用于提示，强认证始终由业务接口判断。

### 7.3 端点清单

| 端点 | 页面 | 组合内容 | 依赖与缺口 |
|---|---|---|---|
| `GET /v1/web/session` | 全局 | 第7.2节 | identity、channels；IdP 登记状态随 C-03 |
| `POST /v1/web/invitations:preview`（未认证） | P03 | 租户名称、组织单元、角色、打码邮箱；无效原因码 | identity 新增 `PreviewInvitation`（W-05） |
| `POST /v1/web/invitations:prepare-account`（未认证） | P03 | 创建 IdP 账号并返回设置密码的地址 | identity 新增，依赖 C-03 的 IdP 管理接口（W-05） |
| `GET /v1/web/settings/members` | P04 | 成员（角色、单元、状态、TOTP 登记）、待接受与发送失败的邀请、各成员渠道绑定 | identity 新增 `ListMembers`（W-05）、`ListInvitations`；channels `ListBindings` |
| `GET /v1/web/objectives/{id}` | P10 目标页 | 目标、规则预览与各规则所需数据可用性、首次激活状态、草案与申请状态、反馈汇总摘要 | customer-context；domain-intelligence `GetCoverage`（D-05，未接入时分区 `not_connected`）；learning |
| `GET /v1/web/objectives/{id}/policy` | P11 策略页 | 生效版本、草案修订与校验、最近一次仿真摘要与是否过期、进行中的申请、升级提示 | customer-context、learning、identity |
| `GET /v1/web/approvals/{id}` | P12 审批页 | 申请说明、规则级差异、仿真报告（代表样本按查看者权限过滤）、启用领域集合与“数据未接入”规则、回填选择、申请人与时间、是否已由他人处理 | identity、customer-context、learning；样本过滤即 D-17 |
| `GET /v1/web/my-work` | P12 待我处理 | 待我审批（按提交时间）、分派给我的任务（按截止时间）、无有效负责人的跟进（决策者） | identity、pursuit（第6段） |
| `GET /v1/web/signal-nav` | P13 | 我有 `can_view_signals` 的目标：名称、状态（未激活 / 生效 / 已停用）、待处理数、各信息领域数据截至时间 | customer-context 目标列表 + identity `Check`（每目标一次，最多 50 个；`ListPermitted` 随 C-07 后改用）；detection 计数；`GetCoverage`（D-05） |
| `GET /v1/web/objectives/{id}/inbox-context?since=` | P13 | 策略版本与启用领域集合、各分栏计数、数据截至时间与来源中断、`since` 之后的新信号与更新数 | customer-context、detection、`GetCoverage` |
| `GET /v1/web/signals/{id}` | P13 详情 | 信号、解释（规则轨迹、依据、版本与时间、更新历史）、团队反馈与处理记录、候选负责人、跟进关联（“已转跟进 · 负责人”） | detection（信号、解释、处理记录）、customer-context（分派候选）、pursuit（关联，第6段前分区 `not_connected`） |
| `GET /v1/objects/{id}/versions` | P14 | 对象的事实版本、实体版本与归属边版本摘要，供版本检查（第4段第10节，gateway 第10节“未实现”项） | 随 D-11 数据平面读取 |
| `GET /v1/web/briefings`、`GET /v1/web/briefings/{id}` | P15 | 简报历史（推送状态）；完整简报：条目快照 + 各条当前处理状态叠加（分别标注时间轴） | channels（第5段，C-04）、detection |
| `GET /v1/web/pursuits/{id}` | P16 | 跟进、任务、结果与修订、时间线（逐项标注时间轴）、授权同步状态、同标包的其他跟进 | pursuit（第6段） |

已有、无需新增的直接调用：收件箱列表、详情的单项读取、单条与批量反馈、撤销（detection 公开接口）；档案、目标、策略草案、版本与差异、申请（customer-context）；仿真与反馈汇总（learning）；邀请、审批的读取、决定与撤回（identity）；渠道安装与绑定、卡片链接（channels）；对象页、分区与证据（gateway 第10节）。

### 7.4 网关 `/auth` 端点

| 端点 | 作用 |
|---|---|
| `GET /auth/login?return_to=` | 发起授权码 + PKCE 登录（第4.2节） |
| `GET /auth/callback` | 回调：按事务 Cookie 核对 state，校验 nonce，建立会话 |
| `GET /auth/step-up?op=` | 强认证（第4.4节），可附“登记 TOTP”必需动作 |
| `POST /auth/logout` | 退出（需 CSRF 头） |
| `POST /auth/backchannel-logout` | IdP 服务器调用，不适用浏览器 CSRF 规则；按第4.2节完整校验 `logout_token`（签名、`iss`、`aud`、时间、`events`、`sid`/`sub`、无 `nonce`、`jti` 防重放）后删除对应会话 |
| `GET /auth/channel/sso?return_to=` / `GET /auth/channel/callback` | 渠道免登（第4.5节），调用 channels `ChannelSSO` |
| `GET /auth/channel/oauth?purpose=bind&return_to=` | 为绑定取渠道网页授权码（第4.6节） |

这些端点是 [C-02](../05-delivery/09-a02-end-to-end-acceptance.md#42-身份与渠道c) 所说的“gateway 未认证入口”中面向浏览器的部分；渠道的安装回调与消息回调仍按 C-02 另行实现。

## 8. 页面清单

每组页面列出对应的 PRD、数据来源与本设计需要落实的“随 Web”行为；状态与文案以 PRD 第7节与原型为准。编号用于第10节批次与走查记录。

| 编号 | 页面 | PRD | 数据来源 | 随 Web 的行为（必须实现） |
|---|---|---|---|---|
| P01 | 登录 / 强认证 / TOTP 登记 | 1 F3 | IdP 托管页面、`/auth/*` | 强认证弹框与待续操作；取消登记原动作不执行、内容保留；锁定提示；IdP 不可用时已有会话的只读页面继续可用 |
| P02 | 激活与开始设置 | 1 F1 | `web/session` 的 `setup` | 三项可跳过但始终显示未完成项；建议至少两位管理员 |
| P03 | 接受邀请 | 1 F2 | `invitations:preview`、`prepare-account`、identity `AcceptInvitation` | 已撤回、邮箱不一致、已属于其他租户、过期或被重发替换分别说明；接受后开通访问的等待提示 |
| P04 | 设置 / 成员与邀请 | 1 F2 | `web/settings/members`、identity 邀请接口 | 多行粘贴批量邀请、逐行结果、只重试失败行（每行固定幂等键）；发送失败可复制邀请链接；末位管理员保护提示 |
| P05 | 设置 / 渠道 | 1 F4、5 F8 | channels 安装接口 | 授权跳转、待验证与发送测试消息、可见范围提示、停用确认；投递统计与失败原因（不含条目） |
| P06 | 个人设置 / 渠道、简报与通知 | 1 F5–F6、5 F9 | channels `ListBindings`、偏好（第5段） | 只给出绑定入口说明、不提供二维码；解绑处理中；偏好从下一期生效、版本冲突 |
| P07 | 绑定确认（H5） | 1 F5 | `/auth/channel/oauth`、channels `bindings:prepare` / `:confirm` | 第4.6节；内置浏览器以外只提示“请在企业微信中打开” |
| P08 | 卡片落地与通用页 | 1 F7、5 F6 | `/c/:link`、channels `card-links:open` | 第4.5节；账号不一致、链接过期、通用“无法打开此内容” |
| P09 | 客户档案 | 2 F1–F4 | customer-context 档案与清单接口、entity-graph `ResolveEntity`（W-05）、evidence 文件上传（W-05） | 候选最多 10 个且不自动选中；保存前影响提示（受影响的目标与规则）；版本冲突显示差异；资质“未核验 / 待核对”；清单导入的逐行结果与提交前不生效 |
| P10 | 目标列表、启用 A02 向导、目标页 | 2 F5–F6 | customer-context、`web/objectives/{id}` | 后续目标列出开放批次不可启用；规则预览标“数据未接入，暂不生效”；参数“随档案”；停用确认 |
| P11 | 策略页、编辑草案、仿真报告、提交申请、版本历史与回滚 | 7 F1–F7 | `web/objectives/{id}/policy`、customer-context、learning | 规则级差异；停用硬条件的高亮；报告“已过期”；“候选变更仿真，非当时实际信号”；提交时再次提示未关联本企业与数据未接入；强认证；回滚说明报告反映当时数据 |
| P12 | 待我处理、审批页 | 5 F1、7 F5 | `web/my-work`、`web/approvals/{id}`、identity `decide` | 进入审批页先显示内容再要求强认证；驳回必须填原因；已由他人处理；申请人本人无批准按钮 |
| P13 | 信号收件箱、详情、批量反馈、反馈汇总、报告漏报 | 3 F1–F9 | `web/signal-nav`、`inbox-context`、`web/signals/{id}`、detection、learning、`ReportMiss`（W-05） | 不提供“全部目标”；新信号不自动重排；单条撤销 10 秒；批量固定选中清单、确认影响、逐项结果与撤销；“按 v3 判断（当前生效 v4）”；回填与已更新标记；样本少于 20 条不显示比例 |
| P14 | 对象页（采购项目、项目、组织）、证据面板、时间线与版本 | 4 F1–F10 | 对象页 BFF、`/v1/objects/{id}/versions` | 信号上下文条（求值时间与生成时间分别标注）；两种视角与锚点；分区独立加载与重试；字段八种状态；证据逐层展开与高亮；水印；查看期间合并或归属变化只提示不替换；配额与安全上限文案 |
| P15 | 今日简报（历史、完整简报、投递与重新推送） | 5 F8 | `web/briefings` | 条目“组装时”与状态“当前”两条时间轴；完整简报条目上的动作与卡片相同；只能重新推送当日一期 |
| P16 | 跟进列表、详情、创建面板、任务与截止、关闭与结果、撤销创建、持续跟踪 | 6 F1–F12 | pursuit 接口、`web/pursuits/{id}` | “已存在 · 已关联”时提示负责人与任务未写入；授权同步中；截止只填日期与时区；金额条目取值性质；撤销创建 10 分钟 |
| P17 | 未开放模块（检索、关注、研究、决策、问妙洽） | 产品体验第3节 | `web/session` 的 `modules` | 显示模块说明与开放批次，不提供可执行入口（M2 不展示研究入口） |

各页面的桌面与 H5 布局、跨段流程（信号 → 详情 → 对象页 → 证据；卡片 → 免登 → 详情；审批 → 强认证 → 批准）与全局状态见[整合原型](../01-product/prd/prototypes/00-web-shell.html)。

## 9. 安全要点（浏览器侧）

| 项 | 规则 |
|---|---|
| 令牌 | 访问令牌与刷新令牌不进入浏览器脚本、不进入 URL；会话 Cookie `HttpOnly`、`Secure`、`SameSite=Lax`、`__Host-` 前缀 |
| CSRF | 带会话 Cookie 的浏览器请求中，除 GET、HEAD 外必须带自定义头 `X-Miaoqia-Request: 1`，并校验 `Origin`（或 `Sec-Fetch-Site: same-origin`），否则 403 `csrf_rejected`；`SameSite=Lax` 是第二层。不依赖 Cookie 的服务器回调（`/auth/backchannel-logout`）不适用此规则，以自身令牌认证（第4.2节）；登录、免登与强认证的回调以事务 Cookie 防登录 CSRF |
| 跳转 | `return_to` 只接受同源相对路径并匹配路由白名单；OIDC 的 state 单次、10 分钟；IdP 与渠道回调地址精确登记 |
| 内容安全策略 | `default-src 'self'`；`script-src 'self'`（无内联脚本、无 `eval`）；`frame-ancestors 'none'`；`connect-src 'self'`；`img-src 'self' data:`；`form-action 'self'` 加 IdP 源 |
| 原文渲染 | 来源原文与模型输出一律作为文本渲染；证据高亮按定位偏移在规范化文档的文本节点上标注，不以 `innerHTML` 插入来源 HTML；外部链接加 `rel="noopener noreferrer"` |
| 弹出窗口 | 强认证窗口只接受同源 `postMessage`，消息中只有操作标识与结果，不含令牌 |
| 水印 | 对象页与证据面板叠加租户名称、用户姓名与时间的水印（威慑与追溯，不当作访问控制） |
| 无权限与不存在 | 对象级统一通用页；跨租户、伪造标识与无权限的 URL 与不存在的返回相同结果 |
| 本地数据 | 第5.6节；退出时广播清除 |
| 埋点 | 只记录租户、用户标识、页面、动作、耗时、结果与原因码，不记录字段内容与搜索词原文（各段 PRD 第11节） |

L2 环境中重新执行、并由 Web 实现 PR 附带的浏览器侧越权与反例测试：Cookie 缺失或伪造、会话固定（登录后 Cookie 必须轮换）、登录 CSRF（在另一浏览器打开合法但尚未消费的登录、免登或强认证回调地址，必须拒绝且不设置会话）、强认证回调的会话、主体或操作与发起时不符、后端通道登出（没有浏览器头的正常 IdP 通知能作废会话；伪造签名、错误 `aud`、过期、缺 `events`、带 `nonce` 与重放的通知被拒且不删除会话）、跨站请求（无 CSRF 头或错误 `Origin`）、开放跳转（`return_to` 指向外站或协议相对地址）、他租户与伪造标识的 URL、强认证窗口来自他源的消息、强认证后主体变化、免登会话尝试绑定与强认证动作、解绑后免登会话的下一次请求、卡片链接被转发给他人打开、退出后其他标签页与后退按钮不显示业务内容。

## 10. 实现批次与缺口

### 10.1 批次

按[贯通验收线](../05-delivery/09-a02-end-to-end-acceptance.md#7-已确认事项) D4，Web 实现在 L1 端到端测试通过后开始。批次按四个环节组织，每批交付页面、所需的 BFF 端点与 OpenAPI、越权反例与 L2 走查记录：

| 批次 | 页面 | 前提 |
|---|---|---|
| W0 外壳与会话 | 应用外壳、`web/session`、P01、P08 通用页、`/auth/*`、`ui` 与 `copy` 包 | R-03 开发 VPS；C-03 的 Web 登录部分（Keycloak 客户端、主题、TOTP） |
| W1 环节① 激活策略 | P02、P03、P04、P09、P10、P11、P12 | W0；W-05 中 identity 与 entity-graph 的接口 |
| W2 环节② 收到信号 | P13（含个人保存视图） | W1；L1 的信号产出 |
| W3 环节③ 查看原文 | P14 | W2；D-11、R-05 |
| W4 环节④ 简报触达 | P07、P08 卡片落地、P15、P05、P06 | 第5段服务端（C-04）；L3 时换生产企业微信适配器 |
| W5 跟进与工作台 | P16、团队保存视图、列表组件的列配置与导出 | 第6段服务端 |

### 10.2 新增缺口

以下缺口登记到[贯通验收线第4.4节](../05-delivery/09-a02-end-to-end-acceptance.md#44-界面w)；W-01 在第11节待决事项经项目负责人确认后关闭：

| 编号 | 缺口 | 最早级别 |
|---|---|---|
| W-04 | gateway `web_session`（会话、`/auth/*`、CSRF、后端通道登出）与 `/v1/web/*` 聚合端点及其 OpenAPI | L2 |
| W-05 | Web 页面需要、尚未公开的子系统接口：identity `ListMembers`、`PreviewInvitation`、邀请的账号准备；entity-graph `ResolveEntity` 的公开读取；evidence 证书文件上传；learning `ReportMiss` 的公开接口；domain-intelligence `GetCoverage` 的公开读取（随 D-05） | L2 |
| W-06 | Web 工程：`web/apps/workspace`、`web/packages/api`（OpenAPI 生成）、`ui`（设计令牌、九种状态组件、统一列表组件）、`copy`（文案目录与覆盖检查）、CI 中的构建、类型检查与端到端浏览器测试 | L2 |
| W-07 | 保存视图：customer-context `SavedView` 与接口（个人视图随 W2，团队视图随 W5） | L2（个人）、L5 前（团队） |

## 11. 待决事项

以下由编写人选定默认做法，均为推荐方案、待项目负责人确认；技术评审不代替确认（[研发规范第1节](../05-delivery/03-engineering-standards.md)）。确认后本节改名为“已确认事项”并注明确认人与日期，正文与相关规格中标“待确认”的表述随之改为已确认。

| # | 事项 | 推荐 | 备选与取舍 |
|---|---|---|---|
| WD1 | 浏览器会话模型 | gateway 持有令牌的服务端会话（第4.1节），浏览器只持有 `HttpOnly` Cookie | 单页应用作为公开客户端自己持有令牌（内存保存、静默刷新）：少一个服务端组件，但令牌暴露给脚本，XSS 可直接窃取，且企业微信内置浏览器的第三方 Cookie 与静默刷新不可靠 |
| WD2 | 免登会话的凭证形态 | gateway 会话直接引用 `chs_`，每次请求经 `ResolveChannelSession` 核验（第4.5节）；浏览器免登不再需要 IdP 按免登会话铸造令牌：C-03 去掉这一项，浏览器侧的会话与落地由 W-04 承接，`ChannelSSO` 的生产接入（渠道网页授权）由 C-01、C-02 承接；Bearer 免登令牌路径只保留给测试签发方与非浏览器客户端，不阻断浏览器 L3 | IdP 令牌交换铸造带 `miaoqia_channel_session` 的令牌：与 Bearer 路径完全统一，但需要定制 Keycloak 扩展，且令牌有效期与撤销水位要再对齐一次 |
| WD3 | 强认证后如何继续原动作 | 桌面弹出窗口完成后自动以同一操作标识重新提交；H5 与内置浏览器整页跳转，返回后恢复表单，用户再点一次（第4.4节） | 一律回到确认页由用户再点：更保守但多一步，不满足第1段 F3 “验证后继续原动作”的体验；一律自动提交：整页跳转后自动写入有被诱导的风险 |
| WD4 | 保存视图的所有者与团队视图权限 | customer-context 拥有；收件箱团队视图创建需目标 `can_edit`，可见需 `can_view_signals`（第6.2节） | gateway 保存：违反 gateway 不保存业务数据；identity 的 `Workspace`：identity 只表达授权，不宜承载列表条件；新建工作台子系统：M2 规模不值得 |
| WD5 | 登录、TOTP、找回密码页面由谁提供 | Keycloak 托管页面（妙洽主题），应用不收集密码 | 应用自绘登录页再调用 IdP：体验一致，但需要资源所有者密码模式或自建认证流程，扩大攻击面，且 `auth_time` 与 `amr` 的真实性更难保证 |
| WD6 | 未开放模块在客户导航中是否可见 | 可见、标开放批次、不可进入，租户可关闭（第3.1节） | 隐藏：界面更简洁，但客户看不到版图，与第2段目标目录的做法不一致 |
| WD7 | BFF 的边界 | 单一子系统资源直接调用公开接口，只有跨子系统组合才走 `/v1/web/*`（第7.1节） | 所有页面数据都走 BFF：浏览器只依赖一套接口，但 BFF 会复制各子系统契约并逐渐长出业务逻辑 |
