# 策略 DSL

> 状态：评审中 · 版本：1.6 · 更新：2026-10-17 · 负责人：首席架构师、招投标领域负责人

策略决定“一条领域事实对某个租户的某个商业目标意味着什么”。本文是策略 DSL 的唯一规范：结构、求值语义、CEL 环境、编译与下推、生命周期和限制。机器可读定义放在 `contracts/policy/`（`policy.schema.json` 文档结构、`cel-environment.yaml` 变量、函数、规模上界与限制），与本文一致；编译与求值实现在 `services/kernel/policy`，`customer-context`、`detection`、`learning` 共用。

| 使用方 | 用途 |
|---|---|
| `customer-context` | 拥有策略：存储、校验、编译、版本、激活与回滚；自然语言调优生成草案 |
| `detection` | 加载激活版本，对候选事实求值，产出处置与信号 |
| `search` | 承载由策略下推的订阅反查查询 |
| `learning` | 直接用 `kernel/policy.Compile` 与 `Compiled.Evaluate` 做历史仿真，不调用 detection |
| 应用领域包 | 提供策略模板、事实视图、特征函数与可用参数 |

## 1. 设计目标

| 目标 | 设计手段 |
|---|---|
| 业务人员可用 | 绝大多数租户只填写模板参数；表单与自然语言是默认入口，表达式只在高级模式中出现 |
| 结果可解释 | 每条规则有编号与业务说明；求值留下逐条轨迹（真 / 假 / 未知） |
| 缺失信息不误判 | 字段缺失或证据不达标时取值为“未知”，由 `missing_policy` 明确处理 |
| 确定、可重放 | 求值是输入快照上的纯函数，相同输入产生相同输出 |
| 召回不丢失 | 下推到订阅反查的查询只会比策略更宽，不会更窄 |
| 安全 | 租户写的表达式不能访问任何未声明的数据、不能有副作用、受成本上限约束 |

## 2. 概念与归属

```text
Objective（应用领域包定义，例如 APP-TENDER · A02 进入供货）
  └─ PolicyTemplate（应用领域包提供，版本化）：参数 Schema、默认规则、锁定规则、事实视图
       └─ TenantObjective（租户启用的目标）
            └─ Policy → PolicyVersion（租户的策略：模板引用 + 参数 + 追加规则）
                 └─ CompiledPolicy（编译产物：CEL 程序、下推查询、视图需求、哈希）
```

