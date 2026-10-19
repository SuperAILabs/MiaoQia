# 身份与权限子系统（identity）

> 状态：评审中 · 版本：1.9 · 更新：2026-10-19 · 平面：平台

## 1. 使命

确定“谁、代表哪个租户、能对什么做什么”，并为高影响动作提供统一的审批机制。

## 2. 职责与非职责

**职责**

- 租户与组织：租户、子组织（事业部、区域）、工作空间、租户单元归属；
- 成员与账号：用户、服务账号、外部协作者、邀请、停用；
- 认证集成：对接身份提供者（OIDC/OAuth 2.1），支持企业 SSO（企业微信、钉钉、飞书、SAML/OIDC 企业身份源）；
- 通讯录与成员同步：从企业目录同步成员与组织，由 `directory` 直接对接，不经 `acquisition` 的租户集成实例；同步结果仍经成员写入的授权与审计；
- 授权：角色（RBAC）+ 关系与属性（ReBAC/ABAC），统一授权决策点；
- 动作授权与审批：高影响动作的审批请求、审批链、结果；
- 令牌与客户端：OAuth 客户端注册（含 MCP 客户端）、作用域、令牌受众；
- 内部支持访问：妙洽人员访问租户数据的申请、批准、限时与审计（break-glass）；
- 服务委托：租户把托管服务所需的有限操作委托给指定妙洽人员（第 7.8 节）。

**非职责**

- 不存储业务数据；
- 不实现业务规则中的“谁该处理什么”（分派属于 `pursuit`）。

## 3. 内部组件

| 组件 | 说明 |
|---|---|
| `idp` | 身份提供者（Keycloak，独立部署的基础组件），负责登录、联合身份、令牌签发 |
| `directory` | 租户、组织、成员、服务账号目录（本子系统数据库） |
| `authz` | 授权决策点：基于关系的授权模型 + 属性条件；所有子系统通过 SDK 调用 |
| `approvals` | 审批：审批类型注册、审批链、委托、超时 |
| `support_access` | 内部支持访问 |
| `service_delegation` | 服务委托：授予、到期、撤销与随合同或人员状态终止 |

## 4. 拥有的数据

`Tenant`、`OrgUnit`、`Workspace`、`Invitation`（待接受的成员资格：邮箱、组织单元、角色、状态、版本、过期时间）、`ChannelSession`（免登会话记录：成员、渠道绑定标识与版本、签发与到期时间）、`ChannelBindingRevocation`（每个渠道绑定的撤销水位）、`Member`、`ServiceAccount`、`RoleBinding`、`RelationTuple`（授权关系）、`ApprovalType`、`ApprovalRequest`、`ApprovalDecision`、`SupportAccessGrant`、`ServiceDelegation`（受托人员、代表的租户、范围、动作集、授权依据、批准人、生效与到期时间、撤销时间与原因、关联的托管服务合同）、`OAuthClient`。

## 5. 接口

