# Web 工作台

Web 工作台是妙洽的响应式浏览器应用。桌面浏览器、手机浏览器和企业微信内置浏览器共用同一套路由；业务数据经 gateway/API 读取，浏览器只持有 gateway 的 HttpOnly 会话 Cookie。

## Role

为 A02 首条客户旅程提供浏览器端页面与可复用的状态、文案和 API 访问边界，并为后续 H5 渠道落地保留同源路由。

## Owns

- `apps/workspace` 的路由、页面布局和页面级状态。
- `packages/api` 的浏览器请求封装、Problem Details 分类和最小页面资源类型。
- `packages/ui` 的状态面板、状态徽标和工作台视觉基础。
- `packages/copy` 的错误、空态、未知和未接入状态文案。

## Touchpoints

- `/v1/detection`、`/v1/channels`、`/v1/objects` 与 `/v1/evidence` 网关接口。
- `contracts/apis/*/openapi.yaml` 和 `web/packages/contracts` 生成类型。
- gateway 的浏览器会话端点 `/auth/*`；前端不保存访问令牌。
- 开发环境可用 `/auth/login` 跳转到 loopback-only 的 `/auth/dev-login`，通过 `MIAOQIA_WEB_DEV_TOKEN_URL` 或 issuer 的 `/token` 取得测试令牌；该入口不代表生产 OIDC 登录。

## Change Guide

- 新页面先确认对应 OpenAPI 路径和权限/失败关闭语义，再扩展 `packages/api`；不得在页面中拼接未经契约登记的请求。
- 新增错误码或状态必须同步 `packages/copy`，并保持“未接入、未知、暂不可用、空结果”分开呈现。
- 运行 `pnpm typecheck` 与 `pnpm build`；涉及路由或会话时补浏览器端到端验证。

## Decisions

- L2 首批页面覆盖客户档案/目标只读页、A02 信号收件箱、对象页、证据面板、简报历史、策略仿真状态、审批决定、单条与批量反馈、卡片链接落地和渠道绑定确认页；检索、关注、研究、操作撤销和生产渠道按后续批次开放。
- 开发期通过 Vite 代理复用现有本机 API 进程，不启动 Docker。
