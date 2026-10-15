# 代码库结构

> 状态：评审中 · 版本：3.0 · 更新：2026-10-15 · 负责人：首席架构师

## 1. 单一代码仓库

妙洽采用单一代码仓库（monorepo）管理所有子系统、扩展包、契约、前端与部署配置。子系统的独立性由目录边界、独立数据库与自动化依赖检查保证，而不是由仓库隔离。子系统使用哪种语言以 [子系统索引](subsystems/README.md) 为准（ADR-0006）。

```text
miaoqia/
├── contracts/                     契约（唯一事实来源，与语言无关）
│   ├── schemas/                   JSON Schema：common/ 通用类型、domains/<信息领域>/ 领域对象、研究产物
│   ├── apis/<subsystem>/          子系统接口 OpenAPI
│   ├── events/<namespace>/        事件 AsyncAPI + Schema
│   ├── policy/                    策略 DSL Schema 与 CEL 环境声明
│   ├── packs/                     扩展包定义 Schema
│   ├── storage/                   内核表（发件箱、消费去重），Go 与 Python 内核共用
│   ├── tests/                     契约样例（正例与反例）
│   └── identifiers.yaml           标识前缀登记
│
├── services/                      Go 模块：平台服务
│   ├── contracts/                 生成的领域类型（请勿手改）
│   ├── kernel/                    通用基础设施（数据访问与 RLS、发件箱、事件消费、工作流、
│   │                              配置、可观测、接口绑定、授权与审计 SDK、扩展包加载与 CEL 求值）
│   ├── subsystems/<name>/         每个 Go 子系统一个目录
│   ├── archtest/                  依赖规则检查（R1–R3、R5）
│   └── cmd/platform/              platform 部署单元入口与进程角色（含发件箱中继角色）
│
├── data/                          Python 工作区（uv）：公共域数据处理
│   ├── contracts/                 miaoqia.contracts：生成的 Pydantic 模型（请勿手改）
│   ├── kernel/                    miaoqia.kernel：数据访问与 RLS、发件箱写入、事件消费、
│   │                              Temporal worker、配置、可观测、请求上下文、授权客户端
│   ├── sdk/                       miaoqia.sdk：信息领域包 SPI
│   ├── subsystems/<name>/         每个 Python 子系统一个包
│   └── runtimes/{acquisition,data}/  两个 Python 部署单元入口与进程角色
│
├── packs/                         扩展包
│   ├── information_domains/       Python 代码 + 声明（如 procurement/ 即 INF-PROC）
│   ├── application_domains/       纯声明 + CEL（如 tendering/ 即 APP-TENDER）
│   └── industries/                纯声明
│
├── web/                           前端（TypeScript 工作区）
│   ├── apps/workspace/            客户 Web 应用
│   ├── apps/ops-console/          运营控制台
│   └── packages/                  组件、生成的契约类型（contracts/）与 API 客户端、视图注册表
│
├── deploy/                        subsystems.yaml（由子系统索引生成）、<单元>/roles.yaml 角色清单、local/ 本地环境；
│                                  Helm charts、OpenTofu
├── tools/                         契约工具（contracts/，命令 mqc）、dev/ 本地开发脚本
├── tests/
│   ├── contract/                  跨子系统黑盒契约测试（同一套用例验证 Go 与 Python 实现）
│   ├── e2e/                       端到端测试
│   ├── security/                  跨租户、未知字段、提示注入、审批绕过测试
│   └── evaluation/                评估集回归
└── docs/                          本文档体系
```

## 2. 子系统内部结构

两种语言使用相同的分层，Go 子系统把 `api` 以外的层放在 `internal/` 下，由编译器保证外部不可导入：

| 层 | 内容 | Go 位置 | Python 位置 |
|---|---|---|---|
| `api` | 公开接口：命令、查询、DTO、事件定义（其他子系统唯一可导入的部分） | `<name>/api/` | `<name>/api/` |
| `domain` | 领域模型与业务规则（无框架依赖） | `<name>/internal/domain/` | `<name>/domain/` |
| `application` | 用例编排、事务边界、事件发布 | `<name>/internal/app/` | `<name>/application/` |
| `infrastructure` | 数据访问、外部适配、事件消费者、工作流与活动 | `<name>/internal/infra/` | `<name>/infrastructure/` |
| `extensions` | 本子系统定义的扩展点注册表 | `<name>/internal/ext/` | `<name>/extensions/` |
| `migrations` | 本子系统数据库迁移 | `<name>/migrations/` | `<name>/migrations/` |

