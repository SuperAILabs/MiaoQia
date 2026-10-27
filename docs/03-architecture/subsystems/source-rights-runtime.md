# 冻结语料的当前来源权利与可信来源闭包

acquisition 提供冻结语料的持久化当前权利端口，供后续 data-api 注入；evidence/domain 的本地端口提供固定版本可信来源闭包。它没有接通事实/证据查询 listener、Go 生产适配器或完整 R2；A02 保持 L0，E1–E5 仍为建议。登记和激活读取许可都不启用来源采集，不证明领域 live。采集、模型使用、治理下线及真实来源审批不属于此只读契约。

## 权威与引用

[SourceRightsPolicy](../../../contracts/schemas/acquisition/source-rights.schema.json) 1.0.0 定义不可变原始版本：entry_id/version、source_id、精确 tenant/cell scopes、interactive purpose、展示上限、有效期及再分发限制。空 scope 不表示公共通配；业务读取仅支持 fact_read / evidence_resolve。原始版本及当前版本的每项限制同时生效；多个输入取交集、最低展示等级、最早到期和再分发 AND。扩权不能追溯放宽已有原始引用。

数据库 `acq_rights` 保存不可变版本、精确 source+完整 tags 组合到原版本的不可变别名、当前版本/撤销指针、持久 epoch 与追加审计。操作通过受限 SECURITY DEFINER 函数串行执行，指针、epoch、审计同事务；别名不可改绑。运行 reader 仅 SELECT，operator 仅 SELECT 和执行受限 command，owner 只迁移/角色配置；禁止把 owner/operator 凭据放进运行进程。

允许登记的原始组合由 owner 迁移固定为下表；acquisition 的清单决定来源/tag 组合资格，参考内容哈希由 evidence 的固定清单独立核验：

| source | 完整原始 tags | 原 entry |
|---|---|---|
| official-qzygjy | public-official-review + corpus:official-building-tender-20261026 | building-tender@1 |
| official-qzygjy | public-official-review + corpus:official-building-award-20261026 | building-award@1 |
| reference-proc-building-topic-v0-1 | review:building-topic-pair | building-topic-reference@1 |

`public-official-review` 单独不足授权，其他 source、未知或多余 tag、前缀近似、空输入、撤销或缺当前指针都拒绝。旧事实、事件、快照和 tags 原样保留。调用方必须从已验证的持久化来源/固定依赖取得每个原始输入的 source 和完整 tags；不得从用户传参宣称来源，也不得把聚合 tags 并集猜成单个授权条目。受认证 data peer 对这种来源绑定负责；d0 不读取 data 私库核验任意来源断言，本地可信装配通过下述 owner 元数据端口提供；远程 listener 与响应提交集成仍待独立实现。

## 固定版本可信来源闭包

`evidence.api.Closure` 保存完整 source_id/tags 分组、固定参考 name/version/content_hash/原始 tags，以及固定输入依赖及哈希；只去重完全相同的组，同一 source 的不同 tags 组同时保留。它不保存许可或“已绑定”状态，也不从聚合 tags、命名空间或字段当前仍可见的 prov 猜来源。evidence 新派生/增强与发件箱同事务追加 `evd_requirement_sets`；domain 新事实版本追加 `dom_version_requirements`，增强合并父版本完整闭包，pp 在固定成员依赖写完后合并全部成员闭包。元数据失败时同事务回滚。公开事实、原对象、参考哈希、receipt 与历史版本不改写。

`domain_intelligence.queries(...).requirements(id, version)` 返回已核验固定事实哈希的 owner 元数据。部署单元将其显式注入 `evidence.enrichments(..., fact_requirements=queries.requirements)`，连同原 `facts` 内容读取回调使用。派生闭包必须继承该完整父闭包，即使旧参考对应字段已被替换/删除也不能丢失限制；扫描当前 JSON 仅补充文档/DAG 证据。`evidence.source_requirements(database)` 的 snapshot/revision/evidence/reference/recorded 端口只读元数据，resolve 每次核验准确参考句柄并解析当前唯一不可变绑定，不读对象、不缓存正向结果。

旧位置参数 `allowed(tags,purpose)`/`display(tags,purpose)` 与本地测试路径保持兼容。新的 keyword-only `authorize(groups,purpose)` 与旧模式同时装配会在构造时报错，均未装配时拒绝；structured 模式绝不回退旧 callback。Facts 的 authorize 必须返回严格 bool；evidence 返回已知展示级别。domain structured 查询还需显式注入 `provenance=evidence.source_requirements(evd_database)`。来源组、参考或旧版本证明缺失为 Unbound/拒绝；完整性冲突报错。用于应用的结果须对全部闭包满足 full_text，摘录/完整读取还重建 DAG 闭包核对 sidecar；none/citation_only 在对象读取前结束。

