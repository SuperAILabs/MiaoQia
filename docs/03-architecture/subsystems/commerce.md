# 订阅计量计费子系统（commerce）

> 状态：评审中 · 版本：2.4 · 更新：2026-10-26 · 平面：平台

## 1. 使命

把 [商业模式](../../01-product/02-business-model.md) 落地为可执行的套餐、权益、配额与计量，并为单位经济分析提供准确的成本与用量数据。

## 2. 职责

- 产品目录：套餐、应用领域包、数据覆盖包、能力包、超量单价；
- 订阅：租户订阅、试用、续费、升降级、生效时间；
- 权益：根据订阅计算租户可用的应用领域、目标、信息领域、渠道、功能开关；私有部署客户的权益同时决定公共数据馈送的范围（[部署拓扑第 10 节](../07-deployment-topology.md#10-私有部署的公共数据馈送)）；
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
| `GetEntitlements` | 查询 | 租户权益（带缓存，各子系统用于功能开关）；不可用时返回第 8 节的原因类别 |
| `CheckQuota` / `ReserveQuota` / `CommitUsage` | 查询/命令 | 配额检查与预留（研究等昂贵操作先预留后确认） |
| 订阅与账单管理 | 命令/查询 | 运营与租户管理员 |
| `GetUsage` / `GetCostReport` | 查询 | 用量与成本报表 |

## 5. 事件

**消费**：`research.case.completed.v1`（研究计量）、`ai_platform.invocation.recorded.v1`（模型成本）、`gateway.*`（API 用量）、`operations.service_time.logged.v1`、`identity.tenant.created.v1`。

**发布**：`commerce.subscription.changed.v1`、`commerce.entitlement.changed.v1`、`commerce.quota.exceeded.v1`。

## 6. 设计要点

- 用量记录追加式、幂等（以来源事件标识去重），可对账；对象查看与原文查看的计量以计量操作标识（租户、用户、被查看的对象或证据、计量日）去重，每次请求的标识只用于重发，同一计量操作只计一次（[PRD 第4段](../../01-product/prd/04-object-page-evidence.md) F9）；
- 权益变化以事件推送，各子系统缓存失效；
- 配额不足时的行为由套餐定义：拒绝、降级或计入超量；拒绝时按第 8 节区分原因，不统一提示升级；
- 计量数据按月分区存储，报表查询走只读副本，支持单位经济分析。


## 7. 阶段衔接

M1最小模型网关与任务执行器记录技术用量，M2的gateway/search执行查看限额并关联租户；M5接入产品目录、收费权益与账单时复用既有计量。套餐降级/超量计费不得放宽安全上限或来源权利，技术任务限额不依赖财务预算批准。

## 8. 不可用原因

用户遇到“不能用”时，原因不同，应对方式也不同，不能都显示为“升级套餐”：

| 原因 | 含义 | 稳定错误代码 | 界面给出的下一步 |
|---|---|---|---|
| 未订阅 | 套餐不含该应用领域包、能力包或渠道 | `entitlement.not_subscribed` | 说明所需的包，由租户管理员决定是否开通 |
| 覆盖范围不足 | 已订阅，但对象所在区域、行业或信息领域不在数据覆盖包内 | `entitlement.coverage_insufficient` | 说明缺的覆盖维度；对象内容不展示 |
| 额度不足 | 本周期的研究任务、导出等额度已用完 | `quota.exhausted` | 显示已用量与重置时间；按套餐允许追加或计入超量 |
| 安全上限 | 平台为安全与稳定设置的技术上限（如单次查看规模、并发、批量大小） | `limit.safety_cap` | 说明上限与缩小范围的办法；升级套餐不能解除 |
| 来源权利限制 | 来源权利不允许该展示或用途（[数据架构第 12 节](../02-data-architecture.md#12-来源权利与数据权益)） | `rights.restricted` | 说明受限的用途与可用的替代（例如查看来源链接）；与套餐无关 |

错误按 [集成契约](../03-integration-contracts.md#22-约定) 的 Problem Details 返回，代码只用上表；一次请求同时命中多个原因时按表中自下而上的顺序报告最根本的一个（来源权利与安全上限优先，因为它们不能通过购买解决）。各子系统自己判断安全上限与来源权利，本子系统只判断订阅、覆盖与额度，三者的代码共用此表。

## 9. 席位与用量口径

席位不预先作为独立实体建模：席位数是按 `identity` 的成员状态计算的用量，由本子系统以 `Quota` 执行。

| 对象 | 是否计入席位 | 说明 |
|---|---|---|
| 活跃成员 | 计入 | 自成员资格写入时起计 |
| 邀请中 | 不计入 | 接受后才计入；未接受的邀请数可另设上限，防止滥发 |
| 已停用成员 | 不计入 | 自停用时起不计 |
| 服务账号 | 不计入席位 | 计入开放 API/MCP 能力包的客户端数与调用用量 |
| 外部协作者 | 不计入席位 | 按推进上的外部协作关系计数，可设上限；不是租户成员 |
| 服务委托的妙洽分析师 | 不计入 | 托管服务按工时计量（第 2 节） |

调整生效：增加席位立即生效；减少席位在当前计费周期结束时生效。达到上限时阻止新的邀请接受与新增服务账号，不自动停用已有成员；降级后超出的部分由租户管理员选择停用对象，平台不替租户挑选。具体计价档位仍由商务确定（[商业模式第 4 节](../../01-product/02-business-model.md#4-计价结构)）。

## 10. 查看计量的实现状况

**实现状况（PRD 第4段 F9，契约 [`contracts/apis/commerce/openapi.yaml`](../../../contracts/apis/commerce/openapi.yaml) 0.1.0 草案）**：

- **接口**：内部接口 `POST /internal/v1/commerce/view-meters`（`RecordView`，经 `ViewMetering` 绑定由 gateway 以用户主体调用），请求为查看类型（`object_view` / `evidence_view`）与被查看的对象或证据，必须带 `Idempotency-Key`（尝试键）。回执给出计量操作标识（`mtr:` 加（租户、用户、类型、对象、计量日）的 SHA-256）、计量日与时区、`counted` / `unchanged`、已用量、额度与重置时间；
- **去重与额度**：同一尝试以咨询锁串行，同一租户、类型与计量日的用量行以行锁串行，同一事务内依次：按（调用者、尝试键）重放已保存的尝试结果（含 429；同键不同请求 409 `idempotency_conflict`）→ 锁定当日用量 → 计量操作已存在为 `unchanged` → 用量达到额度为 429 `quota.exhausted`（带已用量、额度与重置时间，保存该尝试，不写计量）→ 否则写入计量并加一。额度为每租户每计量日每类型，权益接入前来自配置 `MIAOQIA_COMMERCE_OBJECT_VIEW_DAILY_QUOTA` 与 `MIAOQIA_COMMERCE_EVIDENCE_VIEW_DAILY_QUOTA`（缺省 2000）；计量日按 `MIAOQIA_COMMERCE_METERING_TZ`（缺省 Asia/Shanghai；租户时区随租户设置接入）；
- **补记**：消费 `gateway.view_meter.deferred.v1`（consumer `commerce.view-backfill`），按同一计量操作标识写入（`source = backfill`），已存在则不计，只在新写入时加用量；补记不检查额度，不补授权；
- **数据**：`com_view_usage`、`com_view_meters`、`com_view_attempts`，均按租户行级隔离；
- **覆盖判断（契约 0.2.0，验收线 R-05）**：内部接口 `GET /internal/v1/commerce/coverage`（`checkCoverage`，`GetEntitlements` 的数据覆盖部分，经 `Coverage` 绑定由 gateway 的对象页与 learning 的代表样本以用户主体调用，服务主体 403）。按对象的信息领域与 GB/T 2260 区划判断，省级代码包含该省全部区划、地级代码包含该市全部区划；区划未知时只有“全部区域”覆盖，否则为覆盖不足（缺失维度 `region`）；信息领域不在包内时缺失维度为 `info_domain`。产品目录接入（M2-P8）之前覆盖包来自部署配置 `MIAOQIA_COMMERCE_COVERAGE`（如 `INF-PROC=330000;INF-ORG=*`），对全部租户相同，响应以 `source = deployment_config` 标明；格式错误拒绝启动，未配置时 503 `coverage_unavailable`（调用方失败关闭）。按租户的覆盖包随产品目录与订阅交付；
- **未实现**：产品目录、订阅、按租户的权益与覆盖包、`commerce.quota.exceeded.v1`、用量报表与账单。
