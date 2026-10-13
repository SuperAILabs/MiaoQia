# 架构决策记录（ADR）

ADR 记录重要且难以逆转的决策：背景、决策、备选方案与后果。ADR 一经批准不得修改正文，只能由新 ADR 替代（在旧 ADR 中注明“已被 ADR-xxxx 替代”）。

## 状态

| 状态 | 含义 |
|---|---|
| 提议 | 已提出，待评审 |
| 已批准 | 评审通过，作为执行依据 |
| 已替代 | 被新的 ADR 取代 |
| 已拒绝 | 评审未通过，保留以记录考虑过的方案 |

## 索引

| 编号 | 标题 | 状态 |
|---|---|---|
| [0001](0001-business-hierarchy.md) | 业务层级模型 | 提议 |
| [0002](0002-composable-deployment.md) | 逻辑架构按终态设计，部署从少量单元开始 | 提议 |
| [0003](0003-subsystem-boundaries.md) | 子系统划分与数据所有权 | 提议 |
| [0004](0004-event-backbone-kafka-outbox.md) | 事件骨干：Kafka + 事务性发件箱 + CloudEvents | 提议 |
| [0005](0005-durable-workflow-temporal.md) | 持久化工作流：Temporal | 提议 |
| [0006](0006-primary-language-python.md) | 主语言：Python；前端：TypeScript | 已替代（ADR-0014） |
| [0007](0007-storage-portfolio.md) | 存储组合 | 提议 |
| [0008](0008-tenant-isolation-and-cells.md) | 多租户隔离与租户单元 | 提议 |
| [0009](0009-extension-packs.md) | 三类扩展包 | 提议 |
| [0010](0010-governed-agent-runtime.md) | 受治理的 Agent 运行时 | 提议 |
| [0011](0011-immutable-evidence-dual-versioning.md) | 证据不可变与双版本 | 提议 |
| [0012](0012-identity-keycloak-openfga.md) | 身份与授权：Keycloak + 关系型授权 | 提议 |
| [0013](0013-tiered-persistence-and-search-enrichment.md) | 公共域分层持久化与检索增强 | 提议 |
| [0014](0014-polyglot-backend-python-go.md) | 服务端语言：数据抓取与维护用 Python，平台服务用 Go | 已批准 |
| [0015](0015-self-hosted-open-source-infrastructure.md) | 基础设施开源自建优先 | 已批准 |
| [0016](0016-model-inference-via-api.md) | 前期 AI 推理只接 API | 已批准 |

## 待提出的 ADR

| 主题 | 触发 |
|---|---|
| IaaS 供应商选择（ADR-0015 后只涉及 IaaS） | 商务报价完成后 |
| 图查询引擎选择 | 图查询基准测试完成后 |
| 模型 API 供应商清单 | 评测完成后 |
| 授权数据供应商 | 商务评估完成后 |

## 模板

```markdown
# ADR-NNNN：标题

> 状态：提议 · 日期：YYYY-MM-DD · 决策者：…

## 背景
## 决策
## 备选
## 后果
```
