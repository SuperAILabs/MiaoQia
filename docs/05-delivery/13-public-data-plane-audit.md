# 公共数据面接入与供数审查

2026-11-21（上海时间）。范围为采购包的首批 40 个来源入口、公开数据面的运行代码、开发服务器与有来源收据的实网样本。

## 接入结论

40 个入口中，31 个已配置为启用，均有至少一份实网正文抽样，8 个公开入口还有明确阻碍，1 个需要登录。重庆市公共资源交易网新增政府采购栏目 `procurement-notices`，归入现有交易中心来源。配置和原站准入分别见[来源运行配置](../../packs/information_domains/procurement/sources.yaml)、[来源清单](../../packs/information_domains/procurement/source-inventory.yaml)与[逐源验收证据](12-public-source-probe.md)。

| 暂缓入口 | 验收停在哪一步 |
| --- | --- |
| 四川政府采购公告信息 | 公告查询要求验证码，尚无该列表的公告键和原件。 |
| 四川政府采购首页 | 匿名浏览器取得首屏和正文，部署出口获得应用外壳。该站的列表请求及原件和正文配对仍需在采集角色中复现。 |
| 重庆政府采购首页、重庆政府采购栏目（2 个） | 原站匿名浏览器能显示详情，栏目也可翻到第 2 页。需保存同一次导航的原始响应、渲染正文及公告键，并逐件离线回读。交易中心转载不能代替这两个原站入口的收据。 |
| 江西政府采购 | 匿名 HTTP 返回 412，历史查询有验证码。浏览器可见内容仍缺独立归档。 |
| 西藏公共资源工程交易 | 此前两个出口的 HTTP 列表均耗尽三次额外重试。本次本机和开发 VPS 的 HTTPS 工程列表均返回 403。 |
| 新疆政府采购 | 列表、POST 及详情 iframe 尚未在受控运行时完成同一次采集，开发出口另有 405 或 TLS 证书链校验失败记录。 |
| 中铁鲁班商务网 | 已有站务页面，采购公告正文和公告身份尚缺合格样本，VPS 入口曾返回 403。 |
| 阳煤电子交易 | 需要登录授权。 |

## 真实运行状态

11 月 21 日 01:49 左右以只读方式盘点公共测试 VPS。可见的妙洽常驻容器为 PostgreSQL、Kafka、Keycloak、OpenFGA 和 OpenTelemetry Collector。公共栈 PostgreSQL 的非模板数据库为 `postgres`、`miaoqia_fga`、`miaoqia_keycloak`。该主机未发现常驻采集、Evidence、数据 API、检索或 SeaweedFS 角色，也未发现 `/etc/miaoqia/data-plane`。生产数据量、最新成功时间、消费积压和线上搜索命中数因此未知。现场结果适用于此次实际连接的主机和公共栈实例。[部署清单及专用数据库要求](../../deploy/vps/data-plane/README.md)

同日开发 VPS 对交易中心采购专栏返回 HTTP 403。成功的重庆采集由本机正式采集命令写入隔离 PostgreSQL 和文件对象存储。其他来源已取得的 VPS 隔离验收包含 SeaweedFS 原件回读。当前可证明的供数能力为有账本的验收批次，全天候运行及 30 天正文回填仍缺目标环境收据。

## 从发现到消费

