# A02 真实贯通验收线

> 状态：已批准 · 负责人：项目负责人、首席架构师、产品负责人

本文定义建材供应商 A02 首条旅程从“服务端切片”走到“真实客户可用”的验收线：按“激活策略 → 收到信号 → 查看原文 → 简报触达”梳理真实贯通所需的依赖与分级验收，第4节是 A02 各能力缺口状态的唯一登记处。各段的功能规则以[功能 PRD](../01-product/prd/README.md) 为准，首次使用门禁与当前主线以[阶段基线](06-stage-baseline.md)为准；本文不改变任何门禁。

## 1. 为什么需要这条验收线

第7、3、4、5段的服务端切片都按同一模式交付：接口、事件契约、迁移、实现与测试同时提交，身份、数据库、授权与内部调用用真实组件验证，外部依赖经端口隔离，未接入时失败关闭（`NotWired`，503），测试用夹具。这一模式让每段都可自动验证，但也意味着：

- **每个子系统的缺省装配都是“未接入”**：learning 的数据集、detection 的事实视图、gateway 的数据平面读取、channels 的企业微信适配器、identity 的 IdP 管理接口，在 `Module()` 缺省下都返回不可用，只有 platform 组合装配时才接入生产实现；
- **服务端切片完成不等于旅程可用**：真实数据、真实渠道、界面与客户走查各自独立验收。

因此 M2 的进度同时按两个口径衡量：段的**服务端切片**（PRD 验收表中“部分（服务端）”的覆盖）与链路的**贯通级别**（第3节）。当前贯通级别见第4节开头。

## 2. 贯通链路与依赖

A02 的最短可用链路分四个环节。每个环节列出“真实贯通”还缺什么，编号对应第4节缺口清单。

| 环节 | 用户看到的结果 | 已有（服务端切片） | 真实贯通还缺 |
|---|---|---|---|
| ① 激活策略 | 管理员配置档案与 A02 目标，看仿真报告，提交并批准激活 | 档案与目标、策略草案/版本/差异、仿真与报告、审批服务、激活执行 | 完整仿真数据集与覆盖状态（D-09、D-10）、真实 IdP 与 TOTP（C-03）、审批通知（C-05）、Web 页面（W-02） |
| ② 收到信号 | 新的中标结果公告进入平台后，命中的目标收件箱出现信号，可反馈与批量处理 | 生效边界匹配、收件箱、详情与解释、反馈与批量撤销；回放管线的快照、结果解析、领域事实及事件 | 来源持续采集（D-01，L4）、通用项目/标包聚合、品类增强、领域状态与更正解析（D-02～D-05）、生产订阅反查（D-07）、企业主数据（D-08）、Web 收件箱（W-02） |
| ③ 查看原文 | 从信号进入项目、中标方页面，字段回到原文片段 | 对象页与证据面板 BFF、对象信号分区、查看计量；事实与文档证据的本地读取绑定 | 数据平面读取与证据读取适配器（D-11）、归属边与组织解析（D-12）、信息领域服务状态与字段状态（D-10、D-13）、Web 对象页与证据面板（W-02） |
| ④ 简报触达 | 早间在企业微信收到按目标分区的简报，卡片上反馈或跳转 Web | 企业微信绑定、免登会话与撤销（内存适配器）；简报组装与投递、偏好、历史、重新推送和卡片动作（C-04） | 第5段剩余功能（C-18～C-21）、覆盖与来源权利适配器、生产企业微信适配器与外网回调入口（C-01、C-02）、浏览器免登会话与卡片落地（W-04，按 Web 专项设计 WD2）、H5 落地页（W-03） |

第6段跟进（指派、截止提醒、结果）在环节④之后接入，它的真实贯通依赖与④相同（渠道、调度、Web），不另列环节；未关闭部分登记为 C-12、C-15、C-16、C-18～C-25、D-18。

## 3. 分级验收

把“真实贯通”拆成五级，每一级都有独立的通过证据，前一级是后一级的输入。级别是对整条链路的判断，不是对单个子系统的判断。

