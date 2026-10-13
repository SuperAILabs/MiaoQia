# 身份与权限子系统（identity）

> 状态：评审中 · 版本：1.0 · 更新：2026-10-12 · 平面：平台

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

## 6. 事件

**发布**：`identity.tenant.created.v1`、`identity.tenant.suspended.v1`、`identity.member.changed.v1`、`identity.approval.requested.v1`、`identity.approval.decided.v1`、`identity.support_access.granted.v1`。

## 7. 授权模型

```text
主体（用户 / 服务账号 / 外部协作者）
  └─ 属于 租户 / 组织单元
       └─ 角色绑定（决策者、业务开拓者、项目推进者、研究分析者、业务专家、租户管理员、外部协作者…）
资源（信号、研究、判断、决策、推进、策略、知识资产…）
  └─ 归属 租户 / 组织单元 / 推进（协作者关系）
条件（属性）
  └─ 数据分级、区域、目标、金额阈值、时间窗口
```

高影响动作清单（需审批或特定授权）：策略激活与回滚、决策批准、批量导出、对外发送、联系人信息查看与使用、知识资产分级下调、Webhook 与 API 凭证创建、内部支持访问。

## 8. 安全要点

- 令牌中的租户声明是租户上下文的唯一来源；
- 服务账号最小权限、可轮换、可吊销；
- 授权决策可审计（谁、何时、对什么、结果、依据）；
- 管理员操作需要多因素认证。
