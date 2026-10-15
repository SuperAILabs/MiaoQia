# M0 最小身份生命周期与 cell-01 路由

> 状态：已批准 · 版本：1.0 · 更新：2026-10-15 · 负责人：identity、安全与平台工程负责人

本文界定 M0-06 的工程验收范围。身份认证由 Keycloak 负责，identity 保存租户、成员、角色与单元目录；网关从已验证令牌取得待核对的租户上下文，再由目录确认当前有效状态和 cell 归属。产品交互与原型由独立设计工作交付，不在本文范围内。

## 1. 实现范围

| 能力 | 实现与证据 |
|---|---|
| 租户创建 | 平台operator且有已签名MFA声明才可创建；服务端分配tnt标识与cell-01，原子创建首位管理员。Idempotency-Key按主体/租户作用域去重；同键不同内容或命令409；客户端不能指定cell |
| 租户停用/恢复 | 仅平台operator且MFA；expected_version防止覆盖并发变更。停用提交后，新请求包括旧有效令牌均被目录拒绝；恢复不会恢复已退出成员 |
| 成员加入/退出、角色变更 | 当前租户管理员且MFA；租户来自上下文，目标subject是已知IdP主体标识。请求携带成员expected_version，0表示首次加入；退出必须清空角色。没有邀请邮件、账号注册或跨租户自动加入 |
| 最后一名管理员 | 全部成员写入及租户状态变更在租户行锁下串行化；禁止删除/降级最后一名有效管理员，并发双管理员退出也至少保留一名 |
| 撤权 | 不读取令牌中的角色，不缓存目录授权结果。每次读取当前有效成员与角色，退出后旧令牌和重新登录令牌均不能重新取得访问；重新加入须显式成员命令 |
| 权限模型 | 复用现有OpenFGA模型。用当前SQL角色在随机、非持久化的tenant对象上做contextual tuples求值，避免旧持久化角色关系恢复已撤销权限；不向FGA同步目录角色，无SQL/FGA双写事务 |
| 单元路由 | M0只支持cell-01。签名cell须与目录归属相同；未知租户、停用、退出、错误/未知cell统一403，目录故障503。不会把错误cell静默改成cell-01；请求体/查询/上下文头不能选择另一租户 |
| 写入、事件、审计 | 目录变更、命令回执、identity生命周期事件与成功审计写入同一iam数据库事务。审计失败则全部回滚。事件经通用outbox/relay交付，不意味着新业务消费者已实现 |

撤权边界是变更提交后的新核验：已经通过核验并在执行中的业务请求不被强制中断；长任务在后续执行点按各能力重新授权。租户停用不等于删除数据或撤销整个IdP账号。多cell迁移、组织层级、外部协作者、服务账号、邀请/SSO、审批及生产MFA流程按对应阶段另验收。

## 2. 接口、存储和绑定

[identity OpenAPI](../../contracts/apis/identity/openapi.yaml)当前为1.1.0，生命周期接口复用生成服务端与Problem错误；原授权Check结构保持兼容。

| 接口 | 边界 |
|---|---|
| POST /v1/tenants | 公共网关；平台operator + MFA；name/admin_subject及Idempotency-Key头 |
| PUT /v1/tenants/{tenantID}/state | 公共网关；平台operator + MFA；state/expected_version |
| GET /v1/tenants/{tenantID} | 公共网关；平台operator + MFA；读取当前状态/版本供冲突恢复 |
| GET /v1/identity/members/{subject} | 公共网关；当前租户管理员 + MFA；含退出成员的当前状态/版本 |
| PUT /v1/identity/members/{subject} | 公共网关；当前租户管理员 + MFA；state/roles/expected_version |
| GET /v1/identity/session | 公共网关或内部mTLS；只核验当前调用主体、租户与cell |
| POST /internal/v1/authz:check | 既有内部mTLS；平台资源沿用既有模型，当前M0目录管理支持tenant根资源权限 |
| GET /internal/v1/identity/session | 新增mTLS核验；identity.SessionResolver的本地/远程绑定共用契约 |