本地 provenance 保留原来合法的 source 字符串、全部 tags 与分组，不强加 d0 的 source 格式、16 tags/64 groups wire 限制。仅 resolve 进入 structured 授权时检查可表示性，超限则拒绝，不裁剪、不合并不同组、不改 SDK 原有64层/1024节点 DAG 边界。

参考绑定是一次不可变、带 actor/reason/session_user/时间的审批与绑定，不是来源读取许可。[固定清单](../../../data/subsystems/evidence/src/miaoqia/evidence/resources/reference_manifest.json) 只有 proc-building-topic@0.1.0，内容 hash `sha256:2b5aef8fb787639360a27918f62ee56c4fb1c573f0c564ed288e1660fd13e258`、source `reference-proc-building-topic-v0-1`、完整 tags `[review:building-topic-pair]`；manifest_hash 是清单自身的规范化 SHA-256。新内容必须经新版本、新清单迁移复核。绑定前写入的完整闭包可在后来合法绑定后参与当前 d0 判断，绑定不表示历史已有许可，也不能修复错误 hash、tags 或缺失依赖。测试 identity 参考始终没有批准绑定，旧本地测试可用，structured 拒绝。

owner 迁移 evidence0003/domain0004/acquisition0002，不激活任何政策。acquisition0002 在同事务内锁定 reviewed、仅追加一个参考组合并恢复同一不可变触发器，保留0001及原两行。evidence 固定 search_path、全限定 SECURITY DEFINER 函数只接收 name/version/审计信息，不能由调用方指定来源、路径或 hash；普通参考登记不具备绑定能力。专用 reader 仅 SELECT，binder 仅 EXECUTE；owner 配置会清除直接/默认 DML、EXECUTE、schema CREATE 授权，拒绝超权或继承角色。清单、绑定和闭包均保护 UPDATE/DELETE/TRUNCATE，清单也禁止运行时 INSERT。

## 离线绑定与显式回填

使用[离线环境示例](../../../deploy/data/provenance.env.example)，不要把 owner/binder DSN 放入 reader 运行配置。先创建独立无继承、无超权的现有登录角色，owner 执行 `mq-provenance provision --reader <role> --binder <role>`；此命令不创建账号。普通登记器仍通过原 register_reference 写不可变参考内容，binder 不拥有参考登记或 acquisition 写权限。

以独立 `MIAOQIA_EVIDENCE_BINDER_DSN` 执行 `mq-provenance bind --batch deploy/data/reference-bindings-reviewed.json --actor <id> --reason <reason>`。[绑定批次](../../../deploy/data/reference-bindings-reviewed.json) 只含 name/version，最多100项，整批原子、相同证明重试保留原审计。另由 acquisition operator 明确执行 [参考 register/bind 批次](../../../deploy/acquisition/source-rights-building-reference.json)；它与官方pair批次分开，均不 activate，也不新增默认 scope。读取仍需独立显式激活和 d0 当前许可，参考不得借用 official/corpus 原许可。

历史记录无完整证明时不写占位 sidecar、不在请求中自动回填。显式 owner 命令为 `mq-provenance backfill-domain`/`backfill-evidence --batch <json> --actor <id> --reason <reason>`，前者 JSON 为 `[[fact_id,version],...]`，后者为 `[run_key,...]`，最多100个明确目标。均需配置两个 owner DSN、原对象目录；涉及增强时需明确 recipe 开关（`--enable-test-recipe` 或 `--enable-building-topic`）；不会登记或绑定新参考。按依赖顺序先验证 base domain 版本，再 evidence 运行，再增强 domain/pp。每个 owner 批次重新核验原修订/对象/固定依赖、哈希、receipt 与完整 DAG，成功才追加审计；缺输入/冲突使整个批次回滚，相同证明仅幂等复用；不宣称跨数据库原子性。不同证明不可覆盖旧行，无证明的旧 local callback 保持可用，后续本地派生也不能把未知父证明认证为完整。

## 内部协议与确认边界

acq-admin 内部 listener 使用标准库 ssl/http.server/http.client：TLS1.3、受信 CA、客户端证书、唯一 SPIFFE URI SAN、精确配置 peer。先校验传输 peer，才接受封闭的 Miaoqia-Context；无开发绕过、转发身份或静态 allow。data 不持 acquisition DSN，只导入 `acquisition.api.remote.RightsClient`。identity 继续使用现有平台内部 session 接口（HTTPS、禁止重定向），每次读取/确认/状态都核验当前 user、tenant、cell 和 interactive；service/service_account 首版拒绝。缺配置、依赖或策略失败关闭。