| 环节 | 实现与实测边界 |
| --- | --- |
| 来源、调度和覆盖 | 目录只向调度暴露获准声明。在线、恢复、历史工作线程分别消费计划。列表遍历登记日期窗、停止原因和覆盖缺口。重庆两个栏目都上报 `homepage_current_window`。站点首屏更新速度超过两次调度间隔时，两次之间发布又移出首屏的公告可能漏采。[来源装载](../../data/subsystems/acquisition/src/miaoqia/acquisition/application/sources.py) · [调度](../../data/subsystems/acquisition/src/miaoqia/acquisition/application/dispatch.py) · [列表遍历](../../data/subsystems/acquisition/src/miaoqia/acquisition/application/discovery.py) |
| 抓取和正文边界 | 重庆以来源、原站公告键和栏目路由校验详情，服务端公告记录同时核对标题、分类和唯一正文容器。政府采购专栏与综合交易共享处理器。11 月 20 日采购专栏 12 行全数通过，综合栏目 12 行中 9 行属于已验分类，另 3 行来自 `004008004/004008005`。这 3 行未纳入采集量。[交易中心规则](../../packs/information_domains/procurement/src/miaoqia_pack_procurement/sources/trade_html.py) |
| 附件与原件持久化 | 铁建云链依赖已匹配公告的 `noticeContent`，M2 使用已匹配详情的 `data.files` 并限制文件端点。重庆只归档公告 HTML，外围导航、下载中心和页脚不产生附件任务。历史 M2 冻结样本含两份可回读附件，第三份必需附件仍待补。冻结版本和对象摘要可核对。[附件规则](../../packs/information_domains/procurement/src/miaoqia_pack_procurement/sources/trade_api.py) · [冻结读取](../../data/subsystems/acquisition/src/miaoqia/acquisition/application/frozen_batches.py) |
| 清洗、证据及事实 | Evidence 从原件生成规范化文档，评估抽取结果、字段证据和语义错误，再由领域侧按修订幂等生成事实版本。重庆的 21 份原件走了正式只读评估，自动事实发布尚未准入。M2 仅有附件正文的既有样本进入待解析队列。[文档与评估](../../data/subsystems/evidence/src/miaoqia/evidence/application/pipeline.py) · [事实物化](../../data/subsystems/domain-intelligence/src/miaoqia/domain_intelligence/application/facts.py) |
| 搜索、API 与调研 | 公共搜索对索引候选逐页调用权威读取及授权。A02 调研按指定公告版本核验证据并保留未知项。Go 搜索和调研包测试通过。现场缺少本批原件对应的线上事实版本、索引代次及调用回执，用户通过搜索或 API 可取得的本批公告数待验。[公共查询](../../services/subsystems/search/public_query.go) · [A02](../../services/subsystems/research/a02.go) |

## 已取得的数据

11 月 20 日的 40 源抽查共有 50 份本机、补样及 VPS 探测收据，69 次尝试累计报告 1,904 个列表引用，含重复页和重试。31 个不同来源各有一份正文样本，共 31 个不同响应摘要。该轮湖南工程样本缺单公告键，需按来源的标段与公告绑定规则处理。跨来源项目去重和全月覆盖均不能由这一轮抽查推算。

下表仅纳入有采集报告且本次逐件重新计算 SHA-256 的五个隔离批次。容量取原始响应字节。合计 47 个不同内容摘要、5,482,868 字节。相同公告在不同时间的原件可以有不同内容摘要，公告数量需另用来源和公告键计算。

| 原件来源 | 批次 | 正文原件 | 原始字节 | 详情失败 |
| --- | ---: | ---: | ---: | ---: |
| 重庆交易中心 | 1 | 21 | 4,397,440 | 0 |
| 四川工程 | 1 | 12 | 666,998 | 0 |
| 河南工程 | 1 | 1 | 37,035 | 0 |
| 西藏政府采购 | 2 | 10 + 3 | 287,044 + 94,351 | 2 + 9 |

重庆两个栏目有 21 个不同公告键。采购专栏 12 份产生 12 条候选事实，含 7 份公开招标、5 份竞争性磋商，12 份均有标题、采购人及代理机构字段证据，共 50 个修订内去重的字段证据引用。综合栏目 9 份产生 8 条候选事实、14 个字段证据引用，5 条 `NO_RESULT`、2 条 `MISSING_KEY_FIELD`。一份答疑补遗公告没有匹配的解析器。候选人和结果公告仍需按公告阶段、标段和证据逐项判定。两栏目合计 20 条候选事实。

逐份对照原文发现，12 份政府采购公告写的都是“本项目是否接受联合体：否”，旧规则会把问句里的“接受联合体”当作同意。修正规则后，12 份均返回 `consortium_allowed=false`。无答案、含糊答案和冲突声明的回归样本均留空。原先 12 份通过结构检查且无告警，说明结构检查无法发现这个语义错误。当前样本支持公告检索和字段测试。金额汇总、中标方认定及项目资格筛选的全面准确率仍需逐字段对照原件。

复核重庆采集可在单独数据库及对象目录运行下列命令。首屏随发布滚动，同一日期窗的历史结果应按已保存的观察账本和对象回读。

```sh
cd data
uv run --locked --all-packages mq-acquisition-fetch --source chongqing-ggzy-trade-014 \
  --since 2026-11-20 --until 2026-11-20 --max-pages 1 --packs ../packs
```

此次重庆观察账本的 SHA-256 为 `b7b15a17a1457edd2dfcd4bd8805b63044b148e46272900ecfd7b7b343c2c0cf`。修正后的逐份评估收据摘要为 `b0fff0a19bc13128e4ecba011b4e37d9ac277a639d306cb4697724e3e49554e6`。
