# D1b 试点来源启用验收（安徽）

> 状态：验收中 · 负责人：数据负责人

本文是立项清单 [D1b 来源启用与覆盖基线](00-initiation-readiness.md) 在试点区域的验收记录，对应[阶段基线 M1-P1](06-stage-baseline.md#3-m1-前置条件与能力交接)。试点区域为安徽，P0 来源为下表三个盘点条目（[source-inventory.yaml](../../packs/information_domains/procurement/source-inventory.yaml)）。来源均为公开信息、不对外二次商业发布，不做逐来源授权，只登记来源跟踪（[来源治理](04-data-quality-organization.md#11-来源治理)）。

| source_id | 名称 | 覆盖 | 结论 |
|---|---|---|---|
| `anhui-ggzy-construction` | 安徽省公共资源交易平台-工程建设公告 | 安徽省级与各市工程建设招标、中标候选人、中标结果 | 能力验证通过 |
| `anhui-government-procurement-spa` | 安徽政府采购网 | 安徽省级与各区县政府采购的采购、更正、结果、合同公告 | 能力验证通过；历史深度待复验 |
| `ccgp-central-public-tenders` | 中国政府采购网-中央公开招标公告 | 中央单位公开招标公告 | 未通过：开发环境出口被站点拒绝 |

## 1. 验证方法

2026-11-11 从开发者本机做低频只读探测，请求间隔不小于 1.5 秒，每站不超过 15 个请求；不登录、不处理验证码、不绕过任何访问限制，入口参考 Tender Intel 的公开定位信息（[参考说明](../03-architecture/10-tender-intel-reference.md)）。本机出口地址位于境外；开发 VPS 同样位于境外。

## 2. 安徽省公共资源交易平台（工程建设）

| 项 | 结果 |
|---|---|
| 列表 | `POST https://ggzy.ah.gov.cn/jsgc/list`（表单：`currentPage`、`tenderProjectType`、`bulletinNature`、`jyptId`、`region`），返回服务端渲染 HTML，每页 10 条，含详情链接、地区与公告类型标签、发布日期 |
| 公告类型 | `bulletinNature`：1 招标公告、2 中标候选人公示、3 中标结果公告；`tenderProjectType`：1 建筑、2 水利、3 交通、4 其他、5 农业工程 |
| 分页与历史 | 招标公告（建筑工程）共 2 334 页，末页发布日期 2024-01-02；中标结果公告共 2 178 页。可回溯约两年 |
| 详情 | 公开页 `GET /jsgc/newDetail?guid=…&tenderProjectType=…&bulletinNature=…` 只是外壳；正文由 `POST /jsgc/newDetailSub`（`type`、`bulletinNature`、`guid`、`statusGuid`）返回 HTML 片段 |
| 生命周期 | 同一项目 `guid` 下按 `type` 分环节：`sxsp` 审批、`xmdj` 登记、`tender` 招标、`xmkb` 开标、`pbjg` 评标结果、`zbjg` 中标结果、`zbtzs` 中标通知书、`htxx` 合同；无信息的环节返回“此流程暂无信息” |
| 更正 | 未见独立的更正公告类型，样本中未出现变更内容；需在采集中按正文识别，未确认前登记为未知 |
| 附件与原文 | 样本正文为 HTML，无附件；正文含指向各市交易平台原文的链接（如合肥） |
| 渲染 | 不需要浏览器渲染 |
| 限速 | 间隔 1.5 秒的连续请求未被限制 |
| 共同平台标识 | 省级平台汇集各市交易平台内容；与各市入口（如 `hefei-ggzy-engineer`）属同一来源体系，覆盖不按入口重复计数 |

## 3. 安徽政府采购网

| 项 | 结果 |
|---|---|
| 列表 | `POST https://www.ccgp-anhui.gov.cn/portal/category`（JSON：`categoryCode`、`pageNo`、`pageSize`），返回 JSON，含 `articleId`、标题、公告类型名、发布时间（毫秒时间戳）、开标时间、区划名称 |
| 公告类型 | `ZcyAnnouncement1` 采购意向、`2` 采购公告（共 72 803 条）、`3` 更正公告（共 28 100 条）、`4` 结果公告（共 88 322 条）、`5` 合同公告、`7` 商城采购公告 |
| 详情 | `GET /portal/detail?articleId=…&parentId=541080` 返回 JSON：正文 HTML（`content`）、项目编号与名称、区划、发布时间 |
| 生命周期 | `announcementLinkDtoList` 列出同一项目的采购、更正、结果等环节及其 `articleId`，可直接建立公告之间的关联 |
| 更正 | 有独立的更正公告类型 |
| 附件 | 样本的 `attachmentList` 为空，附件以正文内链接形式指向市级交易平台 |
| 渲染 | 站点前端是单页应用，但列表与详情接口可直接调用，不需要浏览器渲染 |
| 限速 | 间隔 1.5 秒连续 6 个请求后返回 HTTP 429，之后数分钟内的单个请求仍返回 429；采集须按来源设置更低速率并在 429 后退避 |
| 历史深度 | 因限速未完成深页探测，待按低速率复验 |
| 共同平台标识 | 与公共资源交易平台的政府采购栏目内容重叠；试点只从本站采集政府采购，从公共资源交易平台采集工程建设，二者不重复计数 |

## 4. 中国政府采购网（中央公开招标）

首页与列表 `https://www.ccgp.gov.cn/cggg/zygg/gkzb/` 对本机出口均返回 HTTP 403，未取得任何内容。站点很可能拒绝境外出口；开发 VPS 同样位于境外。启用前须在中国大陆出口复验列表、分页、详情与附件，或由项目负责人决定替换来源。

## 5. 后续

- 盘点条目：两个安徽来源登记为 P0、能力已验证；中国政府采购网登记为 P0、待复验；
- 采集实现按本文的接口与限速配置访问契约；离线测试使用录制的响应，不在 CI 中访问外网；
- 中国政府采购网复验或替换后更新本文第4节与盘点条目。
