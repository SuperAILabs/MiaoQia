# VPS 公共测试服务接入

> 状态：已验收 · 负责人：平台工程负责人

VPS 承载 Kafka、Keycloak、OpenFGA、OpenTelemetry Collector，以及 Keycloak/OpenFGA 各自独立的 VPS PostgreSQL 数据库。MiaoQia 的 17 个业务子系统数据库继续位于开发者本机 PostgreSQL 服务，不迁移到 VPS。

## 1. 访问地址

| 服务 | 地址 | 说明 |
|---|---|---|
| Keycloak | `https://auth.lai.co/realms/miaoqia` | Cloudflare 代理，测试 realm |
| OpenFGA | `https://fga.lai.co` | Cloudflare 代理，使用 API token |
| OTLP HTTP | `https://otel.lai.co/v1/traces` | Cloudflare 代理，接收测试追踪 |
| Kafka | `kafka.lai.co:29092` | VPS 外部端口；本机 E2E 通过临时 SSH 转发连接内部 listener |

Kafka 使用独立 topic 集合；Kafka、Keycloak、OpenFGA、Collector 均属于独立 `miaoqia-public` Compose 项目，不操作 VPS 上其他项目的容器。

公共网络由 MiaoQia 自己命名和管理：`miaoqia_public`（VPS 网段 `10.89.14.0/24`，内部网络）。Keycloak 使用 `10.89.14.250`，OpenFGA 使用 `10.89.14.251`，PostgreSQL 由同一 Compose 项目提供并以 `postgres` 服务别名访问。其他项目网络不属于 MiaoQia 的配置、重建或验收前置条件。版本化清单见 [`deploy/vps/miaoqia-public.compose.yaml`](../../deploy/vps/miaoqia-public.compose.yaml)，Nginx 上游模板见 [`deploy/vps/mq-public.nginx.conf`](../../deploy/vps/mq-public.nginx.conf)。

## 2. 数据库边界

VPS 现有 PostgreSQL 服务中创建了两个独立数据库和账号：

- database `miaoqia_keycloak` / role `miaoqia_keycloak`
- database `miaoqia_fga` / role `miaoqia_fga`

这两个数据库只存公共服务自身的认证和授权状态，不存 MiaoQia 业务对象。MiaoQia 的 `acq`、`evd`、`dom`、`ent`、`ctx`、`det`、`res`、`asm`、`pur`、`lrn`、`gwy`、`chn`、`iam`、`com`、`gov`、`aip`、`ops` 数据库在本机 PostgreSQL 中维护。VPS 不需要访问本机 PostgreSQL。

## 3. MiaoQia 测试环境变量

本机运行 VPS E2E 时，MiaoQia 数据库地址由 `MIAOQIA_TEST_PG`/`MIAOQIA_E2E_PG` 决定；`tools/dev/run-m0-vps.sh` 显式选择 `config/local/m0-vps.env`，公共组件地址和密钥由该文件提供，显式 shell 变量可覆盖文件配置。普通开发验收不会自动读取该文件。

OpenFGA token 和 Keycloak 测试用户密码属于部署密钥，不写入仓库；从 VPS `/opt/miaoqia-public/.env` 的受控密钥存储中注入本机进程。不要把这些值放入 shell 历史、CI 日志或文档。

配置一次后直接运行：

```sh
tools/dev/configure-m0-vps-env.sh
tools/dev/run-m0-vps.sh
```

`run-m0-vps.sh` 会临时把本机 `29094` 转发到 VPS Kafka 的内部 listener，测试结束后自动关闭。它不会转发或暴露 PostgreSQL；业务数据库始终使用本机 PostgreSQL。

VPS 首次部署或重建时，先创建 `miaoqia_public` 外部网络，再使用版本化 Compose 清单启动 PostgreSQL、Keycloak/OpenFGA，最后安装 Nginx 上游并执行 `nginx -t && systemctl reload nginx`。迁移或升级前保存 Compose、Nginx、网络检查结果和 `miaoqia_fga`/`miaoqia_keycloak` 数据库导出；不要执行会删除生产数据卷的 `down -v`。远端网络与健康检查使用 [`tools/dev/check-vps-public-network.sh`](../../tools/dev/check-vps-public-network.sh)。

## 4. 当前验收边界

2026-11-04 完成公共网络迁移并通过 [`tools/dev/check-vps-public-network.sh`](../../tools/dev/check-vps-public-network.sh)：Keycloak issuer 可达，OpenFGA `/healthz` 返回 `SERVING`，OpenFGA 数据库迁移退出码为 0，MiaoQia 的 PostgreSQL、Keycloak、OpenFGA 均只接入 `miaoqia_public`。迁移前备份位于 VPS `/opt/miaoqia-public/backups/network-migration-20261104T161618Z/`。Cloudflare 域名、Kafka topic 初始化、Collector HTTP 接收端点、Keycloak realm 和公共服务连接边界继续按下方 M0/A02 脚本验收。

`tools/dev/run-m0-vps.sh` 运行的 M0 E2E 覆盖迁移、探针、审计、授权拒绝、远程 identity 绑定，以及“官方中标公告 → evidence → domain fact → 发件箱”的数据平面回放；data-api 的 mTLS 读取由 A02 进程回放（`tools/dev/run-a02-process-replay.sh`）覆盖，M0 E2E 尚未包含。它不覆盖 detection 收件箱、gateway 对象页或 channels 简报，不关闭 A02 的 L1 贯通。公共服务不依赖开发者本机 PostgreSQL；MiaoQia 业务进程在本机运行时继续使用本机 PostgreSQL。
