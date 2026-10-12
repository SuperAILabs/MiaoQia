# 归档材料

本目录保存过渡期的调研、分析和评审材料。它们**不是正式文档**，不作为研发和业务决策依据。其中仍然有效的内容已被吸收进 `docs/` 下的正式文档。

| 材料 | 时间 | 状态 | 已被吸收到 |
|---|---|---|---|
| `2026-initiation-analysis/AI_NATIVE_OPPORTUNITY_INTELLIGENCE_SAAS_BLUEPRINT_2026-07-26.md` | 2026-07 至 2026-10 | 已废弃 | 市场与竞品判断 → `01-product/`；领域对象与证据原则 → `02-business/`、`03-architecture/02-data-architecture.md`；安全设计 → `03-architecture/06-security-and-tenancy.md`；MCP 设计 → `subsystems/gateway.md` |
| `2026-initiation-analysis/POSITIONING_AND_STRATEGY_REVIEW_2026-10-12.md` | 2026-10-12 | 已废弃 | 价值链角色分析 → `02-business/application-domains/tendering/`；部署单元建议 → `03-architecture/07-deployment-topology.md` |

## 归档时修订的结论

以下观点出现在归档材料中，但已被正式文档**否定或修正**，不得再引用：

1. **业务层级**：评审稿把“管材/建材”与“招投标”放在同一层级讨论。正式层级为：平台 → 应用领域（如招投标）→ 客户细分（价值链角色 × 行业 × 规模区域）→ 商业目标 → 机会路径。管材/建材只是招投标应用领域里行业维度上的一个取值。见 `02-business/01-business-architecture.md`。
2. **以投标为唯一目标**：蓝图以 bid/no-bid 作为核心决策。正式文档把直接投标视为多个商业目标之一，与进入供货、规格入围、分包、合作组队、竞争情报等目标并列。见 `02-business/02-commercial-objectives.md`。
3. **分阶段过渡架构**：评审稿建议首期把 PostgreSQL 当队列、暂不引入工作流引擎、合并服务边界。正式方案为：**逻辑架构按终态设计，部署从两个单元开始**，基础设施一次选定。见 ADR-0002。
4. **先做细分客户付费验证再建设**：正式策略不以细分客户验证作为建设前置门槛，改为以交付质量标准作为上线门槛。见 `05-delivery/02-quality-and-acceptance.md`。
