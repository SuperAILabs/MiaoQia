# 客户上下文与策略子系统（customer-context）

> 状态：评审中 · 版本：1.6 · 更新：2026-10-15 · 平面：客户智能

## 1. 使命

让平台持续、结构化地理解每个客户：他们是谁、能做什么、要达成什么目标、如何判断，并把这种理解以可版本化、可仿真、可回滚的策略形式提供给监测、研究和判断。

## 2. 职责与非职责

**职责**

- 客户档案：价值链角色、行业、产品与服务、规格、资质证书、业绩、人员能力、服务区域、禁入项、风险偏好；
- 目标配置：租户启用的商业目标（`TenantObjective`）、每个目标采用的机会路径与判断框架版本；
- 策略管理：策略 DSL、草案、版本、激活（影子/比例/全量）、回滚；
- 自然语言调优：把用户的自然语言描述转为结构化策略差异（经 `ai-platform`），只生成草案；
- 知识资产：客户上传的资料（资质文件、业绩证明、产品手册、历史方案），带分级、有效期与负责人；
- 租户私有实体：客户自己维护的组织清单（引用公共实体），种类见第 4 节 `TenantEntityList`。

**非职责**

- 不执行匹配（属于 `detection`）；
- 不执行仿真（属于 `learning`）；
- 不负责审批流程本身（属于 `identity` 的审批服务）。

## 3. 内部组件

| 组件 | 说明 |
|---|---|
| `profile` | 客户档案与能力模型 |
| `objectives` | 目标配置 |
| `policy_engine` | 策略 DSL 的解析、校验、版本管理与编译（编译为 `detection` 可执行形式与 `search` 反查查询） |
| `policy_assistant` | 自然语言 → 策略差异草案 |
| `knowledge` | 知识资产管理、解析（复用 `evidence` 的文档处理能力，结果存租户域） |
| `tenant_entities` | 租户私有实体清单与标签 |

## 4. 拥有的数据（租户域）

| 实体 | 说明 |
|---|---|
| `CustomerProfile` / `ProfileVersion` | 档案与版本：主体类型（M2 仅企业，后续支持个人）、我方主体列表 `org_ids`（集团客户的多个法人，每个可挂组织单元）、价值链角色、行业包列表（投资与研究机构可不绑定）、品类、品牌、服务区域、组织单元覆盖（品类、区域、资质）；每次保存一个版本，带乐观并发版本号。快照按目标所在单元合成，记录各来源版本 |
| `Offering` / `Capability` / `Qualification` / `PastPerformance` | 产品、能力、资质、业绩，均带证据文件引用与有效期；资质与业绩属于具体的我方主体。资质的客户录入与公共记录作为两份来源观察分别保存：按所属主体与证书编号（双方都有时加发证机构）识别同一证书，公共记录引用固定版本并记录核验时间与状态（未核验 / 未查到 / 一致 / 待核对）；类型、等级或有效期冲突时快照中为未知、列入待核对，保留双方证据、不自动覆盖；未接入或未查到按客户录入判定，不推断为不具备。每个主体可声明“资质已录全”，未声明时未录入的资质为未知。裁决与求值规则见 PRD 第2段 F3 |
| `Market` | 租户定义的市场 `mkt_`：区域集合（区划代码与版本）、品类集合（行业包分类与版本）、主体类型与时间窗（滚动窗口如近 12 个月，或固定区间）；业务键为（租户、归一后的维度组合），同一租户同一维度组合只建一个，唯一约束防重复。公共域不读取本对象，统计时由研究与判断按归一维度查询 domain-intelligence 的版本化统计，并记录所用统计版本与窗口的实际起止 |
| `TenantObjective` | 启用的目标：模板与版本、所属组织单元、负责人、参与人、参数（随档案或本目标值）、状态（未激活 / 生效 / 已停用）、关联的策略；同一单元同一模板只启用一次 |
| `EntityListImport` | 企业清单导入作业：文件、逐行匹配结果（已匹配 / 待确认 / 需选择 / 未匹配）、状态；匹配完成不写入，操作人看过影响提示并提交后，已匹配与已确认的行才写入清单 |
| `Policy` / `PolicyVersion` / `PolicyRollout` | 策略、版本、发布状态 |
| `KnowledgeAsset` | 知识资产：分级、可用于哪些工作流与模型 |
| `TenantEntityList` | 租户私有实体清单，种类：`customers` 客户、`partners` 伙伴、`competitors` 竞争对手、`blocked` 禁合作、`tracked` 跟踪对象、`counterparties` 交易对手；策略中以 `lists.<种类>` 引用 |

## 5. 策略 DSL

