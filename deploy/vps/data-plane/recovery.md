# 开发公共数据面异地恢复

现有境内开发 VPS 是常驻源端。恢复演练使用另一台获准的境内 VPS 与独立备份存储。平台工程负责人登记源端、目标、发布版本、停写预算和独立备份位置，数据负责人确认来源范围、对象留存、授权与导出用途。验收结果写入 [C11 记录](../../../contracts/reviews/c11-dev-operations.yaml)。

## 1. 建立备份恢复点

1. 停止新采集和浏览器任务，等待在途写入结算。暂停其余写入角色、中继、来源权利变更、数据库迁移、列表清理和影响该范围的存储生命周期。记录截止时间、未完成采集计划、Kafka 集群与主题标识、可重放区间和各消费组已提交位点。记录截止之后发生的撤权，以便恢复后核对当前权利。
2. 在受限目录导出下表五库。为 `mq-backup-<库名>` 配置获准的备份连接服务，使用独立凭据和支持目标版本的 PostgreSQL 客户端。`mq-backup-admin` 只供受信运维导出角色定义。用 `psql` 另存库级 ACL、存储空间、编码、排序规则、扩展、数据库设置和迁移版本。保存可比较的业务行、序列和待发布事件摘要。

   ```sh
   umask 077
   mkdir -m 700 db
   for database in acq evd dom ent sea_public; do
     PGSERVICE="mq-backup-$database" pg_dump -w --format=custom \
       --serializable-deferrable --file="db/$database.dump" || exit 1
   done
   PGSERVICE=mq-backup-admin pg_dumpall -w --roles-only \
     --no-role-passwords --file=roles.review.sql
   ```

   `roles.review.sql` 是角色审阅输入。生成目标角色清单时保留五库的所有者、运行角色、角色关系及直接授权所引用的主体，按目标映射审核权限。目标数据库保留原有对象 ownership 和 ACL。服务凭据、私钥及授权配置由部署侧的受控系统重新交付。

3. 用对象提供商的分页枚举和读接口导出批准的桶前缀。逐对象保存逻辑存储名、键、版本（适用时）、字节数、SHA-256 与原始字节。冻结数据库引用与对象清单双向对账，过期和孤立对象按保留规则登记。缺少仍被引用的对象或对应投影时拒绝发布备份。若批准保留对象历史版本，同时核对历史版本与删除标记，并记录目标版本映射。保存存储的前缀策略、读写和删除权限、版本保护及保留配置。

   | 权威库 | 对象与检查点 |
   |---|---|
   | `acq` | 正文、附件、分组原件、浏览器原件与投影的绑定、留存期内的发现原件、来源权利、观察和任务账本 |
   | `evd` | 规范化制品、被引用的派生及参考制品、修订及证据闭包 |
   | `dom`、`ent` | 当前与历史的事实和实体版本、投影账本、事务发件箱 |
   | `sea_public` | 检索 journal、投递收据、代次和当时的消费水位 |

   在冻结期间用发布镜像中的 `verify_archive.py` 从 `acq` 与 `evd` 的只读事务检查权威引用。采集侧检查全部已登记的原件（包括附件、分组及尚未关联的原件）、浏览器原件和投影，以及截止时间仍在留存期内的列表原件。收据另列已到期和未登记对象键的列表数。证据侧检查快照正文和规范化文档、附件、分组原件、浏览器原件和投影、待解析原件和派生及参考对象。待取得的附件沿用其账本状态。每项校验内容哈希和已登记的长度，规范化文档另核对快照和管线；收据以字节 SHA-256 计算逐类摘要，供读取源端、备份副本和恢复端时比较。使用相同的带时区截止时间，保持数据库、列表和存储写入冻结。

   在受限的一次性运维进程运行下列命令。用 `PGSERVICEFILE` 单独为校验进程提供备份或恢复端的只读连接服务。用 `MQ_RECOVERY_OBJECT_STORE` 指向归档 S3 根或私有导出目录。对象凭据只授予批准前缀的读取。将对象位置切换为导出的当前对象树重跑，将两次成功收据用 `cmp` 核对。保留退出码和收据。工具只打印分类、计数、摘要及脱敏错误，单个对象或总运行时间超过预算时失败。提供商枚举仍须对账过期对象、孤立对象、历史版本及删除标记。

   ```sh
   python /opt/miaoqia/deploy/vps/data-plane/verify_archive.py \
     --phase backup --cutoff "$MQ_RECOVERY_CUTOFF" >archive-source.json || exit 1
   ```

