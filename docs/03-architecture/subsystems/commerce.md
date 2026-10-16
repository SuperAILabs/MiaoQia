# 订阅计量计费子系统（commerce）

> 状态：评审中 · 版本：2.1 · 更新：2026-10-16 · 平面：平台

## 1. 使命

把 [商业模式](../../01-product/02-business-model.md) 落地为可执行的套餐、权益、配额与计量，并为单位经济分析提供准确的成本与用量数据。

## 2. 职责

- 产品目录：套餐、应用领域包、数据覆盖包、能力包、超量单价；
- 订阅：租户订阅、试用、续费、升降级、生效时间；
- 权益：根据订阅计算租户可用的应用领域、目标、信息领域、渠道、功能开关；
- 配额：研究任务、导出、API 调用、存储、席位等限额与执行；
- 计量：消费各子系统的用量事件，形成可对账的用量记录；
- 成本归属：模型成本、数据成本、计算成本按租户归集（与用量分开记录）；
- 账单：账单生成、调整、对接支付或财务系统（发票与收款由财务系统完成）；
- 服务工时计量：托管服务的工时（来自 `operations`）；托管服务合同的有效状态供 `identity` 授予服务委托时核对，合同本身不授予访问。

## 3. 拥有的数据

`Product`、`Plan`、`Subscription`、`Entitlement`、`Quota`、`UsageRecord`、`CostRecord`、`Invoice`、`Adjustment`。

## 4. 接口

| 接口 | 类型 | 说明 |
|---|---|---|
| `GetEntitlements` | 查询 | 租户权益（带缓存，各子系统用于功能开关） |
| `CheckQuota` / `ReserveQuota` / `CommitUsage` | 查询/命令 | 配额检查与预留（研究等昂贵操作先预留后确认） |
| 订阅与账单管理 | 命令/查询 | 运营与租户管理员 |
| `GetUsage` / `GetCostReport` | 查询 | 用量与成本报表 |

## 5. 事件

**消费**：`research.case.completed.v1`（研究计量）、`ai_platform.invocation.recorded.v1`（模型成本）、`gateway.*`（API 用量）、`operations.service_time.logged.v1`、`identity.tenant.created.v1`。

**发布**：`commerce.subscription.changed.v1`、`commerce.entitlement.changed.v1`、`commerce.quota.exceeded.v1`。

## 6. 设计要点

- 用量记录追加式、幂等（以来源事件标识去重），可对账；
- 权益变化以事件推送，各子系统缓存失效；
- 配额不足时的行为由套餐定义：拒绝、降级或计入超量；
- 计量数据按月分区存储，报表查询走只读副本，支持单位经济分析。


## 7. 阶段衔接

M1最小模型网关与任务执行器记录技术用量，M2的gateway/search执行查看限额并关联租户；M5接入产品目录、收费权益与账单时复用既有计量。套餐降级/超量计费不得放宽安全上限或来源权利，技术任务限额不依赖财务预算批准。