每个 `TenantObjective` 有一份策略：应用领域包模板 + 租户参数 + 追加规则，条件以 CEL 书写，求值为三值逻辑并产出处置。结构、求值语义、编译与下推、生命周期与限制以 [策略 DSL](../11-policy-dsl.md) 为准。本子系统负责其中的存储、校验、编译（`policy_engine`）、自然语言调优（`policy_assistant`）与版本管理。

## 6. 接口

| 接口 | 类型 | 说明 |
|---|---|---|
| `GetContextSnapshot` | 查询 | 按（租户、组织单元、上下文版本）或时点返回合成后的客户上下文快照（研究与判断固定引用）：单元覆盖逐项沿单元链向上继承，未覆盖继承、显式空集合不继承；资质按主体给出状态表（代码 → 有效 / 过期 / 待核对）与“资质已录全”声明，供规则区分满足、不具备与未知；供策略求值的部分见[上下文快照契约](../../../contracts/schemas/intelligence/context-snapshot.schema.json)；资质按主体判断、不跨主体拼凑，遍历全部我方主体，缺状态表的主体为未知；未关联本企业时不含我方主体列表；真实快照组装通过用例 22 是 `INF-QUAL` 启用门禁（[PRD 第2段 F3](../../01-product/prd/02-profile-and-objectives.md#f3-产品品牌区域与资质)） |
| `GetContextVersion` | 查询 | 租户当前上下文版本（读主库）；档案与清单的每次提交在同一事务中递增该版本，detection 每批求值前读取，保证提交返回后开始的求值使用新版本 |
| `Get/Update Profile`、`LinkOrganization`、`Manage Offerings/Qualifications/...` | 查询/命令 | 档案维护；`UpdateProfile` 带 `expected_version`，冲突时拒绝 |
| `PreviewProfileImpact` | 查询 | 保存档案或清单前，列出受影响的目标与规则 |
| `ImportEntityList` / `ConfirmEntityListRows` / `ManageEntityList` | 命令 | 企业清单导入（`202` 异步作业，单次 5000 行，经 `entity-graph.ResolveEntity` 匹配；只有统一社会信用代码精确命中可自动匹配）与维护。流程见 [PRD 第2段](../../01-product/prd/02-profile-and-objectives.md) |
| `ListObjectiveTemplates` | 查询 | 按租户的价值链角色列出可启用目标、所需数据及其可用性 |
| `EnableObjective` / `ConfigureObjective` / `DisableObjective` | 命令 | 目标配置：启用时创建目标（未激活）与首版策略草案，首次激活经仿真与审批；参数变更形成新草案；停用后不再产生新信号 |
| `DraftPolicyChange` | 命令 | 结构化或自然语言输入 → 策略草案与差异 |
| `RequestPolicyActivation` | 命令 | 提交审批（审批由 `identity` 执行） |
| `ActivatePolicy` / `RollbackPolicy` | 命令 | 审批通过后由系统执行 |
| `GetActivePolicies` | 查询 | `detection` 加载 |
| `UploadKnowledgeAsset` | 命令 | 上传与分级 |

## 7. 事件

**发布**：`customer_context.profile.updated.v1`、`customer_context.objective.changed.v1`、`customer_context.entity_list.changed.v1`、`customer_context.qualification.expiring.v1`、`customer_context.policy.drafted.v1`、`customer_context.policy.activated.v1`、`customer_context.policy.rolled_back.v1`、`customer_context.knowledge.changed.v1`。

**消费**：`identity.approval.decided.v1`（策略激活审批结果）、`learning.policy.proposal_created.v1`（学习子系统提出的策略调整建议，转为草案）、`learning.simulation.completed.v1`（附在草案上）。

## 8. 扩展点

- 应用领域包：目标模板、默认策略模板、可用字段与函数；
- 行业包：产品分类、能力模板、资质类型、词表；
- 档案字段扩展：通过命名空间化的扩展字段，不修改核心 Schema。

## 9. 伸缩与可靠性

- 读多写少；`detection` 通过本地缓存加载激活策略，策略版本变化以事件驱动缓存失效；
- 档案与清单快照缓存键至少包含租户、组织单元与上下文版本，不同单元的合成快照互不复用；单元覆盖与档案、清单共用上下文版本：`profile.updated`、`entity_list.changed` 事件带版本，只用于提前刷新；正确性靠 detection 求值前读取 `GetContextVersion`，事件丢失或迟到不影响结果；
- 策略编译失败不影响当前生效版本。

## 10. 安全要点

- 所有数据为租户私有，启用行级安全；
- 知识资产按分级决定能否进入模型上下文以及允许的模型路由；
- 策略激活、档案中影响判断的关键字段变更需要授权角色，变更全量审计；档案是客户事实，变更即时影响新求值而不走策略审批，保存前展示受影响的目标与规则，已有信号按原快照版本解释。