| 接口 | 类型 | 说明 |
|---|---|---|
| `Check(subject, action, resource, context)` | 查询 | 授权决策（低延迟，带缓存） |
| `ListPermitted` | 查询 | 列出主体可访问的资源集合（用于过滤） |
| `RequestApproval` | 命令 | 提交审批（类型、对象、申请人、依据），带幂等键；发布 `identity.approval.requested.v1` |
| `WithdrawApproval` | 命令 | 申请人在决定前撤回 |
| `DecideApproval` | 命令 | 审批人批准或驳回（驳回须写原因）；决定时实时检查批准资格，申请人本人被拒绝；策略激活与回滚的申请和决定都要求强认证 |
| `ListPendingApprovals` | 查询 | 当前主体有资格决定的待审批请求 |
| `SyncResourceParticipants` | 命令 | 业务子系统（首个为 pursuit）经发件箱调用，写入其资源上的参与关系（负责人、协作者）。请求带资源、完整期望参与关系与参与版本 N；identity 与成员状态及资源范围访问求交后，在一次 OpenFGA 写入中加上应有的关系、删掉多余的关系，并记录已应用版本；只接受高于已应用版本的 N，同一 N 重复提交返回同一结果，低于已应用版本的返回已过期；结果列出被拒成员及原因（已停用、已退出、无范围访问），其余关系照常应用。业务子系统不直接写 OpenFGA。见 [PRD 第6段](../../01-product/prd/06-pursuit-follow-up.md) F12 |
| 成员、角色、组织管理 | 命令/查询 | 管理控制台 |
| `CreateInvitation` / `ResendInvitation` / `RevokeInvitation` / `ListInvitations` | 命令/查询 | 管理员邀请成员，需 `can_manage_members` 与强认证；邀请不授予任何权限，重发使旧链接失效 |
| `AcceptInvitation` | 命令 | 受邀人接受邀请。独立授权路径，不经成员管理接口：核验链接与邀请状态、已验证邮箱一致、账号不属于其他租户；受邀管理员另须同时满足两项独立条件：IdP 本次读取明确已登记 TOTP（明确未登记时 403 `totp_enrollment_required`，令牌里新鲜的 `amr=mfa` 不能代替，例如认证后删除了 TOTP），且 15 分钟内的强认证（IdP 不可用时 503，失败关闭）；在一个事务中按邀请限定的角色与单元写入成员资格并消费邀请（复用成员写入的内部逻辑），状态变化按邀请版本串行；之后写入 IdP 中账号的租户与 cell 归属，写入前访问失败关闭。见 [PRD 第1段 F2](../../01-product/prd/01-login-and-channel-binding.md#f2-邀请与接受) |
| `IssueChannelSession` / `RevokeChannelSessions` | 命令 | 仅 `channels` 可调用。签发：凭 channels 签发的 30 秒有效签发凭据（绑定标识、版本、成员），在一个事务中锁定该绑定的撤销水位行，核验凭据未过期且版本高于水位后写入不带强认证的免登会话。撤销：绑定失效后由 channels 同步调用（失败时经 `channels.binding.changed.v1` 重试），在同一行上抬高水位（无对应会话也记录）并作废版本不高于水位的会话，确认后 channels 才把失效标为已完成；并定时与 channels 对账。IdP 不保存长期渠道身份关联。并发顺序见 [PRD 第1段 F6](../../01-product/prd/01-login-and-channel-binding.md#f6-解绑与失效) |
| `RecoverAdminAccount` | 命令 | 唯一管理员无法认证时的账号恢复：平台运营双人控制且均 MFA；只清除 TOTP、要求重设密码、撤销会话，不授予租户数据访问，全程审计并通知。见 [PRD 第1段 F8](../../01-product/prd/01-login-and-channel-binding.md#f8-唯一管理员账号恢复) |
| `GrantSupportAccess` | 命令 | 内部支持访问 |
| `GrantServiceDelegation` / `RevokeServiceDelegation` / `ListServiceDelegations` | 命令/查询 | 服务委托，需 `can_grant_service_delegation` 与强认证；授予前核对 `commerce` 中有效的托管服务合同，撤销立即生效（第 7.8 节） |

最小租户/成员/角色生命周期、权威目录核验与cell-01路由已实现，具体接口、MFA边界及真实组件证据见[M0身份验收](../../05-delivery/07-m0-identity-acceptance.md)。目录数据在iam中，角色权限复用OpenFGA模型。

**实现状况（A02 第1–2段服务端切片，契约 1.2.0）**：

- **组织单元**：`OrgUnit` 在 iam 中，每个租户的根单元标识为保留值 `root`（随租户创建），其他单元为 `unt_<ULID>`（[标识前缀](../../../contracts/identifiers.yaml)）。管理员（`can_manage_members` + 15 分钟内的强认证，契约 1.3.0 起）经 `POST/PUT /v1/identity/org-units` 创建、重命名与移动单元，按 `expected_version` 并发控制，租户内串行；不能移到自身或下级之下，单元链（含根）最多 8 层，根单元不能移动。成员经 `GET /v1/identity/org-units` 列出单元；`GET /v1/identity/org-units/{unitID}` 同时经内部端口提供给业务子系统（成员或带租户上下文的服务主体），返回从单元到根单元的单元链。删除单元未实现；
- **单元角色**：成员命令的 `unit_roles` 写入绑定在单元上的角色（全量替换，缺省为空；租户管理员只能绑定在租户上），会话与 `identity.member.changed.v1`（1.1.0 起）携带 `unit_roles`；
- **Check**：带租户上下文时只判断调用主体自身。用户主体按目录中的当前成员资格、租户角色、单元角色、单元层级与资源所在单元构造上下文关系，挂在每次决策独有的一次性租户与单元对象上，由 OpenFGA 按 `model.fga` 求值，持久化旧关系不参与；资源可为本租户的 `tenant`、`org_unit` 与经 `SyncResourceParticipants` 登记的 `objective`、`pursuit`、`research_case`，其他租户的或未登记的资源一律拒绝。服务主体（`service:<子系统>`）不是成员，只能判断上下文租户上的服务动作：`can_read_context`（读取客户上下文快照与版本）与 `can_sync_<资源>_participants`，依据 identity 的服务授权清单（`services/subsystems/identity/internal/infra/service_grants.go`，启动时与 OpenFGA 对齐，清单外的同类关系删除），每次决策核对租户处于启用状态且 cell 一致。服务主体代表用户判断（显式其他主体）仍为拒绝；
- **SyncResourceParticipants**：内部端口 `POST /internal/v1/identity/participants:sync`，只接受资源所属子系统的服务主体（objective 为 customer-context，pursuit 为 pursuit，research_case 为 research）。资源登记所在租户与单元（一经登记不能转到其他租户，可随更高版本改变单元），参与版本按上表规则处理：高于已应用版本时与成员状态和单元范围访问（`org_unit#scope_member`）求交，按 OpenFGA 中的现有关系计算增删并一次写入（单次写入至多 100 条，期望集合至多 50 条），同一 SQL 事务记录版本、请求指纹与结果；同一版本同一请求返回首次结果（`replayed`），同一版本不同请求 409 `participants_version_conflict`，低于已应用版本返回 `stale` 不做修改。成员退出提交后尽力清理其在本租户已登记资源上的参与关系，重新加入前必须清理成功（失败关闭），旧参与关系不会随重新加入复活。清理、成员命令与同步按成员串行：清理与成员命令对成员行加排他锁、同步对涉及的成员行加共享锁，均持有到 OpenFGA 写入后的事务提交；清理在锁内重核成员仍未处于有效状态，成员已重新加入时跳过，因此迟到的清理不会删除重新加入后经更高参与版本写入的关系，同一版本的重放也无需重建；
- 尚未实现：管理员账号恢复、审批、支持访问与服务委托、`ListPermitted`（列表目前由业务子系统按单元逐个 `Check` 列表关系）、服务账号与外部协作者的运行时接入、单元删除与单元变更事件。

**实现状况（PRD 第1段邀请与强认证时效，契约 1.3.0）**：

- **强认证时效**：网关把已签名的 `amr`、OIDC `auth_time` 与会话来源 `miaoqia_session_source` 作为认证保证在进程内交给 identity（不进内部调用头）。租户管理动作——成员读取与写入（`GET/PUT /v1/identity/members/{subject}`）、单元创建与修改、邀请的创建、列表、重发与撤回——先授权（`can_manage_members`，非管理员 403 `forbidden`），再要求 15 分钟内的强认证：令牌带 `amr=mfa` 与 `auth_time`，按 identity 的业务时钟 `auth_time` 距今不超过 15 分钟（第 15 分 00 秒仍满足、第 15 分 01 秒不满足），晚于时钟超过 5 秒的视为不可信。只看 `auth_time` 不看签发时间，刷新令牌沿用原 `auth_time`，不延长窗口；只有 `amr=mfa` 没有 `auth_time` 的不满足；`miaoqia_session_source=channel`（渠道免登会话，见下文契约 1.4.0）与未登记的会话来源一律不满足。不满足时 403 `mfa_required`；IdP 明确该账号尚未登记 TOTP 时 403 `totp_enrollment_required`（客户端先引导登记，原动作不执行）；IdP 不可用、登记状态未知时仍为 `mfa_required`，不当作“未登记”也不放行。其他子系统的强认证动作经本地绑定 `identity.Guard`：`RequireStrongAuth`（有效成员 + 强认证，例如业务开拓者提交策略激活申请）与 `RequireAdmin`（再加 `can_manage_members`，渠道安装与停用、强制解绑）；认证保证不跨进程传递，identity 拆为远程绑定时不提供该绑定，调用方失败关闭。平台运营命令（租户开通与停用）仍只核对已签名的 `amr=mfa`；
- **邀请**：`POST /v1/identity/invitations`、`GET /v1/identity/invitations`、`POST /v1/identity/invitations/{id}/resend`、`POST /v1/identity/invitations/{id}/revoke`（管理员 + 强认证，幂等键与 `expected_version` 同成员命令）与 `POST /v1/identity/invitations:accept`（受邀人）。邀请 `inv_<ULID>` 记录邮箱（小写）、姓名、租户角色、单元角色、状态、版本、链接代次、过期时间、投递结果、邀请人与接受人；链接令牌为 32 字节随机数，只保存摘要，令牌原文只出现在创建或重发的首次响应与邀请邮件中，同一幂等键的重放不再返回令牌（之后要复制链接须重发）。状态机见下文“邀请状态机”；
- **接受**：独立授权，不经成员管理接口，调用者只需是已认证用户（令牌通常不带租户）。按链接存在、未撤回、链接代次等于当前代次、未过期、IdP 中账号的已验证邮箱与邀请一致、账号不是其他租户的有效成员、不是本租户的有效成员、邀请含租户管理员角色时有 15 分钟内的强认证的顺序核验；通过后在一个事务中（租户行锁、邀请行锁）按邀请限定的角色与单元角色经成员写入的内部逻辑写入成员资格（清除旧参与关系、发布 `identity.member.changed.v1`），把邀请置为已接受并保存首次结果，登记待写入 IdP 的租户归属，发布 `identity.invitation.changed.v1`。同一账号以同一链接再次接受（含并发与响应丢失后的重试）返回首次结果（`already_accepted`），不再写入或发布；其他账号接受已接受的邀请 409。接受请求不能携带角色或单元。M2 一个账号只属于一个租户：接受与成员命令的加入都按账号加咨询锁并核对其他租户的有效成员资格，不同租户的并发加入只有一个成功；平台运营开通租户时指定的首位管理员暂未核对；
- **IdP 租户归属**：接受提交后写入 IdP 中账号的租户与 cell（`identity_tenant_bindings`）；写入成功前 `access` 为 `provisioning`，令牌不带租户，访问失败关闭。写入在归属行锁与账号咨询锁内执行，写入前核对接受人仍是该租户的有效成员（否则取消），加入其他租户须先取得同一把账号锁，因此旧租户的迟到写入不会覆盖新租户的归属。未完成的写入由重复接受与 `identity.Maintenance.RetryTenantBindings` 重试；
- **出站端口**：IdP 账号查询与租户归属写入（`api.IdentityProvider`）、邀请邮件（`api.Notifier`）是端口，测试使用内存实现（`api/idptest`）。生产 Keycloak 管理接口与邮件渠道的适配器尚未实现，`identity.Module()` 缺省使用“未接入”实现：接受邀请返回 503 `identity_unavailable`，邀请邮件记为发送失败，强认证的登记状态视为未知；
- **定时维护**：`identity.Maintenance`（本地绑定）提供 `ExpireDueInvitations` 与 `RetryTenantBindings`，定时任务角色尚未接入；在此之前到期的邀请在列表、创建、重发、撤回与接受前按业务时钟置为已过期，接受与撤回在行锁内再次核对，不会接受或撤回到期的邀请；
- 邀请尚未实现：批量粘贴（客户端逐行调用，每行带幂等键）、接受前的确认页查询、新账号经邀请链接注册与邮箱验证（IdP 流程）、接受后以登录邮箱建立邮件渠道绑定（随 channels）、退出成员的 IdP 租户归属清除。

**邀请状态机**：

```text
待接受 pending ──接受──► 已接受 accepted
   ├──撤回──► 已撤回 revoked
   ├──重发──► 待接受（链接代次加一，旧链接 410 invitation_link_superseded）
   └──到期──► 已过期 expired ──重发──► 待接受
```

每次变化在邀请行锁下使版本加一并发布一条 `identity.invitation.changed.v1`（`created`、`resent`、`accepted`、`revoked`、`expired`）；接受、撤回、重发与过期并发时只有一个生效，失败的一方按最新状态返回原因（409 `invitation_not_pending`、409 `version_conflict`、410 `invitation_revoked` / `invitation_expired` / `invitation_link_superseded`、409 `invitation_already_accepted`）。其他租户的与不存在的邀请都返回 404 `invitation_not_found`。

本切片选定的缺省值（规格未定处）：

| 事项 | 取值 |
|---|---|
| 强认证时间声明 | OIDC `auth_time` 与 `amr` 含 `mfa`；时效 15 分钟（含边界），未来偏差至多 5 秒；会话来源声明 `miaoqia_session_source`，缺省为 IdP 交互登录 |
| 强认证判断顺序 | 先授权后强认证：非管理员得到 `forbidden`，不会被引导登记 TOTP |
| 邀请列表 | 也要求强认证（PRD 第8节）；按 `(created_at, id)` 倒序分页，每页 `limit` 条（默认且至多 500）。还有更早的邀请时 `truncated` 为真并给出不透明的 `next_cursor`，作为 `cursor` 取下一页：下一页取严格更早的位置，翻页期间不变的邀请不重复、不遗漏，新建的不会挤入；游标绑定签发它的租户与 `state` 过滤，换租户或换过滤为 400 `invalid_cursor`。翻页期间状态改变的邀请可能不再出现在按状态过滤的后续页。列表前先把全部到期的待接受邀请置为已过期 |
| 同一邮箱已有待接受邀请 | 返回已有邀请（`outcome existing`），不发新链接 |
| 待接受上限 | 每租户 1000 个未过期的待接受邀请，写入前核对 |
| 过期时间 | 创建或最近一次重发起 7 天，取整到秒；过期事件的 `effective_at` 为过期时间，其余为执行时间 |
| 撤回与重发 | 均带 `expected_version`；已过期的不能撤回，可重发 |
| 邮件投递结果 | 按链接代次记录 `sent` / `failed`；发送结果未能记录时为 `pending`（未知），重发后旧代次的结果不覆盖新链接 |
| 接受通知 | `identity.invitation.changed.v1`（accepted）每个邀请只发布一次，通知由其消费方发出；邀请本身的邮件经 `Notifier` |
| 受邀账号已是本租户有效成员 | 409 `already_member`，不改变其角色（改角色走成员管理接口） |
| IdP 中没有该账号 | 403 `email_unverified`；IdP 不可用为 503 |

**实现状况（PRD 第1段渠道免登会话，契约 1.4.0）**：

- **接口**：`IssueChannelSession`、`RevokeChannelSessions` 与对账用的 `ListLiveChannelSessions`，经本地绑定 `identity.ChannelSessions` 与内部端口 `POST/GET /internal/v1/identity/channel-sessions`、`POST /internal/v1/identity/channel-sessions:revoke` 提供，本地与远程行为一致。只接受带租户上下文的 `service:channels`（服务授权清单 `channel_session_issuer` → `can_issue_channel_sessions`），其他主体 403 `forbidden`；
- **撤销水位**：每个渠道绑定（`chb_<ULID>`）在 iam 中一行 `identity_channel_revocations`，签发与撤销都在事务中 `SELECT … FOR UPDATE` 锁定这一行，二者在同一行上串行。签发依次核验：凭据到期时间不晚于“现在 + 35 秒”（否则 400 `invalid_credential`）；同一 `credential_id` 已签发过时返回首次结果（`replayed`），内容不同 409 `credential_conflict`，该会话已作废或版本不高于水位 409 `binding_revoked`；“现在 + 5 秒”早于凭据到期时间（否则 410 `credential_expired`，迟到的签发不产生会话）；绑定版本高于水位（否则 409 `binding_revoked`）；成员有效（否则 403 `member_inactive`）；绑定建立后成员没有退出过（否则 409 `member_rejoined`，见下一条）；然后写入会话。撤销把水位抬高到给定版本（只升不降，无会话时也记录）并作废版本不高于水位的会话，重复撤销返回 `already_applied`。签发事务核验水位后、写入前到达的撤销在水位行上等待签发提交，随后作废该会话；
- **成员退出是生命周期边界**：成员每次从有效变为退出时，在成员行记录这次退出时的成员版本（`left_version`），重新加入只会继续增加成员版本，不改变它。channels 在绑定请求固定会话时取当前成员版本，记为绑定的成员版本，签发请求带上它（`member_version`），会话也记录它。绑定的成员版本不高于 `left_version` 即为退出之前的旧绑定：签发 409 `member_rejoined`，该绑定的会话在核验时 401 `channel_session_revoked`。这项判断只依赖 identity 自己的成员行，不依赖 channels 是否已收到 `identity.member.changed.v1`，因此退出事件延迟时重新加入的成员也不能沿用旧绑定或旧会话，须重新绑定（PRD 第1段 F6）；
- **会话**：`chs_<ULID>`，绝对时长 12 小时（identity 业务时钟），不带强认证，不因使用而延长。网关对 `miaoqia_session_source=channel` 的令牌要求 `miaoqia_channel_session` 声明，缺失为 401 `channel_session_invalid`；每次请求经本地 `ResolveChannelSession` 核验会话存在且属于令牌的租户与主体（否则 401 `channel_session_invalid`）、未作废、版本高于水位且来自成员最近一次退出之后（401 `channel_session_revoked`）、未过期（401 `channel_session_expired`），再按普通会话核对成员与租户状态（成员停用或退出 403 `invalid_tenant_context`）。远程会话核验绑定不提供这项核验，渠道免登令牌在 identity 拆为远程时失败关闭（503 `identity_unavailable`）；
- **撤销的重试与对账**：identity 消费 `channels.binding.changed.v1`（`revoking` 与 `invalidated`），按事件版本撤销；`ListLiveChannelSessions` 按绑定标识分页（每页至多 500，游标绑定租户）列出仍有有效会话的绑定及其中最高版本，channels 据此对账（channels.md 第6节实现状况）；
- 尚未实现：IdP 按免登会话铸造令牌（测试由测试签发方按 identity 返回的会话标识签发）、免登会话的单独登出、按成员批量撤销（成员停用或退出时会话由成员核验拒绝，绑定失效由 channels 消费成员事件完成）。

内部授权Check契约见[`openapi.yaml`](../../../contracts/apis/identity/openapi.yaml)。显式subject是可信内部服务的授权查询参数，缺省时取上下文主体；外部上下文由gateway验证后派生，内部头不能作为外部身份凭据。relation/object格式在本地与远程绑定均校验；未知业务字段（包括tenant）、空主体与错误上下文拒绝，错误统一为application/problem+json及稳定code。请求单次超时5秒，仅暂时性失败重试，最多3次。目录核验租户/成员当前状态及单元；当前SQL角色经同一OpenFGA模型做隔离contextual tuples求值，持久化旧角色关系不恢复被撤销权限。上下文提供租户不等于已获得资源权限。网关令牌验证和内部mTLS身份已通过[M0运行时验收](../../05-delivery/05-m0-runtime-acceptance.md)；审批及具体生产部署另按相应门禁验收。

## 6. 事件

**已实现生命周期发布**：`identity.tenant.created.v1`、`identity.tenant.suspended.v1`、`identity.tenant.activated.v1`、`identity.member.changed.v1`、`identity.invitation.changed.v1`，载荷见[identity事件契约](../../../contracts/events/identity/events.asyncapi.yaml)。审批事件随策略激活审批实现：`identity.approval.requested.v1`（通知有资格的审批人）、`identity.approval.decided.v1`（批准或驳回）、`identity.approval.closed.v1`（撤回或失效）；策略激活与回滚申请超过 7 天未决定、申请人失去申请权或目标的生效版本与策略代次不再等于申请时的基准时失效（[PRD 第7段](../../01-product/prd/07-policy-simulation-approval.md) F5，时限由项目负责人确认）。支持访问与服务委托事件随对应功能实现（服务委托发布 `identity.service_delegation.changed.v1`：授予、到期、撤销、终止）；邀请事件 `identity.invitation.changed.v1`（创建、重发、接受、撤回、过期；事件契约 1.2.0）已随邀请实现，载荷不含邮箱与链接令牌，按邀请版本单调递增。消费 `channels.binding.changed.v1`（契约 1.4.0 起，失效中与已失效的绑定按事件版本撤销免登会话，第 5 节实现状况）。

## 7. 授权模型

### 7.1 主体、角色与范围

| 主体 | 说明 |
|---|---|
| 成员 | 租户的用户，通过角色绑定获得权限；一人可兼任多个角色 |
| 外部协作者 | 受邀参与特定推进的合作方用户，不是租户成员，只经推进上带到期时间的外部协作关系获得受限视图 |
| 服务账号 | 租户的 API / MCP 客户端，可以绑定除租户管理员外的角色，权限为角色绑定与令牌作用域的交集 |
| 妙洽内部人员 | 平台运营与支持人员，访问租户数据只能经内部支持访问或服务委托（第 7.6、7.8 节），不成为租户成员 |

角色即 [业务架构](../../02-business/01-business-architecture.md#51-客户角色) 中的客户角色，在授权模型中的关系名为：决策者 `decision_maker`、业务开拓者 `business_developer`、项目推进者 `pursuit_lead`、研究分析者 `research_analyst`、业务专家 `domain_expert`、租户管理员 `admin`。

**成员资格。** 租户的用户与服务账号加入时由 `directory` 写入租户成员关系 `tenant#member`，停用时删除。角色关系与参与关系（目标参与人、研究发起人与共享对象、推进负责人与协作者）都与成员资格取交集，所以成员停用后，即使残留角色或参与关系，也立即失去全部访问。`directory` 停用成员时仍应清理其全部关系，以免残留数据在重新加入时复活。

**范围。** 每个租户有一个根组织单元，其下可建事业部、区域等子单元。租户管理员只绑定在租户上；其余角色可绑定在租户或任一组织单元上，组织单元继承上级的角色，绑定在某个单元的角色不对兄弟单元生效。租户域资源归属一个组织单元：目标（含其策略、信号、判断与决策）、研究案例、推进、知识资产，在资源所在单元上判断；创建类动作（创建目标、发起研究、上传知识资产、申请导出）在要创建到的单元上判断。资源为 `tenant` 的动作作用于整个租户：查看客户档案与检索公共数据对全体成员开放，其余只认绑定在租户上的角色。

**租户管理员按最小权限设计。** 管理员负责成员、配置、审计与审批（含策略激活审批，因此能查看目标与策略），不起草策略、不看信号、不参与研究与推进；需要做业务工作时另兼业务角色。

### 7.2 权限矩阵

| 取值 | 含义 |
|---|---|
| ✓ | 可执行（在角色绑定的范围内） |
| ◐ | 仅对本人参与的资源可执行：目标的参与人、研究的发起人与共享对象、推进的负责人与协作者（外部协作者为推进上的外部协作关系）、受限知识资产的被授予者 |
| — | 不可执行 |

| 动作 | 资源 | 授权关系 | 决策者 | 业务开拓者 | 项目推进者 | 研究分析者 | 业务专家 | 租户管理员 | 外部协作者 |
|---|---|---|---|---|---|---|---|---|---|
| 查看客户档案与能力 | tenant | can_view_profile | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| 维护客户档案、产品、资质与业绩 | tenant | can_edit_profile | — | — | — | — | ✓ | ✓ | — |
| 检索公共数据（受暴露控制） | tenant | can_search_public | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| 创建目标 | org_unit | can_create_objective | ✓ | — | — | — | — | — | — |
| 发起研究 | org_unit | can_start_research | ✓ | ✓ | ✓ | ✓ | — | — | — |
| 上传与维护知识资产 | org_unit | can_manage_knowledge | — | — | — | — | ✓ | ✓ | — |
| 申请批量导出租户产物 | org_unit | can_request_export | ✓ | ✓ | ✓ | ✓ | — | ✓ | — |
| 列出单元内目标（范围内可见） | org_unit | can_list_objectives | ✓ | — | — | ✓ | — | ✓ | — |
| 查看一般知识资产 | knowledge_asset | can_view | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| 查看受限知识资产 | knowledge_asset:restricted | can_view | ◐ | ◐ | ◐ | ◐ | ✓ | ◐ | — |
| 授予受限知识资产 | knowledge_asset:restricted | can_grant | — | — | — | — | ✓ | ✓ | — |
| 下调知识资产分级 | knowledge_asset:restricted | can_downgrade | — | — | — | — | — | ✓ | — |
| 查看目标与策略 | objective | can_view | ✓ | ◐ | ◐ | ✓ | ◐ | ✓ | — |
| 调整目标 | objective | can_edit | ✓ | — | — | — | — | — | — |
| 管理目标参与人 | objective | can_manage_participants | ✓ | — | — | — | — | — | — |
| 起草与仿真策略 | objective | can_draft_policy | ✓ | ◐ | — | — | — | — | — |
| 提交策略激活或回滚申请 | objective | can_request_activation | ✓ | ◐ | — | — | — | — | — |
| 批准策略激活与回滚 | objective | can_approve_activation | ✓ | — | — | — | — | ✓ | — |
| 查看信号与命中理由 | objective | can_view_signals | ✓ | ◐ | ◐ | ✓ | ◐ | ◐ | — |
| 反馈信号 | objective | can_feedback | ✓ | ◐ | ◐ | ✓ | — | — | — |
| 分派跟进（由信号创建推进） | objective | can_assign | ✓ | ◐ | — | — | — | — | — |
| 起草判断与决策 | objective | can_draft_decision | ✓ | ◐ | ◐ | — | — | — | — |
| 批准决策 | objective | can_approve_decision | ✓ | — | — | — | — | — | — |
| 查看研究报告 | research_case | can_view | ✓ | ◐ | ◐ | ✓ | ◐ | ◐ | — |
| 共享研究（仅限租户成员） | research_case | can_share | ✓ | ◐ | ◐ | ✓ | — | — | — |
| 导出研究报告 | research_case | can_export | ✓ | ◐ | ◐ | ✓ | — | — | — |
| 查看推进（完整视图） | pursuit | can_view | ✓ | ✓ | ✓ | ✓ | ✓ | ◐ | — |
| 查看推进（受限视图） | pursuit | can_view_limited | ✓ | ✓ | ✓ | ✓ | ✓ | ◐ | ◐ |
| 评论、提交协作任务 | pursuit | can_collaborate | ✓ | ◐ | ◐ | ◐ | ◐ | ◐ | ◐ |
| 更新推进（阶段、任务、结果） | pursuit | can_edit | ✓ | ◐ | ◐ | — | — | — | — |
| 指定推进负责人与协作者 | pursuit | can_manage_team | ✓ | ◐ | ◐ | — | — | — | — |
| 邀请外部协作者 | pursuit | can_invite_external | ✓ | ◐ | ◐ | — | — | — | — |
| 批准批量导出 | tenant | can_approve_export | — | — | — | — | — | ✓ | — |
| 查看用量与账单 | tenant | can_view_usage | ✓ | — | — | — | — | ✓ | — |
| 管理成员、组织与角色 | tenant | can_manage_members | — | — | — | — | — | ✓ | — |
| 查看审计 | tenant | can_view_audit | — | — | — | — | — | ✓ | — |
| 管理数据政策与暴露配置 | tenant | can_manage_data_policy | — | — | — | — | — | ✓ | — |
| 创建 API 凭证、MCP 客户端与 Webhook | tenant | can_manage_credentials | — | — | — | — | — | ✓ | — |
| 批准内部支持访问 | tenant | can_approve_support_access | — | — | — | — | — | ✓ | — |
| 授予与撤销服务委托 | tenant | can_grant_service_delegation | — | — | — | — | — | ✓ | — |

说明：

- 公共数据（公告、企业、项目，含其中的公开联系人信息）对能看到它的成员按 [安全与租户](../06-security-and-tenancy.md) 第 6、7 节提供，不另设权限；其数量、翻页与配额由暴露控制约束；
- 推进的完整视图与受限视图是两个关系：外部协作者只有受限视图（不含策略、得分、内部评论），字段范围由 `pursuit` 定义；
- 研究只能共享给租户成员，不能共享给外部协作者；
- 知识资产分级下调须在一次写入中删除受限归属并写入一般归属，不留中间状态；
- 授权模型的机器可读定义为 `services/subsystems/identity/internal/infra/model.fga`（OpenFGA）；测试逐格校验模型与本矩阵一致，角色绑定在租户上、绑定在组织单元上、由服务账号承担各校验一遍，并覆盖兄弟单元、跨租户、停用成员、服务账号与外部协作到期等负向用例；启动时与最新模型版本比对，不一致时写入新版本。内部接口见 `contracts/apis/identity/openapi.yaml`。

### 7.3 高影响动作与审批

| 动作 | 申请人 | 批准人 | 附加控制 |
|---|---|---|---|
| 策略激活与回滚 | 有 `can_request_activation` 的成员 | 目标所在范围的决策者或租户管理员，不得为申请人本人 | 附仿真报告（[质量与验收](../../05-delivery/02-quality-and-acceptance.md#8-发布门禁)）；个人租户只调整模板白名单参数时免审批，仍需仿真并留审计（[策略 DSL 3.3](../11-policy-dsl.md#33-模板与租户策略的合成)） |
| 决策批准 | 起草人 | 决策者，金额超过其审批额度时转上级单元的决策者 | 决策记录批准人与理由，不可变 |
| 批量导出租户产物 | 有 `can_request_export` 的成员 | 租户管理员 | 导出文件带租户与用户水印；公共数据不可导出 |
| 对外发送（向外部协作者或外部邮箱发送推进内容） | 推进的负责人 | 无需审批 | 只发往租户管理员维护的渠道白名单（外部协作者或收件域），按受限视图的内容范围执行，记录审计；白名单变更本身留审计；研究报告不对外发送 |
| 邀请外部协作者 | 有 `can_invite_external` 的成员 | 无需审批 | 必须设到期时间，到期自动失效；记录审计 |
| 联系人使用（使用联系人个人信息开展联络） | 推进负责人 | 租户管理员或其指定的数据合规负责人 | 记录用途与依据，授权限时；个人信息不进入其他租户与模型训练 |
| 自动提交（代租户向外部系统提交报名、报价或材料） | 推进负责人 | 目标所在范围的决策者 | 逐次审批；提交内容与回执不可变留存 |
| 知识资产分级下调 | 业务专家 | 租户管理员 | — |
| API 凭证、MCP 客户端与 Webhook 创建 | 租户管理员 | — | 多因素认证；凭证限作用域与有效期 |
| 内部支持访问 | 妙洽内部人员 | 租户管理员 | 限时、默认只读、全程审计，租户管理员可见 |
| 唯一管理员账号恢复 | 平台运营（核验申请人有权代表该租户后发起，核验方式不固定，记录入审计） | 另一名平台运营 | 双人控制、均需 MFA；只重置认证因素与会话，不授予租户数据访问；通知管理员与签约联系人 |

审批由本子系统的 `approvals` 执行：申请与批准分别留审计，申请人不能批准自己的申请（授权模型只表达“谁有资格批准”）。

审批额度条件（第 7.4 节）随决策功能实现。实现之前，目标所在范围内的任一决策者都能批准任意金额的决策，没有超额转批，审批记录供事后审计。

### 7.4 属性条件

关系之外，以下属性参与决策，以 OpenFGA 条件（CEL）表达：

| 条件 | 适用 | 规则 | 状态 |
|---|---|---|---|
| 有效期 `not_expired` | 外部协作者 | 关系携带到期时间，授权决策点注入当前时间，到期自动失效 | 已实现 |
| 有效期 | 内部支持访问、服务委托、服务账号 | 同上 | 随所属功能实现 |
| 审批额度 | 决策批准 | 决策金额不超过批准人角色绑定上的额度 | 随决策功能实现 |
| 数据分级 | 知识资产进入模型上下文 | 按 [安全与租户](../06-security-and-tenancy.md#5-数据分级与处理约束) 的分级与租户授权决定，不由角色决定 | 随所属功能实现 |

### 7.5 服务账号、Agent 与渠道

- **服务账号**：可绑定除租户管理员外的角色，权限为其角色绑定与令牌作用域（`gateway` 的 MCP 与 API 作用域）的交集；停用即删除其成员资格；
- **平台服务主体**：子系统以 `service:<子系统>` 身份在带租户上下文时调用（事件消费、求值、同步），不是租户成员、没有角色，只获得 identity 服务授权清单登记的租户服务动作（第 5 节实现状况）；与服务委托不同，它不代表任何妙洽人员，审计条目记录服务主体本身，不带 `delegation`；
- **Agent**：研究运行以发起人的身份执行，可用的数据与工具为发起人权限与研究模板工具白名单的交集；定时运行的监测以服务身份执行，结果只写入目标范围内的资源；
- **渠道**：渠道内的提问、反馈、分派与审批调用与 Web 相同的接口，经同一授权决策，渠道身份须先与成员绑定。

### 7.6 妙洽内部角色

| 角色 | 平台权限 | 租户数据 |
|---|---|---|
| 平台运营 | `platform:miaoqia` 的 `operator`：链路探针、运行状态；MFA下开通/停用租户及读取目录元数据 | 无 |
| 数据运营 | 公共域数据维护（来源、解析复核、实体复核），经 `operations` 调用数据供应平面的命令接口 | 无 |
| 研究分析师、客户成功 | 托管研究、策略调优 | 托管服务经服务委托（第 7.8 节）；排障经内部支持访问；均按租户逐次授予 |
| 领域专家 | 应用领域包与行业包的设计与验收 | 无 |

### 7.7 列表与过滤

看板、目标列表、知识资产库等列表查询不逐条调用 `Check`：

- **范围内可见**（矩阵中的 ✓）：业务子系统按资源归属单元过滤。用 `ListPermitted`（OpenFGA ListObjects）求出主体在哪些组织单元上有该类资源的列表关系，再在本库按 `unit_id` 过滤；组织单元数量有限，结果可按成员版本缓存。各类资源的单元级列表关系（如 `org_unit#can_list_pursuits`）随该列表功能一起加入模型与矩阵；
- **本人参与**（矩阵中的 ◐）：参与关系（负责人、协作者、共享对象、被授予者）由业务子系统在本库同步保存一份，用于列表查询；授权的权威仍是 OpenFGA，详情与写操作逐个 `Check`；
- 列表结果与详情不一致时，以详情的 `Check` 为准；列表接口的负向测试须覆盖兄弟单元与停用成员。

### 7.8 服务委托

托管研究服务（[商业模式](../../01-product/02-business-model.md#3-交付模式)）需要妙洽人员在租户内起草策略、发起研究、整理简报，这些是写操作，不适合用默认只读、用于排障的内部支持访问。服务委托是为此单设的授权，复用同一套授权模型、审计与有效期条件：

- **合同不等于权限**：`commerce` 中有效的托管服务合同只是授予的前提。授权来自租户管理员逐人、逐范围的授予；合同终止、受托人员离岗或调岗时委托随之终止，不必等到期；
- **授予内容**：受托人员（妙洽平台身份，须 MFA）、代表的租户、范围（组织单元或具体目标）、动作集、授权依据（合同与授予记录）、生效与到期时间。到期必填，续期是新的授予；
- **动作集**：只能从研究分析者与业务开拓者可做的起草类动作中选取（发起研究、起草与仿真策略、提交激活申请、反馈信号、整理简报草稿、查看所需的目标、信号与研究）；第 7.3 节需要批准的动作（策略激活、决策批准、批量导出、联系人使用、自动提交）、对外发送、邀请外部协作者、成员与凭证管理都不能委托。受托人员提交的申请由租户内的批准人批准；
- **授权实现**：在 OpenFGA 中以带 `not_expired` 条件的委托关系表达，只并入上述动作对应的授权关系；受托人员不写入 `tenant#member`，也不获得角色绑定，委托撤销或到期后立即失去访问；
- **委托会话**：受托人员不是成员，普通成员的会话解析（核对成员状态）会拒绝他们，委托不通过放宽成员检查或租户请求头进入租户。受托人员先以已验证的妙洽平台身份登录（须 MFA），再由 `identity` 为其选定的一条委托签发委托会话：核对实际主体与委托的受托人一致、MFA 有效、委托在有效期内且未撤销、托管服务合同有效、租户状态与 cell 归属正确；会话绑定实际主体、代表的租户、cell 与委托标识，到期取委托到期与会话上限的较早者。此后每个请求由 `gateway` 按会话类型走委托解析路径，逐次核对委托当前状态（取代成员状态核对），请求上下文携带委托标识；动作授权仍逐次经 `Check` 判断委托关系。审计 SDK 从请求上下文填入 `delegation`，调用方不能自行声明；
- **记录与可见**：每次操作的审计条目记录实际操作者（受托人员）、代表的租户、委托标识与授权依据（[审计事件](../../../contracts/events/audit/events.asyncapi.yaml) 的 `delegation`）；`operations` 的服务工单引用委托标识；租户管理员可随时查看委托清单与服务记录，并可撤销。

服务委托与内部支持访问并存：前者用于托管服务的日常工作，后者只用于排障，两者不互相替代。

委托会话的接口、令牌形态与机器契约随功能交付。启用门禁至少覆盖：本地与远程会话解析结果一致；非成员持有效委托可完成委托范围内的动作；错租户、超范围动作、撤销后、到期后、合同终止后与缺少 MFA 的请求均被拒绝；普通成员会话的解析与成员状态核对不受影响；审计条目的 `delegation` 只来自委托会话。

## 8. 安全要点

- 租户上下文来自已验证令牌，identity目录每次核对当前租户、成员状态与cell归属；委托会话以委托当前状态取代成员状态核对（第 7.8 节）；错误上下文拒绝而不静默改写；
- 服务账号最小权限、可轮换、可吊销；
- 授权决策可审计（谁、何时、对什么、结果、依据）；
- 租户管理操作需要 15 分钟内的强认证（第 5 节实现状况）：以 IdP 签发的 `auth_time` 计算，不以令牌签发时间计算，刷新令牌与渠道免登会话不延长；生产 IdP 须保证 `amr` 与 `auth_time` 描述同一次带第二因素的交互认证（部署验收）。
