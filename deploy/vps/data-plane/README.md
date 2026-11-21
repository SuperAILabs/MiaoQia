# 境内开发 VPS 公共数据面

## Role

本目录定义妙洽在现有境内开发 VPS 上的常驻公开数据面发布包。部署方提供私有存储、消息端点和运行身份，发布包装配采集、数据读取和公共检索角色。

## Owns

- Python 与 Go 的运行镜像、公共数据面 Compose 清单，以及开发 VPS 原生进程的 systemd 角色模板。
- 公共数据 PostgreSQL 的库和基础角色引导。
- 公开数据异地备份、空目标恢复、备份清单和权威对象引用校验工具。[恢复手册](recovery.md)
- S3 版本桶的列表原件到期只读核验入口。[版本留存验收](#版本留存验收)
- Chromium 隔离网络、控制进程会话、TLS 归档读回及同一 V4 的中继和 Evidence 独立解析夹具。[浏览器隔离](browser/README.md)
- 发布候选的阶段化角色启动、同一发布的开机恢复、接入配置和归档暂存目录。
- 用独立 PostgreSQL 只读身份生成逐源采集覆盖报告。

## Touchpoints

- [采集角色](../../acquisition/roles.yaml)、[数据角色](../../data/roles.yaml)和[平台角色](../../platform/roles.yaml)由同一发布快照提供。
- [公共检索](../../platform/public-search.md)维护 `sea_public`、索引构建和别名切换流程。[冻结批次](../../../docs/03-architecture/subsystems/acquisition.md#12-当前实现边界)维护测试服务按权限取用的入口。
- [部署拓扑](../../../docs/03-architecture/07-deployment-topology.md#6-环境)规定公共数据放置和 C11 常驻运行验收。

## Change Guide

- 角色、凭据或网络变更时，核对实际 CLI 入口、角色的 DB scope、服务证书及 Kafka 和对象存储权限。
- 用同一版本的两张镜像分阶段渲染清单，再在独立环境验收迁移、进程启动、制品读写和退出。开机恢复验收须排除一次性任务、人工停用角色和其他项目。
- 原件、证据或对象留存的数据库契约变化时，核对 `verify_archive.py` 的对象引用范围，并用备份源端和恢复端复核收据一致性。
- 备份包布局变化时，同步 `backup_manifest.py` 和恢复手册，并复核独立清单摘要与全量读回。
- 浏览器绑定变化时，用合成发布者核对控制进程身份、实际连接、原件和投影，以及控制器丢失后未许可请求的到达数。用真实采集角色复核列表、正文、投影和 V4 发件箱。变更 V4 或消费准入时，再验证同一事件经 Kafka 入待解析队列并显式重放。[V4 交付夹具](browser/README.md#v4-交付与独立解析)
- 覆盖报告的查询变更时，同步核对 `mq_acq_coverage_ro` 的列权限，并以有历史列表、分组和公告原件的夹具验证正式报告。

## 开发 VPS 的原生角色

在现有境内开发 VPS 上，可以使用宿主机的 Python 3.12、PostgreSQL 和 systemd 运行公开数据面。采集数据库使用独立集群，原件进入专用 S3 桶。部署前核对主机身份、机房地域，以及宿主服务和容器两处的实例清单。

开发发布从同一份逐文件 SHA-256 清单安装代码、领域包和角色清单，使用 `uv.lock` 安装 Python 依赖，Go 程序从同一源码快照构建。应用只读版本目录位于 `/opt/miaoqia-data-plane/releases/`，`current` 指向运行版本。每个角色在版本的 `bin/` 下链接到对应的正式入口。角色的数据库、对象存储和消息凭据由 systemd 从 `/etc/miaoqia/data-plane/runtime/<角色>.env` 加载。配置由 root 持有并设为 `0600`。每个角色使用独立服务用户和私有状态目录。

在新版本目录中，将核验过的源码解包到 `src/`。用不可访问服务凭据的构建账号执行以下命令，`release` 是该版本目录的绝对路径。原生 Python 环境使用固定绝对路径，安装后保留该目录名。

```sh
: "${release:?set the verified release directory}"
UV_PROJECT_ENVIRONMENT="$release/venv" UV_PYTHON_DOWNLOADS=never \
  uv sync --project "$release/src/data" --python /usr/bin/python3.12 \
  --locked --all-packages --no-editable --no-dev
(cd "$release/src/services" && CGO_ENABLED=0 go build -mod=readonly -trimpath \
  -buildvcs=false -o "$release/platform" ./cmd/platform)
mkdir -p "$release/bin"
for role in acq-scheduler acq-fetcher acq-admin; do
  ln -s ../venv/bin/mq-acquisition "$release/bin/$role"
done
for role in data-consumer data-api; do
  ln -s ../venv/bin/mq-data "$release/bin/$role"
done
ln -s ../platform "$release/bin/outbox-relay"
```

管理员接管版本目录所有权并让角色只读，再建立 `current` 链接。模板的 `%i` 为角色名，例如 `acq-fetcher`。抓取角色的私有暂存路径配置为 `/var/lib/miaoqia-dev-acq-fetcher`，由 systemd 建立。模板用于接受 `--role` 参数的常驻进程。迁移、一次性覆盖报告和使用独立模块入口的搜索出口分别按各自入口执行。

安装[原生角色模板](miaoqia-dev-data@.service)后，先执行对应数据库迁移，再分角色启动。公开采集可先在开发实例持续归档，原件及事务发件箱与采集数据库一起持久化。消费者通过同一稳定版本接续待交付事件。新来源的字段解析沿用其独立准入。观察服务使用 `systemctl is-active miaoqia-dev-data@acq-fetcher.service`，采集验收另查逐源回填游标、公告观察及 S3 原件哈希。升级时先停止对应角色，切换经过验证的版本目录，再恢复进程。常驻实绩、发布权限与异机恢复分别登记在 [C11](../../../contracts/reviews/c11-dev-operations.yaml)。

## 构建和配置

从可信发布提交导出源码目录，以发布 ID 标记同次构建的两个镜像。构建上下文包含 `data/`、`services/`、`deploy/` 和 `packs/`，只包含已审查的发布输入。在导出前核对提交中的 Compose、两份 Containerfile、引导与读取授权 SQL 和开机恢复 unit，任何缺项均停止构建和发布。记录快照 SHA-256，并把该发布提交的 Unix 时间设为 `MQ_DATA_PLANE_BUILD_EPOCH`。部署环境的配置和凭据独立保存在主机上。

```sh
for release_path in \
  deploy/vps/data-plane/compose.yaml \
  deploy/vps/data-plane/python.Containerfile \
  deploy/vps/data-plane/platform.Containerfile \
  deploy/vps/data-plane/public-data.postgres.sql \
  deploy/vps/data-plane/read-roles.postgres.sql \
  deploy/platform/public-search.postgres.sql \
  deploy/vps/data-plane/miaoqia-data-plane-restart.service; do
  git cat-file -e "${MQ_DATA_PLANE_RELEASE_COMMIT:?set the reviewed release commit}:$release_path" ||
    exit 1
done
```

```sh
podman build --platform linux/amd64 --timestamp "$MQ_DATA_PLANE_BUILD_EPOCH" --identity-label=false \
  -t "$MQ_DATA_PLANE_PYTHON_IMAGE" -f deploy/vps/data-plane/python.Containerfile .
podman build --platform linux/amd64 --timestamp "$MQ_DATA_PLANE_BUILD_EPOCH" --identity-label=false \
  -t "$MQ_DATA_PLANE_PLATFORM_IMAGE" -f deploy/vps/data-plane/platform.Containerfile .
```

为 Compose 提供 `MQ_DATA_PLANE_PYTHON_IMAGE`、`MQ_DATA_PLANE_PLATFORM_IMAGE`、`MQ_DATA_PLANE_PRIVATE_NETWORK` 与 `MQ_DATA_PLANE_SOURCE_EGRESS_NETWORK`。私有网络连接数据库、Kafka、对象存储、OpenSearch、identity 及授权调用方。采集出口限制到当前启用来源及公开附件目标。各服务用内部 DNS 和与证书一致的主机名连接，没有宿主机发布端口。访问公共搜索的网关从获准的网络调用 `public-search-api`。

使用 `MQ_DATA_PLANE_ENV_DIR` 和 `MQ_DATA_PLANE_FILES_DIR` 指向部署配置目录。每个服务使用 `<service>.env` 和一个独立的 `<service>/` 文件目录。Compose 读取原样环境文件，值按 `KEY=value` 写入，JSON 和连接串使用实际值。服务内文件目录为 `/run/miaoqia`，只放该服务所需的证书和配置。使用 `MQ_DATA_PLANE_HTTP_SPOOL_DIR` 与 `MQ_DATA_PLANE_BROWSER_SPOOL_DIR` 指定两个可持久化的 `0700` 目录。容器以 `10001:10001` 运行。使用 rootless Podman 时，在其用户命名空间内核对文件读取和暂存目录所有权。

| 服务 | 配置 |
|---|---|
| `acquisition-migrate`、`data-migrate` | 对应子系统的 `MIAOQIA_<SUBSYSTEM>_DB_OWNER_DSN`。`data-migrate` 包含 `EVIDENCE`、`DOMAIN_INTELLIGENCE` 和 `ENTITY_GRAPH`。 |
| `public-search-migrate` | `MIAOQIA_PUBLIC_SEARCH_DB_OWNER_DSN`，使用 [公共检索引导 SQL](../../platform/public-search.postgres.sql) 建立库和角色。 |
| `acq-scheduler`、`acq-fetcher` | `MIAOQIA_ACQUISITION_DB_DSN`、`MIAOQIA_ACQUISITION_OBJECT_STORE_DIR`。启用链路探针时，为调度器配置 `MIAOQIA_KAFKA_BROKERS`、`platform.operations` 主题的读取和该角色的消费组权限。抓取器持相应的 S3 原件读写权限及列表删除权限。 |
| `acq-coverage` | 一次性运维角色。用 `mq_acq_coverage_ro` 配置 `MIAOQIA_ACQUISITION_DB_DSN`，只提供采集库只读连接所需的材料。 |
| `acq-browser` | 采集 DB 和对象存储的连接配置，以及 `MIAOQIA_ACQUISITION_BROWSER_RENDERER`。用 `MQ_DATA_PLANE_BROWSER_PROVIDER_DIR` 挂载已审查的运行绑定。按来源通过渲染器与出站验证后启用 `browser` 组。 |
| `outbox-relay` | `MIAOQIA_RELAY_DSN_TEMPLATE` 的路径包含 `{db}`，指向专用的中继账号。清单将中继限定在采集、证据、领域事实和实体库。配置 Kafka 生产权限。 |
| `data-consumer` | `MIAOQIA_EVIDENCE_DB_DSN`、`MIAOQIA_DOMAIN_INTELLIGENCE_DB_DSN`、`MIAOQIA_EVIDENCE_OBJECT_STORE_DIR`，以及 Kafka 消费和死信写入权限。 |
| `acq-admin` | `MIAOQIA_ACQUISITION_RIGHTS_READER_DSN`、明确的 `MIAOQIA_ACQUISITION_RIGHTS_LISTEN`、`MIAOQIA_IDENTITY_URL`、`MIAOQIA_IDENTITY_PEER`、内部 TLS 文件和 [权利委托](../../acquisition/source-rights.env.example)。用 `mq-source-rights grant-roles` 在迁移后授予独立的 reader 与 operator。 |
| `data-api` | 用 `mq_data_api_dom_ro`、`mq_data_api_evd_ro` 和启用组织时的 `mq_data_api_ent_ro` 配置领域、证据和实体 DSN。另配证据对象只读、`MIAOQIA_DATA_LISTEN`、`MIAOQIA_DATA_DELEGATIONS`、acquisition rights 和 identity 的内部地址和主体。详见 [data-api 装配](../../../docs/03-architecture/07-deployment-topology.md#22-data-单元)。 |
| `public-search-export` | 用 `mq_search_export_dom` 和 `mq_search_export_acq_ro` 配置领域 DSN 与来源权利 reader DSN。启用公告分块或组织时用 `mq_search_export_evd_ro`，启用组织时另用 `mq_search_export_ent`。配置 `MIAOQIA_PUBLIC_SEARCH_EXPORT_LISTEN`、专用服务证书和与检索 API 一致的发现白名单。对象权限见 [公共检索部署](../../platform/public-search.md)。 |
| `public-search-api`、`public-search-consumer`、`public-search-workflow-worker`、`public-search-secondary-worker` | 按 [公共检索环境示例](../../platform/public-search.env.example) 配置 OpenSearch 读取与写入、journal、所有者接口和 Kafka。两个 worker 使用各自的配置目录和固定 generation。常驻 worker 持 writer 证书，一次性索引构建和别名发布使用独立 publisher 证书。示例中的证书路径按角色改为 `/run/miaoqia/<文件名>`。 |

使用 S3 的进程显式配置 `AWS_ENDPOINT_URL_S3`、区域和角色凭据。采集与证据共用获准存放的原件前缀。Kafka 端点、广播地址、主题、消费组和操作按角色授权，令牌和数据库所有者连接只进入相应环境文件。TLS 与 SCRAM 的配置见[发布与消费规则](../../../docs/03-architecture/03-integration-contracts.md#34-发布与消费规则)。将公开数据主题 `public.acquisition`、`public.evidence`、`public.domain`、`public.entity` 及各消费角色的死信主题预先登记，公开数据开发主题保留期不超过七天。

## 发布顺序

在该 VPS 上为公开数据面准备独立的私有 PostgreSQL 集群。使用 PostgreSQL 容器镜像初始化时，设置 `POSTGRES_DB=postgres`。由集群管理员在空集群的 `postgres` 数据库内依次执行两份一次性引导 SQL。第一份建立 `acq`、`evd`、`dom`、`ent` 及各自的 owner、app 和专用 `outbox_relay`，第二份建立公共检索 journal 及其独立角色。通过管理员的连接服务文件选择目标集群。

```sh
PGSERVICE=mq-public-pg-admin psql -X -d postgres -v ON_ERROR_STOP=1 \
  -f deploy/vps/data-plane/public-data.postgres.sql
PGSERVICE=mq-public-pg-admin psql -X -d postgres -v ON_ERROR_STOP=1 \
  -f deploy/platform/public-search.postgres.sql
```

基础角色初始均无登录权限。由凭据所有者为需要使用的角色开启登录并配发各自凭据，向迁移任务提供 owner DSN，向业务写入进程提供对应 app DSN。中继权限由迁移登记，只可读取各公共权威库的发件箱并更新发布标记。数据库引导关闭普通角色对 `postgres`、`template1` 的默认连接权限和各应用库中 `public` schema 的默认访问权限，须在公开数据面专用集群执行。

使用支持原样环境文件的 Compose 提供者及发布账号控制的容器引擎套接字。在装配计划中先检查镜像和配置，然后逐次执行迁移。对放在 `/etc/miaoqia/data-plane/` 的发布配置，运行以下命令。

```sh
release_env=/etc/miaoqia/data-plane/release.env
manifest=deploy/vps/data-plane/compose.yaml
mq_data_plane_compose() {
  podman compose --project-name miaoqia-data-plane --env-file "$release_env" -f "$manifest" "$@"
}
mq_data_plane_compose --profile migrate config --quiet || exit 1
mq_data_plane_compose run --rm acquisition-migrate || exit 1
mq_data_plane_compose run --rm data-migrate || exit 1
mq_data_plane_compose run --rm public-search-migrate || exit 1
```

迁移后为采集权利 reader/operator、冻结 reader 和证据 reader/binder 创建独立登录角色，只授予各自目标库的 `CONNECT`。使用 `MIAOQIA_ACQUISITION_DB_OWNER_DSN` 执行 `mq-source-rights grant-roles` 与 `mq-source-rights grant-frozen-reader`。使用 `MIAOQIA_EVIDENCE_OWNER_DSN` 执行 `mq-provenance provision`，按用途授予 schema 与对象权限。

由集群管理员执行一次应用读取权限装配，再为启用的角色发放凭据。`data-api` 只获权威读取所需的表权限。搜索出口独立读取发现和证据所需的表，唯一可写的表为领域和实体的投影账本。来源、用途和主体仍由服务端请求授权约束。

```sh
PGSERVICE=mq-public-pg-admin psql -X -d postgres -v ON_ERROR_STOP=1 \
  -f deploy/vps/data-plane/read-roles.postgres.sql
```

仅在受控 evidence 运维进程启用 `mq_evidence_assess_ro`，用于读取待解析队列。预验复用 Python 镜像的 `mq-evidence-replay --assess-pending-source`，使用原件和浏览器投影的对象读取凭据。[逐源解析预验](../../../docs/03-architecture/subsystems/evidence.md#12-实现状况)

完成权限装配并复核 C11。常驻发布先启 `data-ingest` 和 `data-read`，验证中继、当前权利与 `data-api`。每次只校验当步使用的组，缺少当步角色的环境文件时在启动前停止。所有配置校验使用 `--quiet`，避免展开连接材料。

```sh
mq_data_plane_compose --profile data-ingest --profile data-read config --quiet &&
  mq_data_plane_compose --profile data-ingest --profile data-read up -d
```

按顺序启用以下角色组。每行沿用上面的 Compose 命令，替换组名执行 `config --quiet` 和 `up -d`。

| 组 | 启用时机 |
|---|---|
| `search-export` 与 `search-ingest` | 中继和 data-api 验证通过后启动投影出口与 journal 消费者，按[检索快照流程](../../platform/public-search.md)采样位点并构建 generation。 |
| `search-worker` | 配置首个 generation 的运行写入身份后启动接续 worker。发布别名使用独立的一次性 publisher 凭据。 |
| `search-secondary` | 另一个 worker 持续接续独立的 generation。为候选代次启用空闲的 worker，别名切换后保留原 worker 继续推进回退代次。 |
| `search-read` | 别名发布及当前权利读取验证通过后启动查询 API。 |
| `acquisition` | 逐源通过采集验收和运行门禁后启动 HTTP 采集与共享调度，检查默认 30 天回填、增量队列、覆盖缺口与归档状态。 |
| `browser` | 另行通过运行绑定、逐源渲染和出口验收后启动浏览器角色。 |

冻结语料测试服务使用独立只读身份和核准权限文件，在数据环境内调用 `mq-acquisition-frozen-batches`。先冻结获准的公告观察版本，再按用途读取正文、附件或浏览器投影。归档与解析分别验收。回退应用镜像按 [C11 收据](../../../contracts/reviews/c11-dev-operations.yaml) 保留兼容的数据库迁移版本和列表收据。恢复运维可为备份 S3 端点配置独立的只读身份，按[恢复手册](recovery.md)验证单件恢复、境内独立备份和空目标恢复。

每步启动后先核对该组角色就绪，再验证该组的业务读写。迁移以退出码和迁移版本核对。数据组核对 PostgreSQL、S3 读回、Kafka 中继和授权正反例。搜索消费核对 journal 位点，接续 worker 核对各自 generation，查询核对当前别名和鉴权。采集核对队列、原件和覆盖报告。任何一步失败时保留当步收据并暂停后续启动，修复依赖后重试该组。

## 开机恢复

平台运维使用公共数据面专用发布账号和该账号独立的 rootless Podman 存储，保留 Compose 项目名 `miaoqia-data-plane`。安装前核对下列项目容器的 ID、名称和角色与发布清单一致。存在意外容器时先查明其归属，再启用恢复。

```sh
podman ps --all --filter label=com.docker.compose.project=miaoqia-data-plane \
  --format '{{.ID}} {{.Names}} {{.Label "com.docker.compose.service"}}'
```

将 [开机恢复 unit](miaoqia-data-plane-restart.service) 安装到该账号的 `~/.config/systemd/user/`。由系统管理员为该账号启用 linger，再由发布账号运行 `systemctl --user daemon-reload` 和 `systemctl --user enable miaoqia-data-plane-restart.service`。使用该项目的恢复 unit 时，发布账号的通用 `podman-restart.service` 保持禁用。

unit 恢复已经创建、且 Podman 按重启策略判为应开机启动的本项目容器。一次性迁移和覆盖报告使用 `restart: "no"`。逐阶段首次启动、变更发布镜像和回退仍用上面的发布流程。存储和消息端点由各自的部署恢复。

开机启动失败时，unit 每 15 秒重试，在十分钟窗口内最多启动四次。超过次数后，值班人员从 unit 状态与日志定位故障，核对实际容器、持久挂载和依赖就绪。修复后运行 `systemctl --user reset-failed miaoqia-data-plane-restart.service`，再运行 `systemctl --user start miaoqia-data-plane-restart.service`。恢复就绪和告警送达按 C11 验收。

在维护窗口验收实际重启。核对 unit 结果和逐个角色的服务状态，验证 PostgreSQL 与对象读回、Kafka 中继及消费、采集窗口续跑、授权 data-api 与当前搜索别名。记录正常运行角色的集合和恢复时间，确认一次性任务和停用的角色未启动。回填范围和异地恢复各有独立收据。[C11 运行验收](../../../contracts/reviews/c11-dev-operations.yaml)

## 采集覆盖查验

使用发布镜像及私网内的一次性报告角色获取来源准入、当前队列、回填缺口和按日、栏目计算的采集证据。使用 `read-roles.postgres.sql` 装配的 `mq_acq_coverage_ro`。报告命令核对 `acq` 库、当前登录与有效身份以及角色成员关系。同一套覆盖查询在该身份下通过 `acq_coverage` 的只读视图访问 JSONB 证据，视图列举所需字段并移除定位 URL。其余列按查询授予，且没有其他库、源写入或发件箱权限。失败输出固定错误消息。凭据只发给负责公共采集的值班人员。对象字节由 [归档校验](recovery.md) 单独核验。

```sh
mq_data_plane_compose run --rm acq-coverage
```

报告要求配置数据库连接。核对一个指定的回填区间时，在命令中给出起止日期。区间含两端，最多 32 天。没有轮次的已声明栏目日期显示 `unknown`，启用和运行状态另列。

```sh
mq_data_plane_compose run --rm acq-coverage \
  mq-acquisition-fetch --list-sources --require-runtime \
  --since "$MQ_COVERAGE_START" --until "$MQ_COVERAGE_END"
```

## 版本留存验收

在获准的境内 S3 兼容端点，用与常驻桶一致的版本和生命周期设置准备隔离合成对象。用真实采集账本核对列表对象键和到期时间，默认留存从捕获起算 30 天。另准备长期原件对照。将提供商确认的生命周期处理时间纳入复核时刻。校验进程只读，使用获准范围的桶版本状态、精确键版本枚举、当前和指定版本读取权限。

```sh
umask 077
python /opt/miaoqia/deploy/vps/data-plane/verify_list_retention.py observe \
  --store "$MQ_RETENTION_STORE" --discovery-key "$MQ_RETENTION_LIST_KEY" \
  --control-key "$MQ_RETENTION_RAW_KEY" --expires-at "$MQ_RETENTION_EXPIRY" \
  --verify-after "$MQ_RETENTION_VERIFY_AFTER" > retention-before.json || exit 1
sha256sum retention-before.json
```

在到期前运行 `observe`，按列表键的 SHA-256 及实际版本读回确认原件，记录长期对照版本及字节。将含版本 ID 的前置收据保存在受限目录，并在独立受信记录中固定其摘要。到约定时刻运行后置核验。

```sh
python /opt/miaoqia/deploy/vps/data-plane/verify_list_retention.py verify \
  --store "$MQ_RETENTION_STORE" --baseline retention-before.json \
  --expected-sha256 "$MQ_RETENTION_BASELINE_SHA256"
```

后置核验绑定原端点、桶前缀和版本设置。所有精确列表键版本及删除标记须消失，普通读取和预先登记的版本读回均须返回明确的缺失信号，长期对照仍须匹配。权限拒绝、枚举失败和未到复核时刻均返回失败。记录前后收据及确切策略。合成缩时演练可验证命令判定，实际生命周期验收按账本的 30 天到期时间记录。[C11 留存验收](../../../contracts/reviews/c11-dev-operations.yaml)
