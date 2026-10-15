# 身份与权限子系统（identity）

> 状态：评审中 · 版本：1.1 · 更新：2026-10-15 · 平面：平台

## 1. 使命

确定“谁、代表哪个租户、能对什么做什么”，并为高影响动作提供统一的审批机制。

## 2. 职责与非职责

**职责**

- 租户与组织：租户、子组织（事业部、区域）、工作空间、租户单元归属；
- 成员与账号：用户、服务账号、外部协作者、邀请、停用；
- 认证集成：对接身份提供者（OIDC/OAuth 2.1），支持企业 SSO（企业微信、钉钉、飞书、SAML/OIDC 企业身份源）；
- 授权：角色（RBAC）+ 关系与属性（ReBAC/ABAC），统一授权决策点；
- 动作授权与审批：高影响动作的审批请求、审批链、结果；
- 令牌与客户端：OAuth 客户端注册（含 MCP 客户端）、作用域、令牌受众；
- 内部支持访问：妙洽人员访问租户数据的申请、批准、限时与审计（break-glass）。

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

## 4. 拥有的数据

`Tenant`、`OrgUnit`、`Workspace`、`Member`、`ServiceAccount`、`RoleBinding`、`RelationTuple`（授权关系）、`ApprovalType`、`ApprovalRequest`、`ApprovalDecision`、`SupportAccessGrant`、`OAuthClient`。

## 5. 接口

| 接口 | 类型 | 说明 |
|---|---|---|
| `Check(subject, action, resource, context)` | 查询 | 授权决策（低延迟，带缓存） |
| `ListPermitted` | 查询 | 列出主体可访问的资源集合（用于过滤） |
| `RequestApproval` | 命令 | 提交审批（类型、对象、申请人、依据） |
| `DecideApproval` | 命令 | 审批人决定 |
| 成员、角色、组织管理 | 命令/查询 | 管理控制台 |
| `GrantSupportAccess` | 命令 | 内部支持访问 |

内部授权Check契约见[`openapi.yaml`](../../../contracts/apis/identity/openapi.yaml)。显式subject是可信内部服务的授权查询参数，缺省时取上下文主体；外部上下文由gateway验证后派生，内部头不能作为外部身份凭据。relation/object格式在本地与远程绑定均校验；未知业务字段（包括tenant）、空主体与错误上下文拒绝，错误统一为application/problem+json及稳定code。请求单次超时5秒，仅暂时性失败重试，最多3次。租户资源访问由OpenFGA关系模型判断；上下文提供租户不等于已获得该租户所有资源权限。网关令牌验证和内部mTLS身份已通过[M0运行时验收](../../05-delivery/05-m0-runtime-acceptance.md)；审批及具体生产部署另按相应门禁验收。

## 6. 事件

**发布**：`identity.tenant.created.v1`、`identity.tenant.suspended.v1`、`identity.member.changed.v1`、`identity.approval.requested.v1`、`identity.approval.decided.v1`、`identity.support_access.granted.v1`。

## 7. 授权模型

### 7.1 主体、角色与范围

| 主体 | 说明 |
|---|---|
| 成员 | 租户的用户，通过角色绑定获得权限；一人可兼任多个角色 |
| 外部协作者 | 受邀参与特定推进的合作方用户，不是租户成员，只经资源上的直接关系（推进协作者、研究共享）获得访问 |
| 服务账号 | 租户的 API / MCP 客户端，权限为其角色绑定与令牌作用域的交集 |
| 妙洽内部人员 | 平台运营与支持人员，访问租户数据只能经内部支持访问（第 7.6 节） |