管理写入统一要求Idempotency-Key；重试返回同一版本的原回执，不重复写事件；每次返回回执前仍重新授权，撤权后不能借重放取回结果。expected_version用于新命令的乐观并发控制，与幂等键各自生效。

平台命令中的tenantID是operator+MFA授权的管理对象，不是普通用户选择租户上下文或cell的参数。网关还校验核验响应的Schema及tenant/cell/subject/state与原上下文一致，不能借错误目录响应换身份。

管理命令不挂载内部端口。MFA只取已验证访问令牌的amr=mfa；进程内传递，不编码到Miaoqia-Context。内部服务不能仅凭上下文头执行管理命令。签发方必须保证MFA声明源于真实强认证，不允许用户自行编辑声明；本次验证声明真实性检查，不代证生产认证流程配置。

目录为identity独占的平台表，[迁移](../../services/subsystems/identity/migrations/00001_directory.sql)创建identity_tenants、identity_members及identity_commands回执表；不与业务数据库共享连接。应用查询显式指定租户，写入在同一租户行锁与版本条件下执行。管理接口在加载identity的API进程提供；identity拆出时，其公共端口仍经同一gateway认证包装，其内部核验通过mTLS调用。

在M0中，托管租户的Check只支持当前主体对当前tenant根资源的关系；其他资源类型、代其他成员决策、服务账号/协作者需在其能力启用前增加权威归属和身份核验，默认拒绝。现有权限矩阵描述目标模型，不代表所有业务资源与管理API均已交付。

[集成配置](../../contracts/integration.yaml)升为3.0.0，原因是租户请求新增强制目录核验。首批冻结与此前运行时验收记录按历史审计保留，不改写原哈希，也不声称当前字节与旧冻结制品相同。

## 3. 验证与复现

主要测试为[TestIdentityLifecycleAndCell](../../services/kernel/integration/identity_test.go)：真实PostgreSQL、OpenFGA、HTTPS RS256/JWKS、生成HTTP服务及mTLS；新增生命周期与审计经真实Kafka中继交付并校验原Schema。覆盖创建及成员写入幂等、并发同键创建、同键冲突、无权限/无MFA/伪造头、未知角色/重复角色、cell与成员校验、末位管理员、并发退出、旧令牌撤权、FGA残留关系、租户停用恢复、SQL/审计故障回滚、事件原Schema校验及本地/远程绑定。

真实Keycloak子用例通过隔离测试管理客户端设置**仅管理员可编辑**的租户/单元属性，然后实际密码登录取令牌，验证正确cell、错误cell及成员退出后的旧/新令牌拒绝。测试显式保留姓名/邮箱等必填profile字段，防止属性更新令账号进入未完成状态；测试结束清除租户属性，避免污染平台探针。测试客户端只存在于[本地/CI realm](../../deploy/local/keycloak/realm-miaoqia.json)，不作为生产模板。

正向MFA管理命令使用共享的[HTTPS签名测试签发方](../../services/internal/testissuer/issuer.go)，与令牌边界测试复用；不将模拟MFA声明称为实际完成OTP/硬件认证。生产IdP、认证流程和客户端配置仍属部署验收。现有完整M0探针继续使用真实HTTPS Keycloak运行。

设置隔离的MIAOQIA_TEST_PG、MIAOQIA_TEST_OPENFGA、MIAOQIA_TEST_KAFKA、MIAOQIA_TEST_KEYCLOAK及MIAOQIA_GATEWAY_CA_FILE后执行：

```sh
go -C services test -race -count=1 ./kernel/integration ./subsystems/identity/... ./subsystems/gateway/...
uv run --project tools/contracts --python 3.12 --locked mqc check
uv run --project tools/contracts --python 3.12 --locked mqc compat --base HEAD
uv run --project tools/contracts --python 3.12 --locked mqc generate --check
```

缺组件或跳过真实Keycloak子用例不能关闭对应门禁。完整测试结果及明确范围由[身份验收记录](../../contracts/reviews/m0-identity-acceptance.yaml)登记；整个M0状态仍以[阶段基线](06-stage-baseline.md)为准。