- 一个 `TenantObjective` 只有一个策略；同一事实对不同目标分别求值，互不影响；
- 用户显式创建的关注（关注某组织、某项目）不使用本 DSL，由 `detection` 的固定关注模板处理；
- 判断框架（`assessment`）与机会路径条件（`pursuit`）同样使用 CEL 与同一平台函数库，但各有自己的结构，不属于本文；判断与监测共用输入的快照与版本解释见 [第 9 节](#9-判断输入与版本解释权)。

## 3. 文档结构

策略以 YAML 编写、以 JSON 存储，结构由 `contracts/policy/policy.schema.json` 校验。

```yaml
policy:
  objective: APP-TENDER.A02             # 所属商业目标
  template: TPL-TENDER-A02-SUPPLY@1.2   # 模板与版本
  params:                               # 模板声明的参数
    categories: [CAT-PIPE-PE, CAT-PIPE-PVC]
    regions: ["330000", "320000"]       # 行政区划代码，含下级
    min_contract_amount_cny: 5000000
  scope:
    fact_types: [tendering.bid_result, tendering.contract_record, investment.construction_permit]
  require:
    - id: R-CAT
      say: 标的或项目包含我方产品品类
      when: category.any_match(fact.categories, params.categories)
      missing: review
    - id: R-REGION
      say: 项目位于服务区域
      when: region.within(fact.location, params.regions)
      missing: review
  exclude:
    - id: X-BLACKLIST
      say: 中标方在我方禁合作名单
      when: fact.winner.org_id in lists.blocked
      reason: BLOCKED_PARTNER
  watch:
    - id: W-PERMIT-EARLY
      say: 施工许可已发放但尚无中标方信息
      when: event.type == "investment.construction_permit.issued.v1" && !has(fact.winner)
  boost:
    - id: B-AMOUNT
      say: 合同金额较大
      when: money.cny(fact.amount) >= params.min_contract_amount_cny
      weight: 20
    - id: B-KNOWN-PRIME
      say: 中标方是我方已合作总包
      when: fact.winner.org_id in lists.customers
      weight: 15
  penalize:
    - id: P-CREDIT
      say: 中标方近一年有失信或重大处罚记录
      when: features.winner_credit_risk >= 2
      weight: 25
  evidence_policy:
    default: { min_confidence: 0.8, methods: [rule, model, manual] }
    fields:
      fact.amount: { min_confidence: 0.9 }
  thresholds:
    base_score: 50
    pursue_at: 70
    drop_below: 20
  route:
    pursue_candidate:
      notify: { audience: [objective_owner], mode: immediate }
      research: { template: RT-TENDER-WINNER, auto: true }
    review_candidate:
      notify: { audience: [objective_members], mode: digest }
  examples:
    - fact: ntc_01JA8Z...           # 带期望结果的样例，编译与仿真时校验
      expect: pursue_candidate
```

### 3.1 子句

| 子句 | 内容 | 必填 |
|---|---|---|
| `objective`、`template` | 所属目标与模板版本 | 是 |
| `params` | 模板参数，按模板参数 Schema 校验 | 按模板 |
| `scope` | 适用的事实类型（必须在模板允许范围内），可附加 `when` 条件 | 是 |
| `require` | 硬条件：全部为真才可能成为候选 | 否 |
| `exclude` | 排除条件：任一为真即排除，必须给出 `reason` 原因代码 | 否 |
| `watch` | 观察条件：只观察不行动（如前置信号） | 否 |
| `boost` / `penalize` | 排序调整，带 `weight` | 否 |
| `evidence_policy` | 字段可被规则使用的证据门槛 | 否（取模板默认） |
| `thresholds` | 评分到处置的阈值 | 否（取模板默认） |
| `route` | 各处置的后续动作：`notify`（`audience` 取 `objective_owner`、`objective_members` 或 `role:<代码>`，`mode` 取 `immediate` / `digest`）、`research`（研究模板与是否自动发起） | 否（取模板默认） |
| `examples` | 样例与期望处置：引用真实事实（`fact`）或内联视图（`given`，用于模板自测） | 否 |

### 3.2 规则

每条规则的字段：

| 字段 | 说明 |
|---|---|
| `id` | 策略内唯一，前缀表示子句（`R-`、`X-`、`W-`、`B-`、`P-`）；模板规则带模板前缀，不可与租户规则冲突 |
| `say` | 业务说明，用于解释与界面展示；可引用参数（如 `截止前不少于 {params.min_days} 天`） |
| `when` | CEL 表达式，结果必须为布尔型（编译期类型检查） |
| `missing` | 仅 `require`：取值为未知时的处理，`reject` / `review` / `ignore`，默认 `review` |
| `on_fail` | 仅 `require`：为假时的结果，`no_signal`（默认）或 `excluded`（需同时给出 `reason`） |
| `reason` | `exclude` 与 `on_fail: excluded` 必填，原因代码来自应用领域包的原因代码表 |
| `weight` | 仅 `boost` / `penalize`：正数 |
| `ignore_requires` | 仅 `watch`：判定该观察规则时忽略的硬条件 `id` 列表（默认不忽略任何硬条件） |
| `locked` | 仅模板规则：租户不可删除或修改（如合规限制） |

### 3.3 模板与租户策略的合成

1. 模板提供参数 Schema、默认规则、锁定规则、默认 `evidence_policy` / `thresholds` / `route`、事实视图；
2. 租户策略 = 模板 + 参数 + 追加规则 + 对非锁定规则的覆盖（以规则 `id` 对齐）；
3. 合成后的完整策略文档随版本保存，求值与解释都基于合成结果，模板升级不影响已激活版本；
4. 模板新版本发布后，租户可在策略实验室中看到差异并仿真后升级；
5. **启用领域集合随版本固定**：策略版本提交仿真时，按当时各信息领域的服务状态（见[可扩展性 4](04-extensibility.md#4-加载版本与发布)）确定本版本实际读取的信息领域集合，与合成文档一起保存，仿真报告与审批材料引用同一集合。之后信息领域状态提升，已激活版本不变：集合外领域的视图仍读取为未知并执行原 `missing` 处理，集合外领域的事件不触发本版本重新求值，回填与重放也按该集合执行。领域上线只让策略实验室提示“可升级”；把新领域纳入集合须形成新版本，重新仿真并经审批激活后才生效，未重新激活的策略保持原行为。 视图声明的历史窗口在仿真时未被覆盖的，在该版本中同样固定为未知，回填完成后只提供升级（见[可扩展性 4](04-extensibility.md#4-加载版本与发布)）。
6. **个人租户只用模板策略**（2026-10-17 决定）：档案主体类型为个人的租户，策略只能由模板合成，只调整模板中标为个人可调的参数（白名单），不追加规则、不覆盖规则。这类调整同样形成新版本并仿真，但不经审批即可激活：个人租户只有本人一名成员，无法满足申请人与批准人分离，而白名单参数的取值约束由模板控制。超出白名单的修改不对个人租户开放。个人客户何时进入产品范围、冷静期等其他事项另行决定，不由本条推出。

### 3.4 模板结构

模板是应用领域包的文件（例如 `packs/application_domains/tendering/policy/templates/`），模板内容以这些文件为准。模板在策略文档结构之外增加：

| 字段 | 说明 |
|---|---|
| `template.id` / `version` / `objective` | 模板标识、语义化版本、所属商业目标 |
| `roles` | 默认启用该模板的价值链角色 |
| `fact_types` | 允许进入 `scope` 的事实类型（租户只能收窄） |
| `params` | 参数定义：类型、默认值、`default_from`（从客户上下文取默认值，如 `profile.offering_categories`，避免在策略中复制档案数据）、取值约束；个人可调标记（第 3.3 节第 6 条）随个人客户进入时加入模板格式 |
| `view` | 需要的视图项名称，引用应用领域包的视图目录（第 5.1 节） |
| `rollout_key` | CEL 表达式，返回比例灰度的哈希键（第 7 节） |
| 规则与默认子句 | 与策略文档相同，模板规则可标 `locked` |

同一应用领域包内共享的视图目录、特征函数与原因代码表放在模板旁的独立文件中，模板按名称引用。

## 4. 求值语义

### 4.1 三值逻辑

每条规则的结果为 **真 / 假 / 未知**。以下情况使被引用的字段成为“未知”：

- 字段不存在或为空；
- 字段的证据不满足 `evidence_policy`（置信度不足、方法不在允许列表、质量状态为可疑）；
- 视图中所需的实体或特征尚未就绪。

字段存在性与取值分开处理：

- `has(x)` 判断字段是否存在，始终返回真或假，用于“尚无中标方”这类显式判断；
- 读取不存在的字段时，CEL 产生错误值；CEL 的 `&&`、`||` 与条件运算会吸收不影响结果的错误（例如 `false && 错误` 为假），最终结果仍为错误时，规则结果记为未知；
- 证据不达标的字段以 cel-go 部分求值的未知属性注入：`has()` 为真，取值为未知，传播方式同上；
- CEL 运行时的其他错误（如类型不符）在编译期已被类型检查排除，运行期仍出现时按未知处理并告警。

`scope.when` 为未知时按不满足处理。

### 4.2 处置决策

对每个候选事实，按以下顺序确定处置，处置定义见 [领域模型](../02-business/application-domains/tendering/04-domain-model.md#4-处置模型)：

```text
1. scope        不满足 → no_signal（结束）
2. require      逐条求值，得到真 / 假 / 未知
3. exclude      任一为真：硬条件中没有为假的 → excluded（记录 reason，结束）；否则 → no_signal（结束）
                为未知的排除规则不生效，记入轨迹
4. watch        观察规则为真，且其未忽略的硬条件中没有结果为假的 → watch_signal（结束）
5. require 结果 任一为假 → on_fail（no_signal 或 excluded，结束）
                为未知 → 按 missing：reject → no_signal（结束）；review → 记“需复核”；ignore → 视为通过并记入轨迹
6. score        base_score + Σ boost.weight − Σ penalize.weight（只计结果为真的规则）
7. disposition  score < drop_below → no_signal
                存在“需复核” → review_candidate
                score ≥ pursue_at → pursue_candidate
                否则 → review_candidate
8. route        按最终处置取 route，作为信号的后续动作建议
```

说明：

- 只有在硬条件不为假（本应相关）时才记录 `excluded`，避免与目标无关的事实产生排除记录；排除优先于观察；观察规则同样受硬条件约束（例如服务区域），但前置信号缺少的字段（如尚无标的、截止时间）在三值逻辑下为“未知”而非“假”，因此不会阻止观察；确需绕过某条硬条件时用 `ignore_requires` 显式声明；
- `no_signal` 不产生信号，只计入匹配统计；`excluded` 产生带原因的信号记录，在收件箱中默认隐藏，可筛选查看并用于仿真与反馈；
- `score` 只用于同一策略下的排序，与 `assessment` 的 `rank_score` 一样，不代表成交概率；
- `route` 只是输出：通知由 `channels` 依据信号事件执行，自动研究由 `assessment` / `research` 发起并受 `commerce` 配额约束，`detection` 不直接调用它们。

### 4.3 确定性与记录

求值输入固定为：事实版本（求值单位为聚合事实时取其投影版本，如采购项目的当前状态版本）、视图快照、客户上下文快照版本、策略版本（含其固定的启用领域集合，见 3.3）、逻辑求值时间。逻辑求值时间取触发事件的时间（回填与仿真时取被回放事件的时间），表达式中的 `now` 即此值，因此重放结果不随实际运行时间变化。

求值单位的当前状态变化时，按变化后的状态重新求值；处置变化以信号更新记录，不新建信号。信号按（模板、求值单位）去重，求值单位由视图目录的 `unit` 声明，触发重新求值的领域事件由视图的 `triggers` 声明，且只限策略版本启用领域集合内的视图。

公告按原文表达，更正、澄清是独立公告，不改写被更正公告。需要“应用更正后的现状”的模板（如 A01 直接投标）以采购项目标包为求值单位，读取投影器应用全部更正后的当前状态；`tendering.notice.amended.v1`、`tendering.deadline.changed.v1` 等事件只是重新求值的触发器。以单条公告为单位的视图只用于公告本身即为判断对象的场景（如结果公告、合同公告）。

信号记录：策略版本与启用领域集合、处置、得分、逐条规则轨迹（结果与未知字段）、命中规则的 `say` 渲染文本、视图快照哈希。`ExplainSignal` 直接基于该记录展示。

## 5. CEL 环境

### 5.1 变量

表达式只能访问以下变量，全部为只读快照：

| 变量 | 内容 | 来源 |
|---|---|---|
| `fact` | 事实视图：领域事实字段，以及模板声明需要的关联实体属性（如 `fact.winner.org_id`、`fact.buyer.type`） | 信息领域包的领域 Schema + 模板的视图声明 |
| `event` | 触发事件：`type`、`changed_fields` 等 | 领域事件 |
| `profile` | 客户上下文快照（按目标所在组织单元合成）：我方主体 `org_ids`、产品品类、服务区域、品牌，以及按主体的资质状态表 `entities`；字段以[上下文快照契约](../../contracts/schemas/intelligence/context-snapshot.schema.json)为准 | `customer-context.GetContextSnapshot` |
| `lists` | 租户私有实体清单，按种类（见 [customer-context](subsystems/customer-context.md#4-拥有的数据租户域)）为组织标识集合，如 `lists.competitors`。租户私有关系（`TenantRelationship`，如经销授权的品牌、区域与期限）尚未进入表达式环境；进入时须同时定义其快照（随上下文版本固定）、规模上界与重放语义，不预留变量名 | `customer-context` |
| `vocab` | 行业包词表与同义词 | 行业包 |
| `features` | 应用领域包特征函数的计算结果 | 应用领域包 |
| `params` | 模板参数 | 策略 |
| `now` | 逻辑求值时间 | 见 4.3 |

**视图声明**：应用领域包维护视图目录（每项声明提供方接口、类型与新鲜度），模板按名称声明本目标需要的视图（例如“中标方的信用标记”“采购人的历史采购摘要”）。`detection` 在求值前按声明向数据所有者查询并组装视图、记录快照；表达式内部不能发起任何查询。需要新的关联数据时，修改模板的视图声明，而不是增加可查询的函数。

**模型产出的视图项**：视图项的值可以来自模型（例如增强层的品类分类、实体风险标记、判断中的模型评分）。这类视图项在视图目录中除提供方接口外，还声明产出者：基础模型与提示的版本引用 `ai-platform` 的模型目录与提示注册，业务评分的定义与版本引用 `assessment` 的判断框架或评分模型；产出者以快照提供，快照记录产出者版本、置信度与生成时间，进入视图快照与信号记录。不另建模型注册表。置信度未达到 `evidence_policy` 要求时视图项读取为未知；产出者版本变化视为视图定义变化，按 3.3 形成新的策略版本才生效，已激活版本继续读取其固定版本的快照。前期只通过 API 推理（[ADR-0015](../04-decisions/0015-model-inference-via-api.md)），自训模型的产出者声明另定范围。

视图的求值单位与进入条件由应用领域包在视图目录（如招投标包的 `policy/views.yaml`）中声明，领域结果语义随声明维护，本文不重复；本地契约检查不证明生产投影器已实现。

### 5.2 平台函数库（初版）

| 函数 | 说明 |
|---|---|
| `category.any_match(categories, codes)` | 品类列表（视图 `fact.categories`，来自增强层品类分类；也接受带 `categories` 的标的列表）是否命中品类代码集合（含下级：代码按“-”分级，如 `CAT-PIPE` 含 `CAT-PIPE-PE`）；只比较行业包分类体系（`scheme` 以 `industry:` 开头）的代码，未标明体系或政府采购品目等其他体系不参与。品类置信度经视图证据受 `evidence_policy` 约束，不达标时为未知 |
| `region.within(location, regions)` | 地点是否在行政区划集合内（含下级：`XX0000` 含全省，`XXXX00` 含全市，`000000` 为全国）。只比较同一代码表版本：事实地点与租户区域的版本不同时，视图组装按版本化换算表换算到租户区域所用版本，无法换算（撤并、拆分无唯一对应）时地点读取为未知；`fact.location` 是项目实施地，交付地、采购人所在地按各自字段与角色另行声明 |
| `text.contains_any(text, vocab_terms)` | 文本是否包含词表中任一词（含同义词） |
| `money.cny(amount)` | 金额归一为人民币元；非人民币金额在汇率视图就绪前为未知 |
| `time.days_between(a, b)` | 从 a 到 b 的整天数（向下取整，可为负） |
| `sets.overlaps(a, b)` | 两个集合是否相交 |

另含 CEL 标准库（比较、逻辑、`in`、`has`、列表宏 `exists` / `all` / `filter` 等）。函数必须是纯函数；新增函数经架构评审，在 Go 中实现并在 `contracts/policy/` 中声明签名。

### 5.3 特征函数

特征函数由应用领域包以 CEL 声明，输入为事实视图与客户上下文视图，输出为数值、布尔或枚举（例如 `features.winner_credit_risk`、`features.deadline_days`）。特征按需惰性计算、每次求值内缓存，计算结果写入信号的特征快照，供 `assessment` 复用。

## 6. 编译与下推

`customer-context` 的 `policy_engine` 在保存草案时编译，产物为 `CompiledPolicy`：

| 步骤 | 检查或产出 |
|---|---|
| 结构校验 | JSON Schema；参数符合模板参数 Schema；规则 `id` 唯一；锁定规则未被修改 |
| 类型检查 | 每个 `when` 在 CEL 环境中检查，结果必须为布尔（视图变量为动态类型，结果类型无法静态确定时在运行期确认，非布尔按未知处理并告警）；引用的特征、清单、词表存在 |
| 成本检查 | 每条表达式的静态成本估算不超过上限；整份策略的规则数与表达式长度不超过上限（第 8 节） |
| 下推编译 | 生成订阅反查查询（见下） |
| 样例校验 | 对 `examples` 求值，结果不符则草案不能提交激活 |
| 产物 | CEL 程序、下推查询、视图需求、内容哈希 |

**下推规则**（保证召回）：

1. 只下推 `scope` 与 `require` 中能直接映射为索引字段的原子条件（事实类型、品类代码、区划代码、词表命中、金额区间）；`on_fail: excluded` 的硬条件不下推，以保证这类排除记录仍能产生；
2. 多个原子条件以“与”组合；含“或”、否定、特征或视图字段的条件不下推，等同于从查询中删除（只会让查询更宽）；
3. 每个下推条件写成“字段缺失 **或** 条件成立”，保证字段缺失的事实仍成为候选，交给 `missing` 处理；
4. `exclude` 与评分不下推；每条观察规则单独生成一条查询，只包含其未忽略的硬条件原子与自身可下推的原子，避免被其忽略的硬条件过滤。

不变量：对任一事实，策略求值结果不是 `no_signal` ⇒ 下推查询命中（`excluded` 只在硬条件不为假时产生，因此同样被覆盖）。CI 用属性测试验证编译器满足该不变量。

## 7. 生命周期

```text
草案 ──编译通过──► 已校验 ──仿真（learning）──► 待审批 ──审批（identity）──► 激活
                                                                    │
                        影子：新版本求值并记录，不产生可见信号；用于比对
                        比例：按模板 rollout_key 的哈希把指定比例交给新版本（招投标模板取项目聚合标识，无项目时取主体组织标识），同一项目的多次公告落在同一版本
                        全量：新版本完全生效
                                                                    └──► 回滚：重新激活上一版本
```

- 每个 `TenantObjective` 同时最多有一个生效版本和一个灰度版本（影子或比例）；
- 激活与回滚是高影响动作，需授权审批（见 [identity](subsystems/identity.md)），提交时必须附仿真报告（见 [质量与验收](../05-delivery/02-quality-and-acceptance.md#8-发布门禁)）；
- 激活后 `detection` 通过 `customer_context.policy.activated.v1` 刷新缓存；新版本可选择触发回填（`RunBackfill`）。

### 7.1 编辑入口

| 入口 | 适用者 | 产出 |
|---|---|---|
| 表单 | 所有有策略权限的用户 | 参数修改、规则启停、权重调整 |
| 自然语言调优 | 所有有策略权限的用户 | `policy_assistant` 经模型网关把描述转为对策略文档的结构化补丁（JSON Patch），输出受 Schema 约束，随后同样经过编译、样例校验与仿真；模型不能直接激活 |
| 高级模式（直接编辑表达式） | 租户管理员、妙洽分析师 | 追加或修改规则表达式 |

所有入口产出同一种草案，差异以“规则级差异 + 仿真差异”展示。

## 8. 限制与安全

| 项 | 上限（初值，按性能测试调整） |
|---|---|
| 每份策略的规则数（合成后） | 200 |
| 单条表达式长度 | 2 000 字符 |
| 单条表达式静态成本 | 500 000（按 `cel-environment.yaml` 的规模上界估算） |
| 单次事实求值的运行时成本 | 1 000 000；超出时中止该事实的求值，不产生信号，记录并告警 |
| 每个租户的激活策略数 | 等于启用的目标数 |

成本单位为 cel-go 成本单位，平台函数的成本模型见 `services/kernel/policy`。初值依据 R1 模板的基准测试：在规模上界下（标的 500 项、清单每种 2 万个组织、词表 1 000 词等），最大静态成本 296 074（A01 `features.qualification_gap`，我方主体 10 个 × 资质要求 200 项按主体逐项判断；此前最大为 `X-TERM` 标题匹配排除词表的 200 005），最坏运行时成本 207 728，单次求值最坏约 13 ms；样例规模的单次求值为 25–55 µs。静态上限约为 R1 最大值的 1.7 倍，运行时上限约为 5 倍。CI 校验 R1 模板在上限之内。

安全要求：

- 表达式只能读取第 5.1 节的变量，不能发起查询、访问网络或产生副作用；
- 事实视图中不包含其他租户的任何数据；`lists`、`profile` 只来自本租户；
- 策略文档是租户“内部”级数据，进入自然语言调优的模型调用时按该分级路由。

## 9. 判断输入与版本解释权

监测回答是否进入处理队列，assessment 回答是否值得行动；两者使用同一 CEL 与平台函数库，但判断框架不是策略处置规则。由信号触发的判断默认采用 `signal_snapshot`：固定信号引用的事实、策略、客户上下文、视图/特征定义、函数库版本与逻辑求值时间，复用已记录的特征快照。缺失特征只能按同一版本输入补算，无法恢复则标为未知/需复核，不能静默读取最新值。

用户显式重评、事实/策略/上下文变化或新研究完成可触发 `refreshed_snapshot`：重新固定全部输入，沿同一版本求值机制计算，产生新 Assessment，记录 `input_mode`、原信号/原判断引用、新旧版本及差异原因。新研究可增加判断依据，但不能覆写信号原有特征。相同输入与定义下出现不同结果是缺陷；不同版本下的差异是新判断，旧信号仍解释发现时状态。新判断不自动覆盖已批准 Decision，也不自动替换正在审批的草案；草案刷新须使旧审批失效并重新提交。

learning 仿真直接调用 kernel/policy，不调用 detection.DryRunPolicy；固定数据集、策略、客户上下文、视图/特征定义、函数库版本及事件逻辑时间。历史仿真与在线求值的差异应来自显式版本或输入差异，不能来自不同的规则实现。

共享 kernel/policy 已实现纯编译/求值与特征输出；信号输入版本持久化、assessment 的两种输入模式、审批失效处理及 learning 仿真编排属于各业务子系统后续实现与契约验收。本节定义实现约束，不代表这些骨架已经完成业务运行；不能用导入规则通过代证快照复用、重评或评估发布门禁。

## 10. 待定事项

| 事项 | 说明 | 决定时点 |
|---|---|---|
| 跨分类体系的品类映射 | `category.any_match` 目前只按代码分级匹配；行业包提供分类映射后加入 | 随首批行业包 |
| 函数库扩展 | R1 模板只用到第 5.2 节的 6 个函数与 CEL 标准库；后续批次按需评审新增 | 随 R2 模板 |