| POST 路径 | 封闭请求 | 响应 |
|---|---|---|
| /internal/v1/source-rights/resolve | operation，inputs（1–64 个 source_id/tags） | allowed/display/redistribution/epoch/receipt |
| /internal/v1/source-rights/confirm | 同样 operation/inputs，加原 receipt | 重新计算的同形决策；不含 receipt 字段 |
| /internal/v1/source-rights/status | {} | domain_service=not_live、capability=frozen_source_rights_only |

receipt 是最长30秒的随机 nonce/epoch/到期/HMAC 完整性封套，绑定原 user/tenant/cell/purpose、operation、全部 source/tags。它不能单独授权：confirm 必须重新读取当前 identity、全部当前权利和权威数据库时间；epoch 变化、同 epoch 到期、主体/引用/用途变化均不通过。进程重启使旧 receipt 失效；没有跨请求正向许可缓存。receipt 可在原范围内重复确认，但每次都是新的当前授权计算，不能当作已授权 boolean 保存或转交给其他主体。

每次判断在 acquisition 本地共享事务锁下读取当前状态，与 operator 撤销的独占锁排序；成功 confirm 的最终检查是授权线性化点。data 必须在响应发送前确认，不能跨单元持有数据库事务。最终远程确认后到数据发出仍有不可消除的间隔，已发出的披露不能撤回；d0 不宣称补齐 d1/d2 的响应提交集成。

明确范围/purpose/当前成员拒绝为403；无效 DTO 为400；认证握手失败没有业务响应；缺依赖/未装配/超时为503。通过请求级 admission 后，未知与无权组合均返回同形 allowed=false，不枚举 registry。无正文、SQL、下载 URL 或写 HTTP 接口。TLS 握手受独立 socket 超时及32个工作线程上限约束；验证对端后，HTTP 总期限从读取请求行和请求头前开始，覆盖正文、身份解析与响应。头/体/深度/并发/期限有界，错误不返回路径、DSN、证书或异常堆栈。status 仍执行相同 admission；数据库连通不等于领域 live。

## 操作与部署

1. 创建独立、无超权/角色继承的 reader 与 operator 登录角色，由 owner 执行 `mq-acquisition migrate --role acq-admin`。
2. 配置 `MIAOQIA_ACQUISITION_DB_OWNER_DSN`，本地执行 `mq-source-rights grant-roles --reader <read-role> --operator <operator-role>`；运行配置删去 owner/operator DSN。
3. 使用 operator DSN（`MIAOQIA_ACQUISITION_RIGHTS_OPERATOR_DSN`）执行 `mq-source-rights apply --file deploy/acquisition/source-rights-frozen.json --actor <operator-id> --reason <review-reason>`。整批至多16条、64KiB，事务失败全部回滚。重试相同操作不加 epoch/审计。
4. [初始清单](../../../deploy/acquisition/source-rights-frozen.json) 仅登记/绑定，不 activate；scope 仅 tnt_01JA8ZXXXXXXXXXXXXXXXXXXX1/cell-01，展示 excerpt、不可再分发，2027-01-01 到期。真实 identity 未有该成员时自然拒绝；修改 scope 必须显式新版本评审，不能扩大旧原始引用。
5. 需要 fixture 读取时，另写显式 activate 命令数组（action/entry_id/version）；撤销数组为 action=revoke + entry_id。版本登记为 action=register + policy，别名绑定为 action=bind + source_id/tags/entry_id/version。必须提供 actor/reason，不接受 SQL、通配目标或网络管理请求。
6. 参考[关闭的配置示例](../../../deploy/acquisition/source-rights.env.example)，发放实际证书，配置精确 data peer→rights.resolve/confirm/status→interactive；设置 reader DSN、identity HTTPS/peer 和监听地址后才显式启用 `MIAOQIA_ACQUISITION_RIGHTS_ENABLED=1`，以 `mq-acquisition --role acq-admin` 启动。8092 是可配置推荐值，并非已有生产端口。

现有 identity peer 使用 `spiffe://miaoqia/service/platform-api`；data-api/acq-admin 的 URI 随角色证书显式发放，示例不是生产默认许可。迁移仅 acquisition 自库，不借用 data/evidence/domain 凭据。停止依赖、失联、过期和撤销时失败关闭；不使用缺撤销保障的本地权利快照。后续 data listener、最终响应确认和真实状态装配须独立实现/评审。
