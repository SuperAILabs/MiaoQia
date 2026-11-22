# VPS 公共测试服务接入

> 状态：已验收 · 负责人：平台工程负责人

VPS 承载 Kafka、Keycloak、OpenFGA、OpenTelemetry Collector，以及 Keycloak/OpenFGA 各自独立的 VPS PostgreSQL 数据库，即开发环境的 `stream` 存储类与 `platform` 存储类中的基础组件部分。MiaoQia 业务子系统的数据库不放在 VPS：公开数据存储类（`acq`、`evd`、`dom`、`ent` 与对象存储）放在境内公开数据主机，其余业务数据库在开发者本机；放置与驻留规则以[部署拓扑第 6 节](../03-architecture/07-deployment-topology.md#6-环境)为准。VPS 只承载合成数据与夹具，以及该节允许的公开数据链路事件。

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

这两个数据库只存公共服务自身的认证和授权状态，不存 MiaoQia 业务对象。MiaoQia 的 17 个业务数据库都不在 VPS 上：`acq`、`evd`、`dom`、`ent` 属于公开数据存储类，常驻实例放在境内公开数据主机（尚未部署）；`ctx`、`det`、`res`、`asm`、`pur`、`lrn`、`gwy`、`chn`、`iam`、`com`、`gov`、`aip`、`ops` 在开发者本机 PostgreSQL 中维护；本机开发与 E2E 同样使用本机的 17 个库。VPS 不访问任何业务数据库。

## 3. MiaoQia 测试环境变量

本机运行 VPS E2E 时，MiaoQia 数据库地址由 `MIAOQIA_TEST_PG`/`MIAOQIA_E2E_PG` 决定；`tools/dev/run-m0-vps.sh` 显式选择 `config/local/m0-vps.env`，公共组件地址和密钥由该文件提供，显式 shell 变量可覆盖文件配置。普通开发验收不会自动读取该文件。

OpenFGA token 和 Keycloak 测试用户密码属于部署密钥，不写入仓库；从 VPS `/opt/miaoqia-public/.env` 的受控密钥存储中注入本机进程。不要把这些值放入 shell 历史、CI 日志或文档。

配置一次后直接运行：

```sh
tools/dev/configure-m0-vps-env.sh
tools/dev/run-m0-vps.sh
```

`run-m0-vps.sh` 会临时把本机 `29094` 转发到 VPS Kafka 的内部 listener，测试结束后自动关闭。它不会转发或暴露 PostgreSQL；E2E 的业务数据库使用本机 PostgreSQL。

## 4. 干净主机重建

从同一个候选提交取 [VPS Compose 清单](../../deploy/vps/miaoqia-public.compose.yaml)、[PostgreSQL 初始化脚本](../../deploy/vps/postgres/01-init-public-databases.sh)、[VPS Collector 配置](../../deploy/vps/otel/collector.yaml)和 [Nginx 模板](../../deploy/vps/mq-public.nginx.conf)。将 Compose 清单安装为 `/opt/miaoqia-public/compose.yaml`，其余挂载源保留 `postgres/`、`otel/` 相对目录。Collector 使用只读文件挂载，配置缺失时容器启动报错。它仅在宿主机 `127.0.0.1:14318` 接收 OTLP/HTTP 追踪，与 Nginx 上游地址一致。

平台工程负责人在启动前提供部署专用 `.env`、`keycloak/` 下供 `--import-realm` 使用的 `miaoqia` realm 导入文件及 Nginx origin TLS 文件。Keycloak 使用受控的 VPS 测试 realm 导出，仓库内的本地/CI realm 含开发夹具凭据，仅用于本地和 CI。预先创建 `miaoqia_public` 外部网络（内部网络，`10.89.14.0/24`）、专用 Kafka 数据目录，并以 `0700` 权限创建 `/opt/miaoqia-public/data/otel`。进入 `/opt/miaoqia-public` 后执行 `docker compose config --quiet` 校验，再启动 PostgreSQL 并等初始化完成，随后启动其余服务。安装 Nginx 模板后执行 `nginx -t && systemctl reload nginx`。

在部署目录内先检查 Collector 配置，再使用以下探针核对接收与落盘。探针只发送合成追踪，使用 Bash、`openssl`、`curl` 和 `jq`，以有权读取私有 `data/otel` 的运维账号执行。

```bash
set -eo pipefail
docker compose run --rm --no-deps otel-collector validate --config=/etc/otel/collector.yaml
trace_id=$(openssl rand -hex 16)
span_id=$(openssl rand -hex 8)
timestamp=$(date +%s)
curl -fsS --max-time 10 -H 'Content-Type: application/json' --data-binary @- \
  http://127.0.0.1:14318/v1/traces <<EOF | jq -e '(.partialSuccess.rejectedSpans // 0 | tonumber) == 0'
{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"$trace_id","spanId":"$span_id","name":"mq-vps-collector-smoke","startTimeUnixNano":"${timestamp}000000000","endTimeUnixNano":"${timestamp}000000001"}]}]}]}
EOF
for attempt in $(seq 1 20); do
  grep -Fq "$trace_id" data/otel/traces.jsonl 2>/dev/null && break
  sleep 1
done
grep -Fq "$trace_id" data/otel/traces.jsonl
```

Collector 将追踪写入 `data/otel/traces.jsonl`，轮转时最多保留五份归档，并清理超过七天的归档。重建证据记录候选 SHA、Collector 镜像、时间和合成追踪 ID。整栈另需核验 `miaoqia` issuer、OpenFGA 健康、Kafka 主题、Nginx 与外部接入。迁移或升级前保存 Compose、Nginx、网络检查结果和 `miaoqia_fga`/`miaoqia_keycloak` 数据库导出，保留现有数据卷。远端网络与健康检查使用 [`tools/dev/check-vps-public-network.sh`](../../tools/dev/check-vps-public-network.sh)。

## 5. 当前验收边界

现有公共网络验收记录确认 Keycloak issuer 可达，OpenFGA `/healthz` 返回 `SERVING` 且数据库迁移退出码为 0，PostgreSQL、Keycloak、OpenFGA 均只接入 `miaoqia_public`。迁移前备份位于 VPS `/opt/miaoqia-public/backups/network-migration-20261104T161618Z/`。Collector 的 HTTP 接收与落盘按重建探针单独验证。M0 的应用追踪 exporter 使用 OTLP/gRPC，走 VPS HTTP 入口的应用追踪仍需独立的协议适配和端到端验证。

`tools/dev/run-m0-vps.sh` 运行的 M0 E2E 覆盖迁移、探针、审计、授权拒绝、远程 identity 绑定和“官方中标公告 → evidence → domain fact → 发件箱”的数据平面回放。data-api 的 mTLS 读取、detection 收件箱、gateway 对象页和 channels 简报由 [A02 独占环境进程回放](09-a02-end-to-end-acceptance.md#3-分级验收)覆盖。公共服务使用 VPS PostgreSQL，本机 MiaoQia 业务进程使用本机 PostgreSQL。
