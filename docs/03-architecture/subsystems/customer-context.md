# 客户上下文与策略子系统（customer-context）

> 状态：评审中 · 版本：1.12 · 更新：2026-10-26 · 平面：客户智能

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
| `TenantObjective` | 启用的目标：模板与版本、所属组织单元、负责人、参与人、参数（随档案或本目标值）、状态（未激活 / 生效 / 已停用）、关联的策略、分派规则（第 11 节）；同一单元同一模板只启用一次 |
| `EntityListImport` | 企业清单导入作业：文件、逐行匹配结果（已匹配 / 待确认 / 需选择 / 未匹配）、状态；匹配完成不写入，操作人看过影响提示并提交后，已匹配与已确认的行才写入清单 |
| `Policy` / `PolicyVersion` / `PolicyRollout` | 策略、版本、发布状态。`PolicyVersion` 保存合成文档、模板版本与仿真时固定的启用领域集合，状态为生效或已替代；每个目标有一个策略代次，激活与回滚在同一事务中写入新的生效版本并递增代次。M2 只有全量激活，影子与比例灰度随 M3 |
| `PolicyDraft` | 每个目标最多一份草案：修订号与乐观并发版本号、编译与样例校验结果、所基于的生效版本；提交激活申请后锁定，撤回、驳回或失效后解锁；回滚申请不锁定草案（[PRD 第7段](../../01-product/prd/07-policy-simulation-approval.md)） |
| `KnowledgeAsset` | 知识资产：分级、可用于哪些工作流与模型 |
| `SavedView` / `UserViewDefault` | 工作台列表的保存视图：列表种类、作用域（目标或组织单元）、名称、所有者、可见范围（个人 / 团队）、条件、排序与列配置、版本；成员在某列表与作用域上的默认视图。只保存条件不保存结果，打开时按查看者权限执行；团队视图的权限与上限见 [Web 专项设计第6.2节](../14-web-application.md#62-保存视图的所有者) |
| `TenantEntityList` | 租户私有实体清单，种类：`customers` 客户、`partners` 伙伴、`competitors` 竞争对手、`blocked` 禁合作、`tracked` 跟踪对象、`counterparties` 交易对手；策略中以 `lists.<种类>` 引用。清单只表达“是否在名单中”，是部分关系的简化视图：例如有效的经销授权可派生出 `partners` 成员，但授权范围与期限只在 `TenantRelationship` 中 |
| `TenantRelationship` | 租户私有关系（与 entity-graph 的公共关系分开，公共域不读取）。种类先登记经销授权 `dealer_authorization`、代理 `agency`、合作伙伴 `partnership`、长期供货 `supply_agreement`，由应用领域包扩展。**端点**：我方主体与对方组织（公共 `org_`，导入时未匹配的行暂存为待核对，不建关系）；**约束**：品牌（`brd_` 或行业包品牌词表代码）、区域（区划代码与版本）、品类，可进一步限定项目或标包；同一端点在不同约束下是不同的关系。**有效期**：起止日期，到期自动失效；**证据**：授权书等文件经 evidence 文档处理保存。**修订与撤销**：每次修改形成新版本并保留旧版本，撤销记录原因与操作人，已有信号按发现时的上下文版本解释；与公共关系冲突（例如授权的品牌归属与 entity-graph 不一致）时并列展示，不自动覆盖任何一方。导入的“备注/属性”列只作补充说明，不能代替结构化的品牌、区域与期限 |

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
| `GetPolicy` / `ListPolicyVersions` / `DiffPolicyVersions` | 查询 | 生效版本、草案、版本历史与规则级差异 |
| `DraftPolicyChange` / `DiscardPolicyDraft` | 命令 | 结构化（M2 表单）或自然语言（M3）输入 → 草案修订与差异，带 `expected_version`；保存即合成、编译并运行模板样例，发布 `policy.drafted` |
| `RequestPolicyActivation` / `RequestPolicyRollback` | 命令 | 按类型绑定后调用 `identity.RequestApproval` 创建审批，带幂等键；同一目标同时只有一个进行中的申请。激活：绑定草案当前修订、该修订未过期的仿真报告（对比版本须为当前生效版本）与申请时的生效版本和策略代次，提交后锁定草案。回滚：绑定目标版本（曾经生效过）、其激活时的报告与申请时的生效版本和策略代次，不绑定、不锁定草案 |
| `ActivatePolicy` / `RollbackPolicy` | 命令 | 审批通过后由系统执行，执行前核验绑定：激活要求草案仍是申请的修订、生效版本与策略代次仍等于基准；回滚要求目标版本存在且曾经生效、生效版本与策略代次仍等于基准。不满足则不执行并通知。回滚恢复目标版本原来的合成文档与启用领域集合；执行后若有草案，其报告的对比基准失效，标为过期 |
| `CreateUpgradeDraft` | 命令 | 模板新版本或信息领域上线后，按规则 `id` 对齐保留租户覆盖创建草案；被删除或改为锁定的覆盖逐条列出待确认 |
| `GetActivePolicies` | 查询 | `detection` 加载，返回策略代次；detection 每批求值前读取代次，缓存较旧时先重新加载（[PRD 第7段](../../01-product/prd/07-policy-simulation-approval.md) 第5节生效边界） |
| `UploadKnowledgeAsset` | 命令 | 上传与分级 |
| `ListSavedViews` / `CreateSavedView` / `UpdateSavedView` / `DeleteSavedView` / `SetDefaultView` | 查询/命令 | 保存视图（第4节）；更新带 `expected_version`；团队视图的变更写审计。契约随第一个使用保存视图的 Web 实现写入（[契约覆盖第9节](../13-contract-readiness.md#9-随功能追加的契约)） |

实现状况：首个切片实现了 `GetContextSnapshot`、`GetContextVersion`（内部端口，经 mTLS）、`Get/Update Profile`（本企业关联与资质在档案内维护）与目标的 `ListObjectiveTemplates`、`EnableObjective`、`ConfigureObjective`、`DisableObjective`（含重新启用），契约为 [`contracts/apis/customer-context/openapi.yaml`](../../../contracts/apis/customer-context/openapi.yaml) 0.2.0 草案。组织单元为 identity 的 `OrgUnit`（根单元 `root` 或 `unt_<ULID>`），经 identity 核对属于本租户并取得单元链；单元覆盖实现前，非根单元的快照与根单元合成结果相同，首次读取时按当时的单元链物化，之后同一（租户、单元、上下文版本）不变。授权全部经 identity 的 `Check`：启用目标在目标单元上判断 `can_create_objective`，查看、调整与参与人管理在目标资源上判断；目标的单元、负责人与参与人在每个目标版本上以服务主体经 `SyncResourceParticipants` 同步（参与版本即目标版本；负责人同时是参与人时只同步负责人关系），命令提交后同步一次，失败时由本子系统消费 `objective.changed` 重试。负责人与参与人合计的不同主体数至多 50（等于 identity 单次同步的关系上限），在命令提交前校验，超出返回 422 `too_many_participants`、不入库、不产生事件。identity 中的参与关系可能落后于本地目标版本，因此可经参与关系获得的权限（目前为查看）按“目标所在单元的 `can_list_objectives`，或调用者是本地当前版本的负责人/参与人且 identity 对目标判断通过”放行：移除参与人、改派负责人在命令提交后立即撤权，不等待同步；新加入的主体在同步应用前不可见；首个版本同步前 identity 尚未登记目标，目标上的判断一律拒绝（失败关闭）。决策者、研究分析者与管理员的单元权限不受同步影响。目标列表与读取同一口径。我方主体可挂接本租户的任一单元（领域校验只核对标识格式，归属经 identity 核对）。快照与版本的服务读取方由 identity 判断 `can_read_context`，本子系统不保存名单。启用目标时在同一事务中创建首版策略草案（第7段，见下）。

**实现状况（PRD 第7段策略草案、版本、激活与回滚，契约 0.3.0 草案）**：

- **接口**：`GetPolicy`、`DraftPolicyChange`（`PUT …/policy/draft`，带 `expected_version` 与幂等键）、`DiscardPolicyDraft`、`ListPolicyVersions`（每页至多 100，游标）、`GetPolicyVersion`、`DiffPolicyVersions`（`from`/`to` 为版本号或 `draft`）、`RequestPolicyActivation`、`RequestPolicyRollback`；learning 经本地绑定 `customer-context.PolicyReader`（`MIAOQIA_BINDING_CUSTOMER_CONTEXT=remote` 时为远程客户端，同时提供 `ContextReader`）或内部端口 `GET /internal/v1/customer-context/objectives/{objectiveID}/policy/simulation-input` 读取某个修订的仿真输入（合成文档、需要的信息领域、样例结果与当前生效版本）。内部接口 `GET /internal/v1/customer-context/objectives/{objectiveID}/access`（本地绑定 `customer-context.ObjectiveAccess`）以转交的用户请求上下文按本节口径判断目标关系，供 learning 的仿真接口使用，使其与本子系统同样在移除参与人后立即撤权；`CreateUpgradeDraft` 与自然语言调优未实现（`GetActivePolicies` 见下）；
- **授权**：读取为目标的 `can_view`，起草与申请为 `can_draft_policy` / `can_request_activation`。identity 模型新增单元关系 `org_unit#can_draft_unit_policies`（决策者，identity 第 7.2 节矩阵），本子系统按“单元关系，或本地当前版本的负责人/参与人且 identity 对目标判断通过”放行，与第6节的查看口径一致。无权查看与不存在同为 404 `objective_not_found`；能查看而不能起草为 403；授权服务不可用为 503，不当作无权限；
- **草案**：保存即按模板约束校验表单（权重 1–50、`drop_below < pursue_at`、锁定规则不可停用或修改、通知受众只能是负责人、参与人或范围内业务角色；不满足时 422 且不保存），合成并用 `kernel/policy` 编译、运行模板样例；编译或样例不通过的修订照常保存（`valid=false` 与问题列表），不能仿真与申请。每次保存产生新修订（修订号单调递增）与 `policy.drafted`。修订是不可变的，申请与审批绑定修订号而不是草案本身。`ConfigureObjective` 的参数变更未自动形成草案（缺口）；没有草案的旧目标在首次整表保存时创建；
- **申请**：先在本库核对前置条件（草案修订等于 `revision`、未锁定、通过校验、该修订的仿真已完成且对比一侧等于当前生效版本与代次；回滚目标版本存在且不是生效版本；目标未停用；同一目标没有进行中的申请），再以申请人的上下文调用 `identity.RequestApproval`（同一幂等键），最后在第二个事务中重核绑定并登记、锁定草案（仅激活）。两个事务之间草案或生效版本变化时返回 409 并以 `CloseApproval` 使刚创建的审批失效；登记失败而 identity 已创建的审批由消费 `identity.approval.requested.v1` 补登或失效（对账）。首次激活的回填天数默认 30（PRD 第13节第2项），之后的激活传入回填为 422 `backfill_not_allowed`；
- **执行**：消费 `identity.approval.decided.v1`，按目标行锁 → 策略行 → 请求行的顺序加锁，按事件中的绑定（不是本地登记）核验：激活要求草案修订仍等于绑定的对象版本、锁定者为空或本审批、修订通过校验、仿真对应该修订与基准；回滚要求目标版本存在、不是生效版本、证据等于该版本激活时的仿真；两者都要求目标未停用、生效版本与代次等于基准。不满足时记为执行失败（`baseline_changed`、`object_changed`、`objective_disabled`），生效版本不变，发布 `customer_context.policy.execution_failed.v1`。决定先于登记送达时直接按事件登记为最终状态，迟到的 `requested` 不再改变状态；重复投递以审批标识幂等。激活写入新版本（最新版本号加一，复制修订的合成文档与该修订仿真时固定的启用领域集合）、原生效版本记为已替代、代次加一、清除草案，目标从未激活或重新启用后的未激活变为生效，发布 `policy.activated` 与 `objective.changed`（`activated`）；回滚恢复目标版本原来的文档与领域集合，发布 `policy.rolled_back`，不锁定也不清除草案，草案报告因对比基准变化而过期；
- **进程角色**：customer-context 加入 `consumer` 角色（`deploy/platform/roles.yaml`），审批与仿真结果的消费者在该角色运行；`services/cmd/platform` 的 `TestEveryRoleResolvesBindings` 对每个角色在本地与远程绑定组合下组装并启动子系统，断言依赖的绑定都可解析；
- **失效**：消费 `identity.approval.closed.v1`，请求记为撤回、过期或失效并解锁草案。目标停用时本子系统在提交后以 `CloseApproval`（`objective_disabled`）使进行中的审批失效；失效未送达而批准先到时，执行前的目标状态核验拒绝执行。重新启用的目标回到未激活并保留原生效版本，下一次激活后变为生效；
- **生效策略读取（PRD 第3段切片，契约 0.4.0 草案）**：`GetActivePolicies`（内部接口 `GET /internal/v1/customer-context/active-policies`，本地绑定 `customer-context.ActivePolicies`，`MIAOQIA_BINDING_CUSTOMER_CONTEXT=remote` 时为远程客户端）以服务主体（`tenant#can_read_context`）读主库，按目标标识升序分页（每页至多 200，游标），返回每个有生效版本的目标的状态、组织单元、策略标识、生效版本与代次、模板版本、启用领域集合与合成文档；激活与回滚在同一事务中切换生效版本与代次，因此返回后开始的读取一律看到新代次。策略标识（`pol_`，迁移 `00003_policy_ids.sql`）每个目标一个、建立后不变，已有的行由目标标识确定性派生。`ObjectiveAccess` 的关系追加 `can_view_signals` 与 `can_feedback`，按本节口径（单元关系 `can_view_unit_signals` / `can_feedback_unit_signals`，或本地当前版本的负责人/参与人且 identity 对目标判断通过）放行，供 detection 与 learning 使用；
- **未实现**：回填的实际执行（随 detection）、通知（随 channels）、升级草案（F7）、按规则显示“数据未接入”（规则到信息领域的生产对应随 domain-intelligence 的覆盖接口）、组织单元迁移时使进行中的审批失效。

**实现状况（PRD 第6段，契约 0.5.0 草案）**：`ObjectiveAccess` 的关系追加 `can_assign`（决策者经单元关系 `org_unit#can_assign_unit_objectives`，业务开拓者经参与关系），并返回目标所在单元、模板与状态（可选字段）；本地绑定与远程客户端另提供 `customer-context.ObjectiveScope`（同一判断，带目标所在单元与模板），供 pursuit 判断创建权限与被分派人的目标访问。分派规则的候选负责人尚未提供给 pursuit。

## 7. 事件

**发布**：`customer_context.profile.updated.v1`、`customer_context.objective.changed.v1`、`customer_context.entity_list.changed.v1`、`customer_context.qualification.expiring.v1`、`customer_context.policy.drafted.v1`、`customer_context.policy.activated.v1`、`customer_context.policy.rolled_back.v1`、`customer_context.knowledge.changed.v1`。

已实现发布 `profile.updated`、`objective.changed`、`policy.drafted`、`policy.activated`、`policy.rolled_back` 与 `policy.execution_failed`（批准后核验绑定不成立、不执行），契约见 [`contracts/events/customer_context/events.asyncapi.yaml`](../../../contracts/events/customer_context/events.asyncapi.yaml) 0.2.0 草案；已实现消费 `identity.approval.requested.v1`（对账补登）、`identity.approval.decided.v1`、`identity.approval.closed.v1` 与 `learning.simulation.completed.v1`（第6节实现状况）；资质有效期跨日改变快照时，`GetContextVersion` 递增上下文版本并以 `cause: qualification_validity` 发布 `profile.updated`。

**消费**：`identity.approval.decided.v1`（策略激活与回滚的审批结果）、`identity.approval.closed.v1`（撤回或失效；激活申请的草案解锁）、`learning.policy.proposal_created.v1`（学习子系统提出的策略调整建议，转为草案）、`learning.simulation.completed.v1`（附在草案上）。

## 8. 扩展点

- 应用领域包：目标模板、默认策略模板、可用字段与函数；
- 行业包：产品分类、能力模板、资质类型、词表；
- 档案字段扩展：通过命名空间化的扩展字段，不修改核心 Schema。

## 9. 伸缩与可靠性

- 读多写少；`detection` 通过本地缓存加载激活策略，每批求值前读取目标的策略代次判断缓存是否过期，`policy.activated`、`policy.rolled_back` 事件带代次，只用于提前刷新，丢失或迟到不影响正确性；
- 档案与清单快照缓存键至少包含租户、组织单元与上下文版本，不同单元的合成快照互不复用；单元覆盖与档案、清单共用上下文版本：`profile.updated`、`entity_list.changed` 事件带版本，只用于提前刷新；正确性靠 detection 求值前读取 `GetContextVersion`，事件丢失或迟到不影响结果；
- 策略编译失败不影响当前生效版本。

## 10. 安全要点

- 所有数据为租户私有，启用行级安全；
- 知识资产按分级决定能否进入模型上下文以及允许的模型路由；
- 策略激活、档案中影响判断的关键字段变更需要授权角色，变更全量审计；档案是客户事实，变更即时影响新求值而不走策略审批，保存前展示受影响的目标与规则，已有信号按原快照版本解释。

## 11. 分派配置

“谁来处理”涉及四个不同的概念，分别由不同所有者负责：

| 概念 | 含义 | 所有者 |
|---|---|---|
| 候选负责人 | 按规则建议的处理人，例如按区域、品类、我方主体或组织单元匹配目标的参与人 | 本子系统拥有分派规则；`detection` 生成信号时按规则算出候选，写入信号，只是建议 |
| 已分派负责人 | 实际承担某件事的人：推进负责人、任务负责人 | 业务所有者执行并记录（`pursuit` 的推进与任务） |
| 通知受众 | 会收到某条通知或某期简报的人 | `channels` 按通知规则确定 |
| 访问权限 | 能否查看或操作某个对象 | `identity`（OpenFGA 关系） |

规则：

- 分派规则随目标配置保存，形成上下文版本；规则只能从该目标的参与人中选人，不能引入目标外的成员；
- 分派写入业务对象时，业务所有者同时在 `identity` 写入参与关系（推进负责人、协作者），并先检查被分派人是否是租户成员、是否有该目标的访问；无权限的分派被拒绝，不自动授予权限；
- 通知受众不能超出访问权限：`channels` 发送前逐人检查，收到通知不等于获得访问，候选负责人也不因被建议而获得访问；
- 改派、成员停用与截止变更按 [pursuit 第 10 节](pursuit.md#10-分阶段交付)处理，本子系统不保存分派历史。