迁移随部署产物分发，不依赖源码目录：Go 以 `go:embed` 编入二进制；Python 迁移位于包内（`src/miaoqia/<name>/migrations/`），作为包资源进入 wheel，由 `importlib.resources` 定位。CI 以 `tools/dev/check_installed_migrations.py` 从 wheel 安装到临时环境后验证可定位、可被 Alembic 读取。

## 3. 依赖规则（CI 强制）

| 规则 | 说明 |
|---|---|
| R1 | 子系统 A 只能导入子系统 B 的 `api` 包，禁止导入其其余各层 |
| R2 | 依赖方向符合 [总体架构](01-architecture-overview.md#22-依赖规则)：交互 → 客户智能 → 数据供应；数据供应平面不得导入客户智能平面 |
| R3 | 所有子系统可依赖本语言的 `kernel` 与 `sdk`；`kernel` 与 `sdk` 不依赖任何子系统或领域包；生成契约也不反向依赖领域包 |
| R4 | 信息领域包的代码只依赖标准库、自己的代码、`sdk` 与被扩展子系统的 `api`；不导入 kernel、生成契约或其他包实现 |
| R5 | `domain` 层不依赖任何框架与基础设施库 |
| R6 | 子系统不得访问其他子系统的数据库（运行时：独立数据库账号无权限；静态：禁止引用其他子系统的表模型） |
| R7 | 跨语言依赖只能经 `contracts/` 生成的远程客户端，不共享任何手写代码 |

执行方式：Go 以 `internal/` 可见性与依赖规则测试（`services/archtest`，读取依赖图与 `deploy/subsystems.yaml` 中的平面）检查；Python 以 import-linter（`data/pyproject.toml`）检查 R1、R3、R5；R2 目前因全部 Python 子系统处于数据供应平面而无跨平面边，新增其他平面 Python 子系统时须扩充；R4 由 `tools/dev/check_python_boundaries.py` 对信息领域包的静态导入检查；同一工具检查 SDK/kernel/生成契约对领域包的反向静态依赖。领域包根命名空间从 manifest 对应 src 发现，不按包名前缀猜测，且不得覆盖 miaoqia 或标准库。源码包不要求安装为工作区依赖。两者都辅以数据库权限。静态检查不能证明动态导入、HTTP、事件或数据库的运行时隔离。

## 4. 接口绑定

同语言内，`kernel` 提供接口绑定机制，调用方只依赖接口：

```go
// 调用方只依赖 api 包中的接口
import "github.com/superailabs/miaoqia/services/subsystems/search/api"

type ChunkSearchTool struct {
    search api.SearchQueries
}
```

部署配置决定接口绑定到本地实现还是由 OpenAPI 生成的远程客户端；跨语言时（例如 Go 的 `research` 调用 Python 的 `entity-graph`）只有远程客户端一种绑定。所有绑定都经过统一的调用管线：上下文传递、授权、超时、重试策略、追踪与指标。

## 5. 代码生成

契约工具位于 `tools/contracts/`（命令 `mqc`），CI 工作流为 `.github/workflows/contracts.yml`。

```text
contracts/schemas ──打包（统一类型名、改写跨文件引用、去掉只用于校验的关键字）──► 打包 Schema
   ├─► go-jsonschema               → services/contracts/contracts_gen.go
   ├─► datamodel-code-generator    → data/contracts/src/miaoqia/contracts/models.py
   └─► json-schema-to-typescript   → web/packages/contracts/src/index.ts
contracts/apis/<子系统>/openapi.yaml
   └─► ogen                        → services/contracts/apis/<子系统>/（客户端与服务端）
```

- **生成类型只表达结构**：证据必填、按类型的条件必填、标识格式等约束不进入类型，入站数据一律用原始 JSON Schema 校验（Go 用 santhosh-tekuri/jsonschema，Python 用 jsonschema）；
- **类型名**：顶层 Schema 取 `title` 的首个词，`$defs` 取 `x-type-name` 或键名的 PascalCase，重名即报错；
- **生成物提交到仓库**，`mqc generate --check` 校验其与契约一致；生成器版本分别固定在 `tools/contracts/gotools/go.mod`、`tools/contracts/pyproject.toml`、`web/packages/contracts/package.json`；
- **Schema 写法**受方言规则约束（无 null、禁止额外属性、前缀须登记等），规则清单见 `tools/contracts/src/mqc/lint.py`；
- **往返测试**：全部正例在 Go 与 Python 中按生成类型严格解析（未知字段即失败）；
- **接口绑定**：远程客户端与内部接口服务端由 ogen 生成；子系统的 `api/remote` 把生成的客户端适配为 `api` 中的接口，`api/apitest` 是提供方契约用例，本地与远程绑定都必须通过。

| 命令（在仓库根目录） | 作用 |
|---|---|
| `uv run --project tools/contracts mqc check` | Schema 方言、样例、事件契约、扩展包一致性、YAML 键名 |
| `uv run --project tools/contracts mqc generate` | 重新生成三种语言的类型 |
| `uv run --project tools/contracts mqc generate --check` | 生成物一致性检查 |
| `uv run --project tools/contracts mqc fixtures --export DIR` | 导出正例，供往返测试 |
| `uv run --project tools/contracts mqc compat --base origin/main -v` | 与目标分支比较的兼容性检查（合并请求必过） |

**版本与兼容性**：每个 Schema 文件带 `x-version`。变更分 breaking / additive / doc 三级：breaking 在 1.0 起须升主版本、0.x 阶段须升次版本，additive 须升次版本；被改动类型的所在文件与引用它的全部顶层 Schema 都须升级。已发布事件不允许不兼容变更，须发布 `.v<n+1>`；标识前缀不得删除或改义；扩展包内容变化须升包版本，策略模板内容变化须升模板版本。判定规则的完整清单见 `tools/contracts/src/mqc/compat.py`。OpenAPI组件引用展开后参与兼容比较；稳定API破坏性变更须新路径主版本并保留旧路径。SQL变化须升版本并提供可审查迁移文件，CREATE IF NOT EXISTS不能代替已有表升级；标识登记还检查对象/所有者/领域归属及代码字面量前缀。冻结按具体清单记录Schema x-version、API/AsyncAPI info.version、标识/存储/集成配置version与文件哈希；事件类型版本独立于文档版本。

## 6. 命名约定

| 对象 | 约定 | 示例 |
|---|---|---|
| Go 子系统包 | `services/subsystems/<lowercase>` | `services/subsystems/customercontext` |
| Python 子系统包 | `miaoqia.<snake_case>` | `miaoqia.entity_graph` |
| 数据库 | 三字母代码，见 [子系统索引](subsystems/README.md) | `ent` |
| 事件类型 | `<namespace>.<entity>.<verb>.v<n>` | `entity.merge.applied.v1` |
| 标识前缀 | 2–4 字母 + `_` | `org_`、`sig_` |
| 接口 | 名词资源 + 动词命令 | `GET /v1/signals`、`POST /v1/policies/{id}:activate` |


## 7. 真实契约语料与语义验证

复用Python `sdk` 的语料工具，自动解析黄金集与人工契约映射分开运行：`mq-corpus check --extractor ...`评测解析字段；`--reviewed`验证已复核事实的Schema、原件哈希、证据归属/定位/哈希与业务语义，可导出正例复用三语言契约测试。人工映射不作为解析器正确率报告。实现位于`data/sdk/src/miaoqia/sdk/corpus.py`及`validation.py`，原件、映射和冻结门禁见[首批契约](13-contract-readiness.md)。

更正替换须按目标字段Schema、原值与版本校验；标识、版本、权利与分级等系统元数据不得经公告更正替换；此SDK检查不执行授权或数据库写入，领域执行器还需验证权利、并发和重放。未找到目标字段、类型不符或陈旧版本不得直接应用。

## 8. 通用代码与领域校验

SDK 的平台通用定位与 R4 的依赖方向决定它不能内置采购、企业等业务语义。`sdk.validation` 只提供通用完整性与字段替换机制；领域语义在信息领域包注册。corpus 是评测组装入口，通过 manifest 加载校验器，SDK 不静态导入领域实现。领域真实样本测试位于各 pack/tests，共享通用原件加载与语料评测工具。依赖检查不能识别业务语义硬编码，代码审查仍须检查规则归属。

## 9. 运行时事件载荷

运行时事件载荷由mqc复用原AsyncAPI引用展开，生成 `services/contracts/events/payloads.json` 与 Python契约包的 `event_payloads.json`。消费框架加载本地制品、启用format并在事务前验证，不从信封取网络Schema；编译失败或已登记契约被覆盖须阻止启动。生成正反例由两语言运行，具体消费者的业务语义由其应用层拥有。M0跨语言真实组件测试见[运行时验收](../05-delivery/05-m0-runtime-acceptance.md)。
