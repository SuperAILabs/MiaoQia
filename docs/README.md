# 妙洽文档体系

本目录是妙洽（MiaoQia）的**正式、可执行**文档。产品、业务、架构和研发决策都以这里为准；对话记录、调研草稿和临时分析不属于正式文档，完成使命后归入 `archive/`。

## 1. 目录结构

```text
docs/
├── README.md                     文档体系说明（本文）
├── glossary.md                   术语表：全体文档共用的概念定义
│
├── 01-product/                   产品层：为什么做、做成什么
│   ├── 01-vision-and-positioning.md
│   ├── 02-business-model.md
│   └── 03-capability-map.md
│
├── 02-business/                  业务层：客户要完成什么、业务如何运转
│   ├── 01-business-architecture.md
│   ├── 02-commercial-objectives.md
│   ├── 03-information-domains.md
│   ├── 04-application-domains.md
│   └── application-domains/
│       └── tendering/            首个应用领域：招投标与政企采购
│
├── 03-architecture/              架构层：系统如何构建
│   ├── 01-architecture-overview.md
│   ├── 02-data-architecture.md
│   ├── 03-integration-contracts.md
│   ├── 04-extensibility.md
│   ├── 05-ai-and-agent-architecture.md
│   ├── 06-security-and-tenancy.md
│   ├── 07-deployment-topology.md
│   ├── 08-technology-stack.md
│   ├── 09-codebase-structure.md
│   ├── 10-tender-intel-reference.md
│   └── subsystems/               18 个子系统的规格说明
│
├── 04-decisions/                 架构决策记录（ADR）
│
├── 05-delivery/                  交付层：如何分阶段建成并验收
│   ├── 00-initiation-readiness.md 立项准入清单
│   ├── 01-roadmap.md
│   ├── 02-quality-and-acceptance.md
│   └── 03-engineering-standards.md
│
└── archive/                      已归档的过渡期材料（只读，不作为依据）
```

## 2. 阅读路径

| 读者 | 建议顺序 |
|---|---|
| 新成员 | `glossary` → `01-product/01` → `02-business/01` → `03-architecture/01` |
| 产品与业务 | `01-product/*` → `02-business/*` → `05-delivery/02` |
| 架构与研发 | `03-architecture/01` → `subsystems/README` → `04-decisions/*` → `03-architecture/09` → `05-delivery/03` |
| 领域扩展 | `02-business/03`、`04` → `03-architecture/04` → 对应应用领域目录 |
| 安全与合规 | `03-architecture/06` → `subsystems/governance.md` → `subsystems/identity.md` |

## 3. 文档层级与依赖关系

文档之间存在单向依赖，下层不得与上层矛盾：

```text
术语表
  → 产品层（愿景、定位、商业模式）
    → 业务层（业务架构、商业目标、领域）
      → 架构层（总体架构、子系统、横切设计）
        → 决策记录（固化架构层的关键选择）
          → 交付层（路线图、验收、研发规范）
```

上层文档变更时，负责人必须检查并更新受影响的下层文档。

## 4. 文档元信息

每份正式文档开头使用统一的元信息行：

```text
> 状态：草案 | 评审中 | 已批准 | 已废弃 · 版本：x.y · 更新：YYYY-MM-DD · 负责人：角色或姓名
```

| 状态 | 含义 | 是否可作为研发依据 |
|---|---|---|
| 草案 | 内容尚在形成，可能大幅修改 | 仅供讨论 |
| 评审中 | 已提交评审，等待结论 | 可作为前期设计输入 |
| 已批准 | 评审通过，作为执行依据 | 是 |
| 已废弃 | 已被替代，保留以便追溯 | 否，必须注明替代文档 |

版本号规则：结构性改动或结论变化升主版本，补充与修正升次版本。

## 5. 编写约定

- **术语一致**：只使用 `glossary.md` 中定义的术语。需要新术语时，先补充术语表。
- **事实与假设分离**：未经验证的判断标注 **[假设]**，需要外部数据确认的标注 **[待核验]**，并在文末列出。
- **可执行**：每份文档都要回答“谁据此做什么”。纯背景性内容放在附录或归档中。
- **单一事实来源**：同一事实只在一处完整定义，其他文档引用链接，不复制粘贴。
- **图表**：使用 Mermaid 或纯文本图，便于评审和版本对比。
- **标识符**：代码、服务、事件、Schema 名称使用英文；说明文字使用中文。

## 6. 架构决策记录（ADR）

重要且难以逆转的技术或业务决策必须写成 ADR，存放在 `04-decisions/`，格式见该目录的 `README.md`。ADR 一经批准不可修改正文，只能由新的 ADR 替代。

## 7. 变更流程

1. 在分支上修改文档，并在提交信息中说明影响范围；
2. 涉及多个层级或子系统边界时，需同步更新相关 ADR；
3. 由文档负责人和受影响子系统负责人评审；
4. 合并后更新文档状态与版本号。
