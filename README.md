# 妙洽 MiaoQia

> 懂你业务、会取证、能推理、可协作的 AI 商业机会研究与决策系统。

妙洽帮助企业持续感知外部世界的变化，围绕自己的商业目标完成调查与判断，并把判断转化为可追踪的行动和可复盘的结果。它通过多源数据服务获取并治理外部信号，结合每个客户的产品、能力、资质、区域和目标，由 AI Agent 调用受控工具完成取证与交叉验证，再由人批准决策并推进。

妙洽不是信息聚合网站、关键词订阅工具或 AI 内容生成工具。采集与入库是可靠的上下文供应链，产品价值发生在研究、判断、决策和推进中。

## 业务与架构概要

- 业务按 L0 平台 → L1 应用领域 → L2 客户细分 → L3 商业目标 → L4 机会路径 → L5 运行实例组织；招投标是第一个应用领域，不是产品边界。见 [业务架构](docs/02-business/01-business-architecture.md)。
- 18 个子系统分属交互、客户智能、数据供应、平台四个平面；公共域数据处理用 Python，平台服务用 Go，前端用 TypeScript；基础设施开源自建。见 [总体架构](docs/03-architecture/01-architecture-overview.md)、[子系统索引](docs/03-architecture/subsystems/README.md)、[技术选型](docs/03-architecture/08-technology-stack.md)。

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

立项设计与 M0 工程准备阶段。HTTP 认证、完整跨语言探针及现有消费者载荷校验已通过[真实组件验收](docs/05-delivery/05-m0-runtime-acceptance.md)，生产部署另行验收；各事项状态见[立项准入清单](docs/05-delivery/00-initiation-readiness.md)。

## Tender Intel

[Tender Intel](https://github.com/lansespirit/tender-intel) 只是立项前的参考原型。妙洽全部自主开发，不迁移其代码、数据结构或运行形态，只借鉴其实践经验。见 [docs/03-architecture/10-tender-intel-reference.md](docs/03-architecture/10-tender-intel-reference.md)。

## 品牌

见 [愿景与定位](docs/01-product/01-vision-and-positioning.md#8-品牌)。
