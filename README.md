# 妙洽 MiaoQia

> 懂你业务、会取证、能推理、可协作的 AI 商业机会研究与决策系统。

妙洽帮助企业持续感知外部世界的变化，围绕自己的商业目标完成调查与判断，并把判断转化为可追踪的行动和可复盘的结果。它通过多源数据服务获取并治理外部信号，结合每个客户的产品、能力、资质、区域和目标，由 AI Agent 调用受控工具完成取证与交叉验证，再由人批准决策并推进。

妙洽不是信息聚合网站、关键词订阅工具或 AI 内容生成工具。采集与入库是可靠的上下文供应链，产品价值发生在研究、判断、决策和推进中。

## 业务层级

```text
L0 平台             妙洽：商业机会研究与决策系统
 └─ L1 应用领域      招投标与政企采购（首个） | 企业客户开发 | 招商引资 | 投资研究 | …
     │   └─ 消费 →  信息领域：公共采购、项目审批、企业工商、司法信用、知识产权 …
     └─ L2 客户细分  价值链角色 × 行业 × 规模/区域
         └─ L3 商业目标   直接投标、进入供货、规格入围、合作组队、竞争情报、风险评估 …
             └─ L4 机会路径  阶段 · 研究模板 · 判断框架 · 决策门 · 行动
                 └─ L5 运行实例  信号 → 研究 → 判断 → 决策 → 推进 → 结果 → 学习
```

- 招投标是第一个**应用领域**，不是产品边界；
- 管材/建材等是招投标应用领域中**行业**维度上的取值，与价值链角色正交；
- 直接投标只是众多**商业目标**之一，同一个客户可同时追求多个目标。

## 系统架构

18 个子系统分属四个平面，逻辑架构按终态设计，部署从两个单元（`acquisition`、`platform`）开始，通过配置演进为独立服务。

```text
交互平面        gateway · channels
客户智能平面    customer-context · detection · research · assessment · pursuit · learning
数据供应平面    acquisition · evidence · domain-intelligence · entity-graph · search
平台平面        identity · commerce · governance · ai-platform · operations
```

## 文档

正式文档位于 [`docs/`](docs/README.md)：

| 层 | 入口 |
|---|---|
| 术语 | [docs/glossary.md](docs/glossary.md) |
| 产品 | [愿景与定位](docs/01-product/01-vision-and-positioning.md) · [商业模式](docs/01-product/02-business-model.md) · [能力地图](docs/01-product/03-capability-map.md) |
| 业务 | [业务架构](docs/02-business/01-business-architecture.md) · [商业目标](docs/02-business/02-commercial-objectives.md) · [信息领域](docs/02-business/03-information-domains.md) · [应用领域](docs/02-business/04-application-domains.md) · [招投标](docs/02-business/application-domains/tendering/README.md) |
| 架构 | [总体架构](docs/03-architecture/01-architecture-overview.md) · [子系统](docs/03-architecture/subsystems/README.md) · [部署拓扑](docs/03-architecture/07-deployment-topology.md) |
| 决策 | [ADR](docs/04-decisions/README.md) |
| 交付 | [立项准入清单](docs/05-delivery/00-initiation-readiness.md) · [路线图](docs/05-delivery/01-roadmap.md) · [质量与验收](docs/05-delivery/02-quality-and-acceptance.md) · [研发规范](docs/05-delivery/03-engineering-standards.md) |

## 项目状态

立项设计阶段：业务层级、商业目标、子系统边界、契约原则与部署拓扑已形成评审稿，ADR 处于“提议”状态，待评审批准后进入 M0 工程基座建设。

## Tender Intel

[Tender Intel](https://github.com/lansespirit/tender-intel) 只是立项前的参考原型。妙洽全部自主开发，不迁移其代码、数据结构或运行形态，只借鉴其实践经验。见 [docs/03-architecture/10-tender-intel-reference.md](docs/03-architecture/10-tender-intel-reference.md)。

## 品牌

| 项目 | 内容 |
|---|---|
| 中文名称 | 妙洽 |
| 英文名称 | MiaoQia |
| 域名 | [miaoqia.com](https://miaoqia.com) |
| 正式定位 | AI 商业机会研究与决策系统 |