| 级别 | 名称 | 数据 | 渠道 | 界面 | 通过证据 |
|---|---|---|---|---|---|
| L0 | 服务端切片 | 夹具 | 内存适配器 | 无 | 各段 PRD 验收表的自动化用例 |
| L1 | 回放贯通 | 仓库内冻结语料中的官方公告（中标结果、更正），经真实数据平面管线处理；企业主数据用标明为夹具的样本 | 内存适配器 + 站内 | 无（API 脚本走查） | 一套端到端测试：在同一环境中启动 data 与 platform 部署单元的真实进程角色，所有端口装配生产实现（不含任何 `NotWired`），从“投放一份结果公告文档”到“目标收件箱出现信号 → 对象页字段回到原文片段 → 站内简报”全程通过；含跨租户与无权限的负向用例 |
| L2 | 界面贯通 | 同 L1 | 同 L1 | Web 最小页面集 + 企业微信内置浏览器的 H5 落地页 | 开发 VPS 上的部署（M0-01）；内部人员按 PRD 各段“可运行”的步骤走查并截图，记录走查时的版本 |
| L3 | 真实渠道 | 同 L1 | 生产企业微信适配器，在**测试企业微信企业**中安装、绑定、免登、收简报、卡片动作 | 同 L2 | C10 对测试企业的验收记录（安装授权、凭证、验签、重放、限流、负向用例）；简报经企业微信实际送达的走查记录 |
| L4 | 真实数据 | 试点区域的 P0 来源持续采集，INF-ORG 来自企业公开信息 | 同 L3 | 同 L2 | M1-P1/P2/P3/P7 在试点范围内关闭；连续运行期间的信号、证据与简报抽查记录；数据截至时间与覆盖如实显示 |
| L5 | 客户走查 | 同 L4 | 客户自己的企业微信企业 | 同 L2 | M2-P8/P9 关闭；试点建材客户按 PRD 各段走查，任务观察与缺陷处置记录齐备，即各段的“功能验收” |

说明：

