# VPS 公共测试服务接入

> 状态：已验收 · 负责人：平台工程负责人

公共测试栈位于境外的公共测试 VPS。现场盘点确认宿主机运行 PostgreSQL 18，`miaoqia-public` 项目另有 PostgreSQL、Kafka、Keycloak、OpenFGA 和 OpenTelemetry Collector。公共栈的 PostgreSQL 含 `miaoqia_keycloak`、`miaoqia_fga` 及管理库。采购数据面的历史组件验证使用临时实例。常驻公开原件、采集库、公共索引及真实事件流的部署目标为境内开发 VPS，需使用对应的 SSH 入口。[部署拓扑第 6 节](../03-architecture/07-deployment-topology.md#6-环境)

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

这两个数据库存公共服务自身的认证和授权状态。公开数据存储类的 `acq`、`evd`、`dom`、`ent` 与公共检索 journal 的 `sea_public` 在境内开发 VPS 的公开数据面专用 PostgreSQL 集群中分别使用独立数据库与账号，常驻实例待部署验收。本机开发与 E2E 按子系统索引生成独立的本地数据库，其中 `sea` 为租户检索库。[公共数据面引导](../../deploy/vps/data-plane/README.md#发布顺序) · [本地数据库引导](../../deploy/local/postgres/10-subsystems.sql)

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

### 合成数据面的兼容存储验证

2026-11-19 在开发 VPS 的独立 Podman 网络上，使用 PostgreSQL 17.6、SeaweedFS 4.54 和 Python 3.12 跑通 **12 项**合成验收。测试范围包括冻结批次的原件字节读取、哈希校验、两个 S3 兼容桶间的备份恢复，以及安徽工程固定 mock 的采集、解析和 mTLS 权威读取。临时容器不映射宿主端口，测试结束后回收。验证使用未提交工作树的测试快照，打包时排除了语料目录，未运行实网采集。

测试快照 SHA-256 为 `d0e459381e7fcdbeeb265a79c9118e0bf76e90c030e49fa43dce2f4d6f7f6b5a`。命令在 `data/` 运行，`MIAOQIA_TEST_PG` 和 `MIAOQIA_TEST_FROZEN_S3_ENDPOINT` 指向同一临时网络的实例。

```sh
uv run --locked --all-packages --group dev pytest -q \
  subsystems/acquisition/tests/test_frozen_batches.py \
  subsystems/domain-intelligence/tests/test_mock_anhui_award.py
```

同一开发 VPS 上的数据面合成验收使用独立 PostgreSQL 17.6、SeaweedFS 4.54 和 Kafka 4.1.0。代码与契约快照 SHA-256 为 `fa6f1d118aaca28020fb061aac407bf6ba6bec97325eb1dfe9aaedb8ba2dfc48`，**143 项通过**，覆盖窗口计划并发、领取与恢复、站点列表预检和分页、Kafka 发件箱与死信、正文与附件的授权冻结读取、待解析观察回放及安徽 mock。验证使用未提交工作树和合成夹具。临时网络、容器及压缩包已清理。真实来源持续运行及目标境内存储仍按 C11 验收。

分组公告测试快照 SHA-256 为 `f7c82efbd71c1dc6c1babfeda8139a7dc64f198e0371dd1426090eaad7fa065d`。独立开发 VPS 环境通过 **53 项**合成测试，另有两条分组观察由 Go 发件箱中继经 Kafka 交给 Python evidence 消费，再从 SeaweedFS 读取并重放。测试核对了湖南启用配置、两页采集预算及默认 30 天回填设置。测试包排除了真实来源夹具和语料。

冻结供数授权与分组备份恢复快照 SHA-256 为 `f124b5b7efd41a93619f48f1c6c3d1f0a2f51622b8be847a245e713eb16b9eba`，同样使用隔离组件和合成输入，**55 项通过**。覆盖单公告与分组公告的只读权限文件、对批次替换和已撤权来源的拒绝，以及归档运维的分组原件恢复。两条分组观察再次通过跨语言中继和回放。[供数装配](../03-architecture/subsystems/acquisition.md#12-当前实现边界)

浏览器双对象快照 SHA-256 为 `297bce679d3a09a69e3bd88a0977d83588eaeb2d76d9ab1a9d71ef51d4c72c88`。在独立 PostgreSQL、SeaweedFS 和 Kafka 容器中通过 **208 项**合成测试，覆盖浏览器任务许可、配对观察、原件和投影分别恢复、冻结供数及旧版兼容。两条配对观察经过 Go 中继与 Python 消费者，等待解析后从两份归档离线重放。另用两个隔离 Podman 网络验证夹具客户端只能经允许的网关路径取得夹具正文，直连和未许可路径均失败。该网络夹具使用内部 HTTP；发布者 TLS、Chromium 独立部署及授予真实站点出口权限另行验收。

浏览器列表发现快照 SHA-256 为 `a57b20b8d61b1827715943fc92c29b981bb5d1574f3c7376118bb63bbee1e43e`。独立 PostgreSQL 17.6 与 SeaweedFS 4.54 环境的 **122 项**合成测试通过，覆盖列表捕获校验、发现收据、原件到期、归档故障恢复及冻结批次。该轮同时复查了[泸州通知公告](12-public-source-probe.md#泸州通知公告复查)，临时资源随后清理。

同一 VPS 的隔离进程夹具用受监督 Chromium 从合成 TLS 发布者获取列表和正文。采集进程停止后，Go 中继发布 V4 及来源健康变更。正式 `data-consumer` 提交 Kafka 位点 2，Evidence 只有一条待解析观察。停止消费者后，显式重放从同一 S3 归档产生一个规范化快照，重复重放处理零条，死信保持为空。独立检查核对源码摘要和远端临时目录清理。[同次采集交付收据](../../deploy/vps/data-plane/browser/receipt-provider-ingest.jsonl)

### 开发运行恢复演练

公共测试 VPS 上的独立演练在同一台主机重建了 PostgreSQL 与 SeaweedFS，校验数据库导出、记录指纹和原件哈希，并完成 PostgreSQL 17.6 到 17.7 的升级及从检查点回退。演练范围及未验收项记录于 [C11 开发验收记录](../../contracts/reviews/c11-dev-operations.yaml)。

发布包中的归档校验工具已在开发 VPS 上通过候选镜像和隔离 PostgreSQL 的校验。工具从权威数据库引用读取原件并核对规范化制品，缺失对象使校验失败。本机的独立 PostgreSQL 集群和 SeaweedFS 桶使用实际迁移，确认源端、导出副本和恢复端的收据一致，覆盖还在留存期的列表、附件、浏览器配对、分组及证据制品。备份清单工具另在本机验证了五份可读取的 PostgreSQL 转储、对象副本和相同的归档收据，并拒绝缺件、篡改和路径替换。独立备份目的地和恢复 VPS 仍待验收。[恢复手册](../../deploy/vps/data-plane/recovery.md)

### 公共数据面发布包验证

源码候选 SHA-256 为 `a60d6c229fefe9d2ba6f482e5956f05be81e832f79c52aaff47ab32ecde40070`。在公共测试 VPS 上以独立 Podman 存储重建 Linux amd64 镜像，重复构建一致。Python 镜像摘要为 `sha256:b94c30e488ae3a388f8aed88f1ab6352a85c87e081142c6e50efc27fc52e9a95`，Go 镜像摘要为 `sha256:8c897cb8fa46012232c65ba464103cb485d8ef26ad36e1a160521af62283b8f3`。两个镜像均以非 root 账号启动，提供与当前发布快照一致的角色清单。Python 镜像识别 40 个来源。

VPS 的 Compose 提供者渲染 14 个服务，默认启动范围为空，合成角色的原样环境值从文件进入容器后保持一致。用相同候选镜像对隔离 PostgreSQL 17.6 执行三条一次性迁移并重复执行，核对 `acq` `0017`、`evd` `0008`、`dom` `0007`、`ent` `0002`、`sea_public` `2`。验收使用临时私网和合成配置。共享存储、真实访问凭据和常驻角色按 C11 及逐源结果继续验收。[发布包与装配命令](../../deploy/vps/data-plane/README.md)

两份一次性 SQL 在该 VPS 的隔离 PostgreSQL 17.6 建立五库和默认禁止登录的基础角色。在本机隔离 PostgreSQL 18 另行验证迁移、读取角色和已建库拒绝重新引导。专用账号完成安徽模拟公告的 mTLS 事实和证据读取、公告与分块投影及撤权、模拟组织的读取和投影。反例覆盖跨库连接、发件箱旁路、改写事实和投影账本键。将合成五库导出并恢复到第二个隔离集群，验证授权和选定记录在脱离备份及重启后保持一致。[恢复操作与验收范围](../../deploy/vps/data-plane/recovery.md)

一次性采集覆盖报告角色仅接私网，核验独立登录并读取限定字段的视图与账本列。本机隔离 PostgreSQL 验证安徽与分组合成回填证据及 JSON 字段筛选，VPS 隔离候选镜像验证 40 个目录项、受阻标记、指定区间缺口、缺数据库配置和权限拒绝。实际 30 天覆盖以常驻环境账本为准。[报告入口](../../deploy/vps/data-plane/README.md#采集覆盖查验)

在公共测试 VPS 上独立渲染十种服务组合，每组只提供所选角色的环境文件。两个搜索 worker 可在同一项目中按不同 generation 分别启动。开机恢复 unit 通过 systemd 语法校验，Podman 验证筛选和故障挂载恢复。安装后的宿主机重启、常驻依赖恢复及双代次实时接续待实际演练。[开机恢复](../../deploy/vps/data-plane/README.md#开机恢复) · [C11 收据](../../contracts/reviews/c11-dev-operations.yaml)

同一 VPS 的隔离 OpenSearch 验证了 reader、writer 和一次性 publisher 的独立客户端证书。Go 查询用 reader 完成限定范围的别名读取、PIT 及权威核验正反控。writer 写入投影和墓碑，publisher 发布及回退代次。reader 写入、writer 改别名及创建代次、相邻命名空间的直接和批量写入均被拒绝。试验节点、证书和网络已回收。常驻集群与生产凭据仍须独立验收。[公共检索装配](../../deploy/platform/public-search.md) · [隔离收据](../../contracts/reviews/c11-dev-operations.yaml)

源码候选 `51d0b6a1413311537de180ecdb536ed6b10816f021e4c78a3de53b53ceb8abde` 在同一 VPS 的独立 Podman 存储重建 Python 镜像，使用 PostgreSQL 17.6 验证待解析预验命令。`mq_evidence_assess_ro` 可读取合成待解析观察，数据库拒绝该身份读取快照、删除队列、建临时表和切换为 owner。合成正例生成带引用的字段证据，空队列、无匹配提取器或缺正文均报告独立失败。三条观察继续留在队列，归档清单未变化，快照、修订和发件箱均无写入。隔离容器、网络和候选镜像验后清理。[逐源解析预验](../03-architecture/subsystems/evidence.md#12-实现状况)