4. 在受限目录整理备份包。包根只包含以下文件和目录。发布方按冻结点审核库元数据、Kafka 检查点、对象范围和保留例外，并对五份转储逐一运行 `pg_restore --list`。

   | 路径 | 内容 |
   |---|---|
   | `db/{acq,evd,dom,ent,sea_public}.dump` | 五份 PostgreSQL custom 转储 |
   | `db-metadata.json`、`roles.review.sql` | 库级元数据、摘要和角色审阅输入 |
   | `kafka-checkpoint.json` | 集群、主题、分区、可重放区间和已提交位点 |
   | `objects/` | 当前对象树，根目录保留原有逻辑键布局 |
   | `object-inventory.json` | 提供商枚举、版本映射、删除标记、保留与权限记录 |
   | `versions/` | 按清册登记的历史对象字节。无获准历史版本时保留空目录。 |
   | `archive-source.json`、`archive-copy.json` | 同一截止时间的成功收据，字节完全一致 |

   在文件冻结、只由受信运维读取的目录运行 `backup_manifest.py`。它绑定规范 UTC 截止时间、发布 SHA-256、批准记录 ID、所有目录与各文件的相对路径、长度及 SHA-256。工具要求标准文件布局，拒绝符号链接、硬链接和特殊文件。审批记录及对象清册的语义审阅、冻结数据库与对象的引用对账分别由受信运维和 `verify_archive.py` 完成。

   ```sh
   umask 077
   python /opt/miaoqia/deploy/vps/data-plane/backup_manifest.py create \
     --root "$MQ_RECOVERY_BUNDLE" --cutoff "$MQ_RECOVERY_CUTOFF" \
     --release-sha256 "$MQ_RECOVERY_RELEASE_SHA256" --approval "$MQ_RECOVERY_APPROVAL" \
     >"$MQ_RECOVERY_MANIFEST" || exit 1
   sha256sum "$MQ_RECOVERY_MANIFEST"
   ```

   将清单放在包根之外，并在独立受信记录中固定其 SHA-256。加密传送备份及清单至独立的获准位置，从该位置全量读回并运行下节的校验，再发布完成标记。源端重新开始写入前记录本次停写耗时和需要回补的检查点。

## 2. 在空目标恢复

1. 在恢复 VPS 验证集群、数据库、目标桶、Kafka 位点和索引别名的空状态。确认新目标的身份及地域。保持业务角色、通知和采集出口关闭。将读回的备份解密至受限目录，以独立受信记录提供 `MQ_RECOVERY_MANIFEST_SHA256`，执行下列命令。复核输出的截止时间、发布摘要和批准记录。通过后保持目录冻结，并以包根为工作目录执行恢复。

   ```sh
   python /opt/miaoqia/deploy/vps/data-plane/backup_manifest.py verify \
     --root "$MQ_RECOVERY_BUNDLE" --manifest "$MQ_RECOVERY_MANIFEST" \
     --expected-sha256 "$MQ_RECOVERY_MANIFEST_SHA256" || exit 1
   cd "$MQ_RECOVERY_BUNDLE" || exit 1
   ```
2. 按审核后的角色与库元数据清单在目标新建角色和空库。为 `mq-restore-<库名>` 配置目标恢复连接，执行下列命令。保留数据库对象与角色的对应关系和 ACL，恢复结束后核对迁移版本、固定行/序列摘要、发件箱和库级权限。

   ```sh
   for database in acq evd dom ent sea_public; do
     PGSERVICE="mq-restore-$database" pg_restore -w --exit-on-error \
       --single-transaction --dbname="$database" "db/$database.dump" || exit 1
   done
   ```

3. 按原键条件写入对象，逐条读取复核长度与 SHA-256。断开备份介质并重启 PostgreSQL 和 S3 后，以恢复端对象根运行同一命令，使用 `--phase restore` 选择恢复连接服务并沿用恢复点截止时间。要求命令成功且收据与源端完全一致。使用 [冻结批次入口](../../../docs/03-architecture/subsystems/acquisition.md#136-下游契约)抽验原件、附件及浏览器投影，使用真实运行角色验证权威读取与越权拒绝。任何失败都保留目标隔离，记录失败范围，再从空目标重跑。
   如需从独立 S3 端点恢复单件原件或投影，将 `MIAOQIA_ACQUISITION_FROZEN_BACKUP` 指向备份桶及前缀，并通过 `MIAOQIA_ACQUISITION_FROZEN_BACKUP_S3_PROFILE` 选择备份专用 AWS profile。运维在 `AWS_CONFIG_FILE` 的对应 profile 中设置 `endpoint_url` 和 `region`，在受保护的 `AWS_SHARED_CREDENTIALS_FILE` 的同名区段提供只读访问密钥及可选会话令牌。目标归档使用该进程平时配置的 S3 端点和条件写身份。省略备份 profile 时，可从本机独立副本或当前 S3 连接下的备份桶读取。

4. 用独立的当前权利记录对账恢复点之后的撤权和权限变化。缺少证据的来源保持封闭。恢复原 Kafka 时先查验原分区和位点仍可读取，再执行保有幂等键的重放。更换 Kafka 时使用新的事件检查点与检索 journal，单独验证权威快照和历史补发范围。已标记发布的历史事件需要纳入明确的重放计划。按 [检索构建流程](../../platform/public-search.md#构建与回退)重建并验收派生索引。最后核对恢复后的积压和未完成计划，采集暂存缺失时按历史窗口补采，保留受阻来源的覆盖缺口。

验收记录同时包含停写和恢复时长、目标角色读写测试、外部备份读回证明、对象和数据库的对账结果、位点与索引恢复情况及未覆盖项。同机隔离和本地合成演练记录各自适用的范围。[恢复与升级门禁](../../../docs/03-architecture/07-deployment-topology.md#12-运行恢复与升级验收立项清单-c11)
