# 公共检索部署

| 角色 | 接线 |
| --- | --- |
| `public-search-api` | 通过 `POST /v1/search` 提供持令牌的交互查询。复用网关 OIDC 和远程 identity 当前会话核验。使用公共 OpenSearch 只读凭证和 data-api mTLS 调用凭证。 |
| `public-search-consumer` | 将公共主题上已校验的事件写入公共 PostgreSQL journal。启动只需公共 PostgreSQL 与 Kafka。 |
| `public-search-workflow-worker` | 按固定 generation 从公共 journal 接续，在专用 mTLS owner export 上解析事件，并用公共 OpenSearch writer 写入。每个需要接续的 generation 由部署明确启动 worker。 |

公共 PostgreSQL 的库和登录角色固定为 `sea_public`、`sea_public_owner` 和 `sea_public_app`。在境内基础设施运行 [初始化 SQL](public-search.postgres.sql)，由凭据所有者启用登录并配发密钥。迁移命令为 `platform -role public-search-consumer migrate`，使用公共 owner DSN。为 Cell 的 `sea` 库保留独立的角色与连接授权。运行环境见 [配置示例](public-search.env.example)。

境内开发 VPS 通过 Compose 的 `search-export` 和 `search-ingest` 先启动投影出口与 journal 消费。建立首个 generation 后用 `search-worker` 启动接续。查询别名发布后用 `search-read` 启动 API。升级时，在 `search-worker` 和 `search-secondary` 的空闲服务配置候选 generation，使用独立配置及相同 journal。两个 worker 的 generation 必须不同。切换前核对两个代次各自的接续位点，切换后原 worker 继续推进回退代次。在保留窗口结束并停止其 worker 后，才能把该位置用于下一候选。[公共数据面发布顺序](../vps/data-plane/README.md#发布顺序)

独立公共 OpenSearch 使用 Security 插件和专用运行客户端证书。当前别名的查询使用 `GET /<alias>/_alias/<alias>`，资源范围限定为该别名及它指向的代次。reader 读取当前别名、创建 PIT 并查询。常驻 writer 只写投影及撤回标记、读回冲突摘要和刷新。一次性构建与发布使用独立 publisher 身份，另有索引建立、读取与切换别名的权限。OpenSearch 可允许有别名权限的调用方为其获准索引挂上其他名称。此装配用于一个命名空间独占的公共集群，publisher 凭据只进入受控发布任务。索引删除、权限管理和节点配置由平台运维的独立管理身份执行。

为 REST 端口启用 HTTPS 与强制客户端证书，接入受控签发的证书认证域。认证域取证书 CN 作为用户名，reader、writer、publisher 的 CN 分别为 `mq-public-<namespace>-reader`、`mq-public-<namespace>-writer` 和 `mq-public-<namespace>-publisher`。将发行和续期限制给各角色所有者。用独立 Security 管理证书安装对应角色映射，服务进程使用相应的运行或发布证书。部署时在实际 OpenSearch 核对代次切换、投影写入、PIT 查询、错误证书与越权写入。另核对常驻 writer 无法建立代次、切换别名或修改管理配置。

```sh
python3 deploy/platform/provision_public_opensearch.py \
  --origin "$MQ_SEARCH_ADMIN_ORIGIN" --namespace "$MIAOQIA_PUBLIC_SEARCH_NAMESPACE" \
  --ca "$MQ_SEARCH_ADMIN_CA" --cert "$MQ_SEARCH_ADMIN_CERT" --key "$MQ_SEARCH_ADMIN_KEY"
```

装配命令覆盖该命名空间的三条角色定义和三条映射。失败时保留部署隔离并修复后重跑全量装配，通过权限验收后发放运行凭据。写入接口内部使用 OpenSearch 的 bulk 协调动作，实际写入项仍按所属代次检查索引权限。

入口接受 `application/json`。请求需要 `query` 对象和 1–50 的 `page_size`，可传 `cursor`。查询键为 `text`、`identifier`、`kind`、`domain`、`region`、`notice_type`、`from`（含）和 `before`（不含）。返回 `hits`、`indexed_as_of`，有经核验的后续候选时带 `next_cursor`。每页最多检查 256 个索引候选，空游标只说明该窗口内无可确认的后续候选。部署边缘路由须转发经过会话处理的用户 Bearer 令牌。[公共检索 OpenAPI](../../contracts/apis/search/openapi.yaml)

查询适配器通过 data-api `GetFact` 对 `INF-PROC` 公告和采购项目逐项核验固定版本、当前版本、来源标签和查询条件，返回固定引用。启用分块后，`GetNoticeChunk` 核验当前公告版本和全文展示权利，从哈希已验证的规范化文档返回匹配的文字、快照与定位。公告和采购项目按标题复核文本，分块按原文片段复核。所有来源标签仍须满足 `MIAOQIA_PUBLIC_SEARCH_DISCOVERY_SOURCES`。data-api 对 API 证书精确委托 `data.get_fact`，分块另需 `data.get_notice_chunk`，两者都绑定 `interactive` 和用户主体。identity 的内部 peer 白名单也须接纳该角色证书。data-api 的 403 会拒绝整页。依赖不可用或无法核验时返回 503。

企业检索通过 `GetPublicOrganization` 复查 `INF-ORG` 已确认组织的当前固定版本。可按组织 ID、统一社会信用代码和登记名称检索，命中返回固定引用与权威登记名。data-api 需配置实体数据库和 `MIAOQIA_PUBLIC_REGISTRY_SOURCES`，并额外委托 `data.get_public_organization`。证据闭包仍由当前身份与来源权利核验。

增量出口为 `POST /internal/v1/search-projection/mutations`。服务端证书的 SPIFFE URI 为 `spiffe://miaoqia/service/public-search-export`，授权的调用者为 `spiffe://miaoqia/service/platform-public-search-workflow-worker`。首个请求为原始公共 CloudEvent。来源权利事件返回 `next_after` 时，后续页以 `{"event": <同一事件>, "after": <前页键>}` 续读。单页最多 32 项。每页写入并刷新后持久化续页键，事件完整应用后推进 journal 位点。所有者按服务证书授权本操作。成功返回 `{"mutations":[...]}`，每项包括 `ref`（`kind`、`domain`、`id`、十进制字符串 `version`）、正整数单调 `revision` 和布尔 `withdrawn`。已获准公开匹配的记录可包含 `identifiers`、`region`、`notice_type`、`published_at`、`title`。撤回项仅包含引用、顺序及撤回状态。空数组表示 owner 已确认该事件无需更新。拒绝和不可用使用非 200。worker 在失败时保留待处理位点或续页键。

数据所有者在 `data` 工作区以 `uv run --locked --all-packages python -m miaoqia.runtime_data.search_projection serve` 启动专用监听。配置显式监听地址 `MIAOQIA_PUBLIC_SEARCH_EXPORT_LISTEN`、专用证书与 CA、领域数据库凭据、来源权利 reader DSN 和发现白名单。先迁移并按[公开数据面发布顺序](../vps/data-plane/README.md#发布顺序)装配专用角色。搜索出口的来源权利 reader 访问来源目录、当前来源撤权和权利版本。启用 `MIAOQIA_PUBLIC_SEARCH_NOTICE_CHUNKS=1` 还需证据数据库与对象存储的只读凭据。每条公告最多投影 12 段，每段最多 768 个码点。源级撤权和新版分块数量减少都按固定槽位下发墓碑。

启用 `MIAOQIA_PUBLIC_SEARCH_ORGANIZATIONS=1` 还需实体迁移 0002、实体投影账本写权限、登记来源白名单和证据读取凭据。可供发现的登记来源同时属于登记与发现白名单。用 `uv run --locked --all-packages python -m miaoqia.runtime_data.organization_registry <ogr_id> <version>` 从有明确登记证据的固定事实确认实体。确认写入与实体事件同事务，来源撤权流程依次清理采购和实体投影。两个所有者的续页键均由 worker 保存，并在同一事件内用原值续读。

生产启用需要所有者提供获准公开发现的字段与撤回语义、可复现的快照与 Kafka 各分区起始位点、实际成功处理的重放水位和相关性比较结果。以下运维命令使用公共 worker 的独立凭据，从 stdin 接收经审核的 JSON 材料。公共查询与 owner export 使用同一个发现来源白名单，查询范围可进一步缩小。缩小静态白名单时先更新查询角色。立即撤权走 acquisition 当前权利门，由 data-api 在权威读取时核对。撤权后仍须由所有者提供墓碑或受控重建以清理索引。查询、消费、投影应用以及首次快照发布应分别验证。

## 构建与回退

以 publisher 证书调用 `platform -role public-search-workflow-worker search-build <command>`。配置项沿用 worker 的 OpenSearch 证书变量。由数据所有者冻结并分批校验快照清单，由搜索运维提供已核实的 Kafka 水位与相关性比较结果。

先运行 `sample-replay-start` 记录同一 Kafka 的四个公共主题全部分区下一位点，再执行 `uv run --locked --all-packages python -m miaoqia.runtime_data.search_snapshot` 捕获采购主库快照。启用分块时每页一个事实句柄。启用组织时，同一命令也捕获实体主库固定版本，取两库快照时间的较早者。清单中的 `snapshot_id` 与 `as_of` 同 Kafka 起点用于 `begin`，`pages` 的每个元素交给 `snapshot-page` 生成可装载的投影。若捕获版本在确认该页前被更替，该页会拒绝，整个构建使用新起点与新清单重试。页确认后的更替以事件追赶。捕获命令的 `--help` 包含逐步输入示例。

| 命令 | 输入 |
| --- | --- |
| `sample-replay-start` | 连接 `MIAOQIA_KAFKA_BROKERS`，输出公共主题下一位点。与采样时间及集群和主题身份一起保存。 |
| `begin` | stdin 提供 `snapshot_id`、`as_of`、`replay_start`，输出 generation。 |
| `load <generation> <snapshot_id>` | stdin 提供一页 owner DTO。每页可复用 `python -m miaoqia.runtime_data.search_projection snapshot-page` 由固定事实句柄产生。 |
| `seal <generation> <snapshot_id>` | stdin 提供最后一页。该页写入后封存快照。 |
| `status <generation>` | 查看快照与该 generation 已成功应用的 journal 序号。 |
| `current` | 查询读别名实际指向的 generation 和 activation。首次发布前返回 null。切换响应丢失时用它核实。 |
| `replay-status <generation>` | stdin 提供各公共主题及分区的目标下一位点。返回已检查范围、可证明的连续进度与首个缺口。 |
| `publish <generation> <expected-activation>` | stdin 提供 `generation`、`snapshot_id`、`applied_sequence`、`barrier`、`compared`。命令从 `MIAOQIA_KAFKA_BROKERS` 取得分区末位点。首发的 expected-activation 为空字符串，回退使用当前 activation。 |

所有需要接续增量的 generation 都以独立 worker 应用 journal。切换后继续推进保留的回退 generation。成功处理的 Kafka 投递和 journal 在同一 PostgreSQL 事务登记，按明确忽略的已验证事件登记可解释的空操作。死信、未识别事件、未应用投影或缺少投递回执均阻止切换。发布检查覆盖采样时的 Kafka 尾部及已入库的后续投递，之后到达的消息由增量 worker 接续。部署须使用同一组公共主题，并允许发布命令查询位点。全量快照清单、快照与 Kafka 起点的一致性以及相关性验证仍由负责构建的流程提供。

升级公共索引库到 00002 后，既有 journal 可继续服务当前别名。若要发布新代次或回退，先用 `replay-status` 核对缺失的投递回执。在 Kafka 仍保留相应日志时，由值班人员暂停对应的 `search.projection.<topic>` 消费组，按分区从需要的位点重读。inbox 幂等去重会保留原有 journal 项，同一交易事件的投递回执由重读补齐。若起点早于保留范围，使用经审核且起点可读的新快照。主题重建或切换 Kafka 集群时使用新的公共 journal 及快照，以免复用同名分区的旧位点证据。
