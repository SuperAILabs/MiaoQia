# 冻结语料的当前来源权利（d0）

acquisition 提供冻结语料的持久化当前权利端口，供后续 data-api 注入。它没有接通事实/证据查询 listener、Go 生产适配器或完整 R2；A02 保持 L0，E1–E5 仍为建议。登记和激活读取许可都不启用来源采集，不证明领域 live。采集、模型使用、治理下线及真实来源审批不属于此只读契约。

## 权威与引用

[SourceRightsPolicy](../../../contracts/schemas/acquisition/source-rights.schema.json) 1.0.0 定义不可变原始版本：entry_id/version、source_id、精确 tenant/cell scopes、interactive purpose、展示上限、有效期及再分发限制。空 scope 不表示公共通配；业务读取仅支持 fact_read / evidence_resolve。原始版本及当前版本的每项限制同时生效；多个输入取交集、最低展示等级、最早到期和再分发 AND。扩权不能追溯放宽已有原始引用。

数据库 `acq_rights` 保存不可变版本、精确 source+完整 tags 组合到原版本的不可变别名、当前版本/撤销指针、持久 epoch 与追加审计。操作通过受限 SECURITY DEFINER 函数串行执行，指针、epoch、审计同事务；别名不可改绑。运行 reader 仅 SELECT，operator 仅 SELECT 和执行受限 command，owner 只迁移/角色配置；禁止把 owner/operator 凭据放进运行进程。

初始允许登记的组合只有 source `official-qzygjy` 与下列之一：

| 原始 tags（两项必须同时存在） | 原 entry |
|---|---|
| public-official-review + corpus:official-building-tender-20261026 | building-tender@1 |
| public-official-review + corpus:official-building-award-20261026 | building-award@1 |

`public-official-review` 单独不足授权，其他 source、未知或多余 tag、前缀近似、空输入、撤销或缺当前指针都拒绝。旧事实、事件、快照和 tags 原样保留。调用方必须从已验证的持久化来源/固定依赖取得每个原始输入的 source 和完整 tags；不得从用户传参宣称来源，也不得把聚合 tags 并集猜成单个授权条目。受认证 data peer 对这种来源绑定负责；d0 不读取 data 私库核验任意来源断言，d1 必须实现该可信装配。

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

现有 identity peer 使用 `spiffe://miaoqia/service/platform-api`；data-api/acq-admin 的 URI 随角色证书显式发放，示例不是生产默认许可。迁移仅 acquisition 自库，不借用 data/evidence/domain 凭据。停止依赖、失联、过期和撤销时失败关闭；不使用缺撤销保障的本地权利快照。后续 data listener、可信 provenance 还原、最终响应确认和真实状态装配须独立实现/评审。
