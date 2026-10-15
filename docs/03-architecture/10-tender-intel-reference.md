# Tender Intel 参考说明

> 状态：评审中 · 版本：3.0 · 更新：2026-10-14 · 负责人：首席架构师

## 1. 定位

[Tender Intel](https://github.com/lansespirit/tender-intel) 是妙洽立项前的**前期参考原型**，不是妙洽的代码基础，也不是成熟的架构。

**妙洽全部自主开发**：

- 不迁移 Tender Intel 的代码、数据库结构、对象模型或运行形态；
- 不以 Tender Intel 的测试作为妙洽的验收基线；
- 不进行新旧系统并行运行或切换；
- 妙洽的子系统、契约、扩展包和数据模型完全按 `docs/` 中的设计从零实现。

## 2. 可借鉴的经验

Tender Intel 在实践中暴露并初步验证了一些问题和思路。妙洽把它们作为设计输入，已在正式文档中以妙洽自己的方式重新定义：

| 经验 | 妙洽中的正式定义 |
|---|---|
| 公共信源采集应与客户需求解耦，一次采集多客户使用 | 公共域与租户域分离（[数据架构](02-data-architecture.md)） |
| 原始响应需要不可变归档，便于复核与重放 | [ADR-0011](../04-decisions/0011-immutable-evidence-dual-versioning.md) |
| 来源内容变化与解析器升级是两种不同的版本 | 快照与解析修订（[evidence](subsystems/evidence.md)） |
| 关键字段需要能回到原文 | 字段溯源（[evidence](subsystems/evidence.md)） |
| 冷启动时“处置分层”比单一分数更实用 | 处置模型（[detection](subsystems/detection.md)） |
| 客户规则需要版本化，判断需要固定当时的规则与数据版本 | 策略版本与版本固定（[customer-context](subsystems/customer-context.md)、[assessment](subsystems/assessment.md)） |

## 3. 已知的不足（妙洽需避免）

| Tender Intel 的问题 | 妙洽的做法 |
|---|---|
| 对象模型以招投标公告为中心，难以扩展到其他信息领域 | 平台核心与信息领域包分离（[可扩展性](04-extensibility.md)） |
| 客户规则与匹配语义带有具体行业特征 | 策略 DSL + 行业包 |
| 只有“匹配”，没有研究、判断、决策、推进的完整旅程 | 客户智能平面的六个子系统 |
| CLI、SQLite、本地文件的运行形态，无多租户、身份、API、事件与运维体系 | 完整的平台平面与部署拓扑 |
| 以“重建模式”代替数据库迁移 | 向前兼容的迁移规范（[研发规范](../05-delivery/03-engineering-standards.md)） |

## 4. 可选的参考材料

在知识产权与来源权利经书面确认后，Tender Intel 积累的来源清单与公告样本**可以**作为妙洽制定来源参照集和标注评估集的参考素材。这些素材进入妙洽前，需按妙洽的来源权利登记与评估集规范重新整理，不作为代码或数据直接导入。


妙洽已按该边界建立[候选来源盘点](../../packs/information_domains/procurement/source-inventory.yaml)；本地公告样本只用于[契约表达与覆盖审查](13-contract-readiness.md)，原型解析结果不作为妙洽已验证的事实或验收结论。