- L1 使用的是仓库内已冻结的官方公告语料（[`packs/information_domains/procurement/corpus/`](../../packs/information_domains/procurement/corpus/)），作为测试材料处理，不构成来源启用，不宣称 INF-PROC 就绪，不改变 M1-P1/P2 的门禁；
- L2 与 L3 互不依赖，可以并行；L4 依赖[阶段基线第4节](06-stage-baseline.md#4-m2-前置条件与能力交接)的外部事实，L5 依赖 L4；
- 每一级的负向用例都在该级环境中重新执行，不沿用 L0 的结论（第6节）；
- L1 的可重复入口：`tools/dev/run-a02-l1-gate.sh --local-openfga`（服务端门禁）与 `tools/dev/run-a02-process-replay.sh --local-openfga`（进程级门禁：同一组 `platform`、`mq-data` 与 `mq-acquisition`（acq-admin）进程完成建材中标公告 → signal → object → evidence → briefing 回放；data-api 为 mTLS 内部 listener，经 acq-admin 做来源权利准入，detection 以 monitoring、learning 以 simulation 按租户读取，回放租户的来源权利由脚本登记；并验证跨租户、visitor、无来源权利的租户、撤销来源权利与 data-api 不可用时失败关闭，gateway 证据面板未接入前失败关闭（D-11）），结果写入 `deploy/local/.data/` 下对应目录；验收脚本探测本机共享 PostgreSQL，CI/隔离环境用 `MIAOQIA_TEST_PG` 覆盖；
- L1 的口径：评测样本即解析器的开发样本，准确率不代表来源泛化能力；回放材料带 `corpus:<样本>` 权利标签，不构成来源启用；对象存储在回放与单机部署使用文件系统实现（不可覆盖、按内容寻址），S3 实现随开发 VPS（R-03）；
- L1 建材配对语料为衢州市阳光交易服务平台的 `official-building-tender-20261026` 与 `official-building-award-20261026`：同一项目 `XCXCG-2025-008`，采购人为江山市水务有限公司；原始响应、正文、人工事实及证据已保存，哈希登记于 `lifecycle-manifest.json`。中标定价 `76.5000%` 保留为 `conditions.contract_pricing`，不记金额，不据此推算成交总价。此配对形成同一 `pp_`，保留成员固定版本与字段证据；没有跨公告标包依据，故 `lots=[]`，不推断 `prj_` 或 `part_of`（[domain-intelligence 实现状况](../03-architecture/subsystems/domain-intelligence.md#12-实现状况)）。

## 4. 缺口清单

**当前贯通级别：L1 已通过；L2 进行中（W 缺口）；L3、L4、L5 未开始。** 本节是 A02 缺口状态的唯一登记处，每行只写尚未关闭的部分，已实现的设计以子系统规格为准；关闭的缺口移入第4.6节。“环节”指第2节的①～④；“最早级别”是该项必须关闭的贯通级别；“—”表示不在最短链路上，不阻断贯通。来源一列给出需求或规格的位置。

### 4.1 数据供应（D）

| 编号 | 缺口 | 来源 | 环节 | 最早级别 | 外部前置 |
|---|---|---|---|---|---|
| D-01 | acquisition 调度与来源健康未实现，evidence 未消费 `fetch.completed`；来源声明、抓取与归档已有操作员单次命令（[acquisition 第12节](../03-architecture/subsystems/acquisition.md#12-当前实现边界)） | [阶段基线 M1](06-stage-baseline.md#3-m1-前置条件与能力交接) | ② | L4 | M1-P1、M1-P2、M1-P7 |
| D-02 | evidence 的远程查询与生产来源权利策略、PDF/OCR 管线、内容增强与重放未实现（公共文档管线与文档证据本地读取见 [evidence 实现状况](../03-architecture/subsystems/evidence.md#12-实现状况)） | [阶段基线 M1-P6](06-stage-baseline.md#3-m1-前置条件与能力交接) | ②③ | L1 | — |
| D-03 | INF-PROC 更正公告解析器未实现；招标与结果公告解析器 `proc.tender-notice`、`proc.result-notice` 以人工映射为黄金标注评测 | `packs/information_domains/procurement/src/` | ② | L1 | — |
| D-04 | 标包 `lot_`、其他来源与完整生命周期的项目聚合未实现；冻结来源的 `pp_` 聚合与复核官方 pair 的公告主题品类只是局部实现（[domain-intelligence 实现状况](../03-architecture/subsystems/domain-intelligence.md#12-实现状况)） | [detection 第8节](../03-architecture/subsystems/detection.md#8-伸缩与可靠性) | ② | L1 | — |
| D-05 | 信息领域服务状态与覆盖接口（`GetCoverage`）未实现，规则到信息领域的生产对应缺失（B-PERMIT 不能显示“数据未接入”） | PRD 第3段用例7、第4段用例24–27；[customer-context](../03-architecture/subsystems/customer-context.md) | ①②③ | L1 | — |
| D-07 | 生产 OpenSearch percolation 未实现；当前为 search 的最小 PostgreSQL percolator（按 cell、事实类型、区域和品类过滤），未登记查询时失败关闭 | 同上；`services/subsystems/search/` | ② | L1（可先用最小实现） | — |
| D-08 | INF-ORG 企业主数据未接入；X-SELF 需要“关联本企业” | PRD 第2段用例1、7–9；第3段用例7 | ①② | L1 用夹具，L4 用企业公开信息 | M1-P3 |
| D-09 | learning 仿真数据集的完整多领域历史、覆盖与来源权利未接入（A02 结果视图已按草案所属租户以 simulation 用途读取） | [learning 第5节](../03-architecture/subsystems/learning.md#5-接口)；PRD 第7段用例11、24 | ① | L1 | — |
| D-10 | 完整 `GetCoverage` 与按租户覆盖状态未接入（learning 适配器对配置的 `INF-PROC` 返回 live） | 同上 | ①③ | L1 | 随 D-05 |
| D-11 | gateway 只接入采购项目 `pp_` 的概要、标包与公告时间线（`internal/dataplane`）；`prj_` / `org_` 与上级项目（entity-graph 读取未进 data-api，随 D-12）、证据面板（data-api 的证据载荷不含所属对象与来源名称、标题、链接）、历史与统计分区、字段状态（随 D-10、D-13）未关闭；未配置 data-api 时失败关闭 | [gateway 第10节](../03-architecture/subsystems/gateway.md#10-对象页与证据面板-bff)；[契约覆盖第9节](../03-architecture/13-contract-readiness.md#9-随功能追加的契约) | ③ | L1 | — |
| D-12 | entity-graph 组织解析、`part_of` 归属边、合并与撤销未实现；detection 归属边端口 `NotWired` | [detection 第5节](../03-architecture/subsystems/detection.md#5-接口)；PRD 第4段用例19–22、37–38 | ③ | L1（组织解析与 `part_of`）；品牌随 M2-P13 | — |
| D-13 | 字段状态、更正/撤回时间轴、来源中断、空态原因由数据平面提供 | PRD 第4段用例8–10、15–18、24–27 | ③ | L2 | 随 D-02～D-05 |
| D-14 | 重新求值与撤回关闭的生产者、`signal.updated`、A02 更正公告重新求值 | [detection 第5节](../03-architecture/subsystems/detection.md#5-接口)；PRD 第3段已确认事项11 | ② | L2 | — |
| D-15 | 首次激活回填的执行、标记与去重 | PRD 第7段用例18；detection | ② | L2 | — |
| D-16 | 漏报、来源权利与展示级别、清单导入、升级草案（F7） | PRD 第3段用例26–27、38–42；第7段用例19 | — | — | — |
| D-18 | 公开截止时间绑定（PRD 第6段 F6，用例33–37）：domain-intelligence 的投标截止变化事件（更正、撤回、业务有效值与版本）与 pursuit 任务的绑定未实现，任务只有手工截止 | [pursuit 第11节](../03-architecture/subsystems/pursuit.md#11-实现状况)；[契约覆盖第9节](../03-architecture/13-contract-readiness.md#9-随功能追加的契约) | ④ | L2 | 随 D-04 |

### 4.2 身份与渠道（C）

| 编号 | 缺口 | 来源 | 环节 | 最早级别 | 外部前置 |
|---|---|---|---|---|---|
| C-01 | 生产企业微信适配器、渠道凭证入库、“发送测试消息”与安装健康状态 | [channels 实现状况](../03-architecture/subsystems/channels.md) | ④ | L3 | C10 真实调用与凭证授权 |
| C-02 | gateway 未认证入口：安装回调、消息回调与免登的外网入口（目前只以本地绑定提供） | 同上 | ④ | L3 | 开发 VPS 公网域名与证书 |
| C-03 | IdP：Keycloak 管理接口与邀请邮件适配器、受邀新用户的账号准备、真实 TOTP 仪式与会话时长配置、Web 登录的保密客户端与后端通道登出。浏览器免登不再依赖 IdP 按免登会话铸造令牌（[Web 专项设计 WD2](../03-architecture/14-web-application.md#11-已确认事项)）：免登的浏览器会话由 W-04 承接，渠道网页授权与 `ChannelSSO` 的生产接入由 C-01、C-02 承接；Bearer 免登令牌只保留给测试签发方与非浏览器客户端，不阻断浏览器 L3 | [identity 第5节](../03-architecture/subsystems/identity.md#5-接口)；PRD 第1段用例6、18 | ①④ | L2 | — |
| C-04 | 简报与卡片动作经生产企业微信的投递（站内部分已在 L1 关闭） | PRD 第5段；[channels 实现状况](../03-architecture/subsystems/channels.md) | ④ | L3 | 随 C-01 |
| C-12 | 持续跟踪每期的变化简报（channels 消费 `pursuit.tracking.period_closed.v1`，按期末成员快照与上一期对比组装并投递）与 detection 关注消费 `pursuit.tracking.changed.v1`；依赖数据平面按对象集合与期间读取变化的内部接口（未建） | [契约覆盖第9节](../03-architecture/13-contract-readiness.md#9-随功能追加的契约)；PRD 第6段 F10 | ④ | L2 | 随 D-12、D-04 |
| C-15 | 互相提示的范围为同一采购项目（同一事实类型与主体），跟进目标不含标包标识，尚不能缩小到同一标包 | PRD 第6段 F2 第1条 | — | L2 | 随 D-04 的 `lot_` |
| C-16 | 评估报告的持久化与 Web 展示（`getOutcomeSummary` 只读计算，不留存报告版本） | [learning 实现状况](../03-architecture/subsystems/learning.md) | — | L2 | — |
| C-18 | 简报的 AI 开头摘要（PRD 第5段第13节第8项，M2 不开启）。没有实现，也不建端口：需要 ai-platform 与生成标识的验收，真实模型调用未获授权；开启时摘要失败不得影响简报 | PRD 第5段第13节第8项 | — | — | 随 M3 研究入口 |
| C-19 | 简报“关注变化”区（PRD 第5段第13节第9项：关注开放前不显示该区）。关注（watch）的数据来源与变化读取接口未建（detection、domain-intelligence 与数据平面均没有） | PRD 第5段第13节第9项；[channels 4.4](../03-architecture/subsystems/channels.md#44-简报) | ④ | L2 | 随关注功能与数据平面 |
| C-20 | 即时通知的合并卡片（按收件人、合并组与窗口合并为一条，窗口由偏好与包声明），当前以每人每小时 6 张节流、超出的留给下一期简报代替；`route` 受众 `role:<角色>` 尚不支持（需要按目标单元枚举持有角色的成员，依赖 C-07 的 `ListPermitted`），只支持 `objective_owner` 与 `objective_members` | [channels 4.5](../03-architecture/subsystems/channels.md#45-通知合并)；PRD 第5段 F7 第2条 | ④ | L2 | 随 C-07 |
| C-21 | `detection.signal.updated.v1` 的实质更新（处置上调、中标方/金额/品类更正）与 24 小时内截止的事项不即时通知，只在下一期简报中标“已更新”；依赖重新求值的生产者（D-14） | [channels 4.5](../03-architecture/subsystems/channels.md#45-通知合并) | ④ | L2 | 随 D-14 |
| C-22 | 分派规则只支持按区域与品类匹配；按我方主体（信号没有“由我方哪个主体承接”的字段）与组织单元（目标与信号同在目标所在单元，identity 登记单元覆盖前只有根单元）匹配的条件未提供；按角色选人（如“该单元的项目推进者”）需要按单元枚举持有角色的成员，依赖 C-07 的 `ListPermitted` | [customer-context 第11节](../03-architecture/subsystems/customer-context.md#11-分派配置) | — | L2 | 随 C-07 |
| C-23 | 候选负责人只经 pursuit 的创建面板（`getCreateDefaults`）展示，信号详情（`getSignal`）、卡片与即时通知中不显示候选；规则变更后已有信号的候选不重算（保留生成时的建议与分派配置版本，随重新求值 D-14）；分派规则的 Web 配置页随 W-02 | PRD 第3段 F3 第1条；PRD 第6段 F1 第2条 | — | L2 | 随 D-14、W-02 |
| C-24 | 授权同步告警（`authorization_sync_delayed` / `failed` / `recovered` 事件与 `alert` 键的结构化日志）尚无路由到值班的告警规则与通知渠道（仓库没有告警子系统，operations 只有链路探针）；未绑定渠道的决策者只在 Web 中看到“授权同步失败”（Web 列表与详情的展示随 W-02）；部分路径（成员变化、identity 拒绝后的重新同步）的 `sync_pending_since` 取数据库时钟而阈值按业务时钟判断 | [pursuit 第11节](../03-architecture/subsystems/pursuit.md#11-实现状况)；PRD 第6段 F12 第2、6条 | ④ | L2 | 随 R-04、W-02 |
| C-25 | 对象合并后的替换（消费 entity-graph 的 `entity.merge.applied.v1` / `entity.merge.reverted.v1`：对象集合中被吸收的组织替换为存续组织、撤销合并时恢复，已结束各期的快照不变；事件契约已有、生产者未实现）、实体图中组织版本的生产读取（`api.Organizations` 生产适配器，缺省未接入：名单导入只校验标识格式、各期对象记为 `not_wired`）、按名称（与统一社会信用代码）解析名单行（`entity-graph.ResolveEntity`，当前只有名称的行为 `unresolved`、不猜测）、直接从 customer-context 企业清单（`TenantEntityList`，尚未实现）选取对象；周期时区仍为缺省 Asia/Shanghai（租户与成员时区无所有者，见契约覆盖第9节） | PRD 第6段 F10 第1、3条、用例47；[契约覆盖第9节](../03-architecture/13-contract-readiness.md#9-随功能追加的契约) | — | L2 | 随 D-12 |
| C-05 | 审批通知、策略执行失败通知、邀请通知的实际发送 | PRD 第7段用例13、14、20；identity | ① | L2 | 随 C-04 |
| C-06 | 管理员账号恢复（IdP 管理接口与平台运营双人控制） | PRD 第1段用例17 | — | L5 前 | — |
| C-07 | identity：`ListPermitted`、单元删除与变更事件、服务账号与外部协作者、个人租户免审批例外、额度条件 | [identity 第5节](../03-architecture/subsystems/identity.md#5-接口) | — | — | — |

### 4.3 运行装配（R）

| 编号 | 缺口 | 来源 | 环节 | 最早级别 | 外部前置 |
|---|---|---|---|---|---|
| R-02 | 贯通装配：各子系统 `Module()` 缺省为 `NotWired`，缺少一份“全部端口装配生产实现”的部署配置与启动检查 | 各子系统实现状况 | ①～④ | L1 | — |
| R-03 | 开发 VPS 部署与重建（M0-01）、候选版本的实际 CI 与开发 E2E（M0-08） | [阶段基线 M0](06-stage-baseline.md#2-m0-逐项核对) | ①～④ | L2 | — |
| R-04 | 持续运行的告警、值班、备份恢复（C11） | 阶段基线 M1-P7 | ② | L4 | C11 |
| R-06 | 埋点、产品目录、完整商业权益与配额事件（`commerce.quota.exceeded.v1`）、用量报表 | [commerce 第10节](../03-architecture/subsystems/commerce.md#10-查看计量的实现状况)；gateway | ③ | L5 前（M2-P8） | — |

### 4.4 界面（W）

| 编号 | 缺口 | 来源 | 环节 | 最早级别 | 外部前置 |
|---|---|---|---|---|---|
| W-02 | 成员管理、档案/目标完整写入、强认证续接和版本检查未实现（已交付页面见 [`web/README.md`](../../web/README.md)） | 各段 PRD 中标为“随 Web”的行为（强认证弹窗与已填内容保留、确认页、版本检查、“按 vN 判断”、逐层展开与高亮等） | ①②③ | L2 | gateway Web 会话与子系统公开读取接口 |
| W-03 | 真实企业微信网页授权、免登会话和渠道负向验收未关闭（H5 绑定确认页、卡片链接落地及提示已实现） | PRD 第1段 F5、F7；第5段 | ④ | L2（内存适配器下的页面）、L3（真实渠道） | — |
| W-04 | 生产 OIDC 授权码/PKCE、数据库会话、刷新令牌、后端通道登出、渠道免登与 `/v1/web/*` 聚合端点未实现（开发会话边界已实现） | [Web 专项设计第4、7节](../03-architecture/14-web-application.md#4-浏览器会话与认证) | ①～④ | L2 | C-03 Web 登录配置 |
| W-05 | Web 页面需要、尚未公开的子系统接口：identity `ListMembers`、`PreviewInvitation` 与受邀新用户的 IdP 账号准备；entity-graph `ResolveEntity` 的公开读取；evidence 证书文件上传；learning `ReportMiss` 的公开接口；domain-intelligence `GetCoverage` 的公开读取（随 D-05） | [Web 专项设计第7.3节](../03-architecture/14-web-application.md#73-端点清单) | ①～③ | L2 | 账号准备随 C-03 |
| W-06 | OpenAPI 生成客户端、九种状态全集、统一列表、错误码覆盖检查、CI 与浏览器端到端测试未补齐 | [Web 专项设计第2、5、6节](../03-architecture/14-web-application.md#2-应用形态与工程结构) | ①～④ | L2 | gateway Web 会话与浏览器验收环境 |
| W-07 | 保存视图：customer-context `SavedView` 与接口；个人视图随收件箱，团队视图随统一列表组件 | [Web 专项设计第6.2节](../03-architecture/14-web-application.md#62-保存视图的所有者) | ② | L2（个人）、L5 前（团队） | — |

### 4.5 客户与运营（U）

| 编号 | 缺口 | 来源 | 环节 | 最早级别 | 外部前置 |
|---|---|---|---|---|---|
| U-01 | 测试企业微信企业（用于 L3，不用客户企业） | C10 | ④ | L3 | 项目负责人提供 |
| U-02 | 建材试点客户候选与供应商 M2 验证计划 | 阶段基线 M2-P9、A5 | ①～④ | L5 | 未确定 |
| U-03 | 客户试用前置：单元级授权与列表过滤的运行时核对、查看限额、删除更正流程、支持与缺陷反馈、对外协议 | 阶段基线 M2-P8 | ①～④ | L5 | — |
| U-04 | 各段 PRD“可运行”所要求的走查记录、截图与任务观察 | 各段 PRD 第12节 | ①～④ | L2 起逐级 | — |

### 4.6 已关闭

| 编号 | 关闭内容 | 证据 |
|---|---|---|
| D-06 | data-api 服务主体按租户读取：SourceRightsPolicy 用途为 interactive、monitoring、simulation 的子集；mTLS 准入按委托点名服务主体；acquisition 权利裁决接纳服务主体；detection 先由 percolator 取候选租户（不读内容），再以 monitoring 按租户读取求值单位并按该租户的区域/品类过滤；learning 以 simulation 按草案租户读取。进程回放在 mTLS data-api 与 acq-admin 上通过 | `tests/e2e/a02_process_replay.py`（2026-11-07 02:50（+08:00），VPS Kafka、本机 OpenFGA；结果见 `deploy/local/.data/a02-process-replay/result.json`，信号 `sig_01M6S35QSD2AJNF3W56FX2TE58`、对象 `pp_01M6RY2DT9ZPFDW3PV54G2PCJE`、简报 `brf_01M6S35RKDSPKTT0FVS4CTAYJN`）、`data/kernel/tests/test_internal_http.py`、`data/subsystems/acquisition/tests/test_rights.py`、`data/subsystems/acquisition/tests/test_remote.py`、`data/subsystems/domain-intelligence/tests/test_read_transport.py`、`services/subsystems/detection/internal/app/evaluate_test.go`、`services/subsystems/learning/api/remote/data_test.go` |
| D-17 | 仿真报告的代表样本按查看者本人的 `can_search_public` 与数据覆盖过滤，无权的只给数量，判断不可用时不返回样本 | `services/kernel/integration/policy_approval_test.go`（用例11）、`services/subsystems/learning/internal/app/samples_test.go` |
| C-08 | 即时通知 F7 与卡片“跟进”按钮（服务端） | `services/kernel/integration/notify_test.go`（`TestInstantNotifications`）、`services/kernel/integration/pursuit_test.go`、`services/subsystems/channels/internal/app/pursue_internal_test.go` |
| C-09 | 第6段渠道侧：提醒卡片投递与“完成/撤销”、简报“待我处理”任务、授权被拒通知、提醒安静时段（服务端） | `services/kernel/integration/pursuit_test.go`（`TestPursuitFollowUp`） |
| C-10 | 持续跟踪 F10、创建面板候选负责人、同一采购项目的互相提示、评估报告结果汇总（服务端） | `services/kernel/integration/pursuit_test.go`（`TestPursuitFollowUp`） |
| C-11 | 渠道通知投递事件与统计、写入准入不满足时的转交、转交提醒分页展开、持续跟踪的授权被拒通知（服务端） | `services/kernel/integration/pursuit_test.go`、`services/subsystems/channels/internal/app/notices_internal_test.go` |
| C-13 | 持续跟踪的改派与协作者（`assignTracking`）、归档与取消归档（`archiveTracking` / `unarchiveTracking`，已归档只读）、按名单导入对象（`importTrackingMembers`，逐行结果与操作一起固定）、成员快照中每个对象的加入版本与来源及期末实体来源版本（`TrackingPeriod.objects`、`tracking.period_closed` 的 `objects`）（服务端） | `services/kernel/integration/pursuit_test.go`（`TestPursuitFollowUp` 的 C-13 子测试）、`services/subsystems/pursuit/internal/app/domain_internal_test.go`（`TestTrackingMemberSources`、`TestPeriodObjects`） |
| C-14 | 目标分派规则配置与信号上的候选负责人，pursuit 候选适配器（服务端） | `services/kernel/integration/assignment_test.go`、`services/subsystems/detection/internal/app/assignment_test.go`、`services/subsystems/customercontext/internal/domain/assignment_test.go` |
| C-17 | 授权同步待处理超过 5 分钟与超过 24 小时的告警事件、授权同步失败时通知能处理的决策者、负责人恢复写入准入后转交提醒的转回（服务端） | `services/kernel/integration/pursuit_test.go`（`TestPursuitFollowUp` 的 C-17 子测试）、`services/subsystems/channels/internal/app/notices_internal_test.go`、`services/cmd/platform/roles_test.go` |
| R-01 | `scheduler` 角色运行各子系统声明的周期任务，主备同一时刻只一个实例执行 | `services/kernel/scheduler/scheduler_test.go`、`services/kernel/integration/tenants_test.go`、`services/subsystems/channels/jobs_test.go`、`services/cmd/platform/roles_test.go` |
| R-05 | commerce `checkCoverage` 与 gateway 覆盖适配器，覆盖服务故障时失败关闭 | `services/kernel/integration/coverage_test.go` |
| W-01 | Web 专项设计（WD1–WD7 由项目负责人确认） | [Web 专项设计](../03-architecture/14-web-application.md)、[整合原型](../01-product/prd/prototypes/00-web-shell.html) |

## 5. 顺序

当前主线以[阶段基线第1.1节](06-stage-baseline.md#11-当前主线)为准：真实数据链路对应本线的 L4，最薄研究切片不在本线的四个环节内，由阶段基线单独验收。本线各级按依赖排列，不给日期，每一级的开始以前一级的通过证据为准：

1. **L1 回放贯通**：已通过（第3节的可重复入口，Kafka 使用 VPS 公共组件，OpenFGA 使用本机内存服务）；
2. **L2 界面贯通**：只完成已开工的页面与已登记的 W 缺口，不扩展新页面；R-03 与 C-03 的生产 Web 登录部分是独立门禁；
3. **L3 真实渠道**：与 L2 并行，前提是 U-01 测试企业与 C10 真实调用授权到位；交付 C-01、C-02，以及 W-04 中浏览器免登会话与卡片落地在真实渠道下的验收（按 Web 专项设计 WD2）；
4. **L4 真实数据**：当前主线。在一个试点区域的少量 P0 来源上关闭 M1-P1/P2/P7，以真实数据替代 L1 的回放语料与企业夹具，关闭 D 缺口；不以全国覆盖为前提；
5. **L5 客户走查**：L4 通过且 M2-P8、M2-P9 关闭后，由试点客户按各段 PRD 走查，关闭各段的“功能验收”。

## 6. 维护规则

- **缺口登记**：第4节是 A02 缺口状态的唯一登记处。实现 PR 在此新增编号或改写未关闭部分，关闭时移入第4.6节并写明证据；PRD 验收表只登记用例状态与测试证据，子系统规格只描述已实现的设计，二者不写进度叙述；
- **进度口径**：汇报 M2 进度时同时给出服务端切片覆盖与贯通级别，不把服务端切片完成表述为旅程可用；
- **负向验证随级别重做**：每升一级，在该级环境中重新执行跨租户、无权限、令牌重放与失败关闭的负向用例；涉及授权或租户隔离的实现 PR 附独立的越权与反例测试，不以 CI 全绿或验收表标记代替；
- **外部事实**：D1c/D2、建材试点客户候选、C3/C7a、C10 的状态以[阶段基线第4节](06-stage-baseline.md#4-m2-前置条件与能力交接)和[立项清单](00-initiation-readiness.md)为准，本文只标注它们阻断哪一级。

## 7. 已确认事项

以下事项由项目负责人确认：D1、D3、D5–D7 随 [PR #33](https://github.com/SuperAILabs/MiaoQia/pull/33) 评审按推荐做法采纳；D2、D4 为 2026-11-06 的主线决议。真实调用、凭证与来源启用仍按各自门禁另行验收，本节不代替这些授权。

| # | 已确认事项 |
|---|---|
| D1 | M2 进度同时按服务端切片覆盖与贯通级别（L0–L5）汇报 |
| D2 | 当前主线是真实数据链路（本线 L4）与最薄研究切片（[阶段基线第1.1节](06-stage-baseline.md#11-当前主线)）；改 detection、learning、gateway、commerce 的端口适配器前核对并行工作的文件范围 |
| D3 | L1 使用仓库内冻结的官方公告语料与标明为夹具的企业主数据；订阅反查先用按目标区域与品类过滤的最小实现；仿真接入真实数据集前先关闭 D-17 |
| D4 | L2 Web 只完成四个环节已开工的最小页面集、H5 落地页与已登记的 W 缺口，不扩展新页面 |
| D5 | L3 只在测试企业微信企业中验收，L3 与 L2 并行；测试企业与 C10 真实调用、凭证授权由项目负责人提供（U-01） |
| D6 | L4 先在一个试点区域的少量 P0 来源上关闭 M1-P1/P2/P7；D1c/D2、试点客户候选、C3/C7a 作为 L4/L5 阻断项跟踪 |
| D7 | 按第6节维护缺口编号与负向验证 |