角色即 [业务架构](../../02-business/01-business-architecture.md#51-客户角色) 中的客户角色，在授权模型中的关系名为：决策者 `decision_maker`、业务开拓者 `business_developer`、项目推进者 `pursuit_lead`、研究分析者 `research_analyst`、业务专家 `domain_expert`、租户管理员 `admin`。

角色绑定在租户或组织单元上，组织单元继承上级的角色。租户域资源归属一个组织单元：目标（含其策略、信号、判断与决策）、研究案例、推进、知识资产。绑定在上级的角色对下级单元的资源生效，绑定在某个单元的角色不对兄弟单元生效。

### 7.2 权限矩阵

| 取值 | 含义 |
|---|---|
| ✓ | 可执行（在角色绑定的范围内） |
| ◐ | 仅对本人参与的资源可执行：目标的参与人、研究的发起人与共享对象、推进的负责人与协作者、受限知识资产的被授予者 |
| — | 不可执行 |

| 动作 | 资源 | 授权关系 | 决策者 | 业务开拓者 | 项目推进者 | 研究分析者 | 业务专家 | 租户管理员 | 外部协作者 |
|---|---|---|---|---|---|---|---|---|---|
| 查看客户档案与能力 | tenant | can_view_profile | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| 维护客户档案、产品、资质与业绩 | tenant | can_edit_profile | — | — | — | — | ✓ | ✓ | — |
| 上传与维护知识资产 | tenant | can_manage_knowledge | — | — | — | — | ✓ | ✓ | — |
| 查看一般知识资产 | knowledge_asset | can_view | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| 查看受限知识资产 | knowledge_asset:restricted | can_view | ◐ | ◐ | ◐ | ◐ | ✓ | ✓ | — |
| 下调知识资产分级 | knowledge_asset | can_downgrade | — | — | — | — | — | ✓ | — |
| 创建与调整目标 | tenant | can_create_objective | ✓ | — | — | — | — | ✓ | — |
| 查看目标与策略 | objective | can_view | ✓ | ◐ | ◐ | ✓ | ◐ | ✓ | — |
| 起草与仿真策略 | objective | can_draft_policy | ✓ | ◐ | — | — | — | ✓ | — |
| 提交策略激活或回滚申请 | objective | can_request_activation | ✓ | ◐ | — | — | — | ✓ | — |
| 批准策略激活与回滚 | objective | can_approve_activation | ✓ | — | — | — | — | ✓ | — |
| 查看信号与命中理由 | objective | can_view_signals | ✓ | ◐ | ◐ | ✓ | ◐ | ✓ | — |
| 反馈信号 | objective | can_feedback | ✓ | ◐ | ◐ | ✓ | — | — | — |
| 分派跟进（由信号创建推进） | objective | can_assign | ✓ | ◐ | — | — | — | — | — |
| 起草判断与决策 | objective | can_draft_decision | ✓ | ◐ | ◐ | — | — | — | — |
| 批准决策 | objective | can_approve_decision | ✓ | — | — | — | — | — | — |
| 检索公共数据（受暴露控制） | tenant | can_search_public | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| 发起研究 | tenant | can_start_research | ✓ | ✓ | ✓ | ✓ | — | — | — |
| 查看研究报告 | research_case | can_view | ✓ | ◐ | ◐ | ✓ | ◐ | ✓ | ◐ |
| 导出研究报告 | research_case | can_export | ✓ | ◐ | ◐ | ✓ | — | ✓ | — |
| 查看推进 | pursuit | can_view | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ◐ |
| 评论、提交协作任务 | pursuit | can_collaborate | ✓ | ◐ | ◐ | ◐ | ◐ | ◐ | ◐ |
| 更新推进（阶段、任务、结果） | pursuit | can_edit | ✓ | ◐ | ◐ | — | — | — | — |
| 邀请外部协作者 | pursuit | can_invite_external | ✓ | ◐ | ◐ | — | — | — | — |
| 申请批量导出租户产物 | tenant | can_request_export | ✓ | ✓ | ✓ | ✓ | — | ✓ | — |
| 批准批量导出 | tenant | can_approve_export | — | — | — | — | — | ✓ | — |
| 查看用量与账单 | tenant | can_view_usage | ✓ | — | — | — | — | ✓ | — |
| 管理成员、组织与角色 | tenant | can_manage_members | — | — | — | — | — | ✓ | — |
| 查看审计 | tenant | can_view_audit | — | — | — | — | — | ✓ | — |
| 管理数据政策与暴露配置 | tenant | can_manage_data_policy | — | — | — | — | — | ✓ | — |
| 创建 API 凭证、MCP 客户端与 Webhook | tenant | can_manage_credentials | — | — | — | — | — | ✓ | — |
| 批准内部支持访问 | tenant | can_approve_support_access | — | — | — | — | — | ✓ | — |

说明：

- 公共数据（公告、企业、项目，含其中的公开联系人信息）对能看到它的成员按 [安全与租户](../06-security-and-tenancy.md) 第 6、7 节提供，不另设权限；其数量、翻页与配额由暴露控制约束；
- 外部协作者看到的推进是受限视图（不含策略、得分、内部评论），字段范围由 `pursuit` 定义；
- 授权模型的机器可读定义为 `services/subsystems/identity/internal/infra/model.fga`（OpenFGA）；测试逐格校验模型与本矩阵一致；启动时与最新模型版本比对，不一致时写入新版本。内部接口见 `contracts/apis/identity/openapi.yaml`。

### 7.3 高影响动作与审批

| 动作 | 申请人 | 批准人 | 附加控制 |
|---|---|---|---|
| 策略激活与回滚 | 有 `can_request_activation` 的成员 | 目标所在范围的决策者或租户管理员，不得为申请人本人 | 附仿真报告（[质量与验收](../../05-delivery/02-quality-and-acceptance.md#8-发布门禁)） |
| 决策批准 | 起草人 | 决策者，金额超过其审批额度时转上级单元的决策者 | 决策记录批准人与理由，不可变 |
| 批量导出租户产物 | 有 `can_request_export` 的成员 | 租户管理员 | 导出文件带租户与用户水印；公共数据不可导出 |
| 对外发送（向外部协作者或外部邮箱发送推进内容） | 推进的负责人 | 无需审批 | 只发往租户管理员维护的渠道白名单（外部协作者或收件域），按受限视图的内容范围执行，记录审计；白名单变更本身留审计；研究报告不对外发送 |
| 联系人使用（使用联系人个人信息开展联络） | 推进负责人 | 租户管理员或其指定的数据合规负责人 | 记录用途与依据，授权限时；个人信息不进入其他租户与模型训练 |
| 自动提交（代租户向外部系统提交报名、报价或材料） | 推进负责人 | 目标所在范围的决策者 | 逐次审批；提交内容与回执不可变留存 |
| 知识资产分级下调 | 业务专家 | 租户管理员 | — |
| API 凭证、MCP 客户端与 Webhook 创建 | 租户管理员 | — | 多因素认证；凭证限作用域与有效期 |
| 内部支持访问 | 妙洽内部人员 | 租户管理员 | 限时、默认只读、全程审计，租户管理员可见 |

审批由本子系统的 `approvals` 执行：申请与批准分别留审计，申请人不能批准自己的申请（授权模型只表达“谁有资格批准”）。

### 7.4 属性条件

关系之外，以下属性参与决策，以 OpenFGA 条件（CEL）表达，随所属功能实现：

| 条件 | 适用 | 规则 |
|---|---|---|
| 审批额度 | 决策批准 | 决策金额不超过批准人角色绑定上的额度 |
| 有效期 | 外部协作者、内部支持访问、服务账号 | 关系带到期时间，到期自动失效 |
| 数据分级 | 知识资产进入模型上下文 | 按 [安全与租户](../06-security-and-tenancy.md#5-数据分级与处理约束) 的分级与租户授权决定，不由角色决定 |

### 7.5 服务账号、Agent 与渠道

- **服务账号**：权限为其角色绑定与令牌作用域（`gateway` 的 MCP 与 API 作用域）的交集；
- **Agent**：研究运行以发起人的身份执行，可用的数据与工具为发起人权限与研究模板工具白名单的交集；定时运行的监测以服务身份执行，结果只写入目标范围内的资源；
- **渠道**：渠道内的提问、反馈、分派与审批调用与 Web 相同的接口，经同一授权决策，渠道身份须先与成员绑定。

### 7.6 妙洽内部角色

| 角色 | 平台权限 | 租户数据 |
|---|---|---|
| 平台运营 | `platform:miaoqia` 的 `operator`：链路探针、运行状态 | 无 |
| 数据运营 | 公共域数据维护（来源、解析复核、实体复核），经 `operations` 调用数据供应平面的命令接口 | 无 |
| 研究分析师、客户成功 | 托管研究、策略调优 | 仅经内部支持访问，按租户逐次批准 |
| 领域专家 | 应用领域包与行业包的设计与验收 | 无 |

## 8. 安全要点

- 令牌中的租户声明是租户上下文的唯一来源；
- 服务账号最小权限、可轮换、可吊销；
- 授权决策可审计（谁、何时、对什么、结果、依据）；
- 管理员操作需要多因素认证。
