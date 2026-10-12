# ADR-0006：主语言：Python；前端：TypeScript

> 状态：提议 · 日期：2026-10-12 · 决策者：首席架构师、产品负责人

## 背景

平台以数据处理、文档解析与 AI 为核心；已有 Tender Intel Python 资产与测试。

## 决策

后端全部子系统、扩展包与 SDK 使用 Python 3.12+（FastAPI、Pydantic v2、SQLAlchemy 2、uv workspace）；前端使用 TypeScript + React。只有在出现经测量证明的性能瓶颈时，才允许个别组件采用 Go 实现，且必须保持相同契约。

## 后果

- 正面：一套语言覆盖数据、AI 与业务；扩展包作者门槛低；资产可直接复用；
- 代价：CPU 密集型路径需注意并发模型与进程化；需要严格的类型检查（mypy/pyright）与性能测试。
