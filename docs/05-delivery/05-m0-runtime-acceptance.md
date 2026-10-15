# M0 认证、探针与消费运行时验收

> 状态：三项实现与真实组件门禁已关闭 · 版本：1.1 · 更新：2026-10-15 · 负责人：平台工程、安全、数据负责人

本文对应 C4a 契约冻结后需要关闭的 HTTP 认证、完整 M0 探针与现有消费者载荷校验门禁。记录见[运行时验收清单](../../contracts/reviews/m0-runtime-acceptance.yaml)。关闭依据是实际生产代码和隔离的真实组件测试，不代表已经部署到生产环境，也不把这些结果扩展为整个 M0 或身份子系统全部完成。

## 1. 门禁与证据

| 门禁 | 状态 | 覆盖与关闭证据 |
|---|---|---|
| M0-HTTP-AUTH | 实现及真实组件验收通过 | 网关验证 HTTPS Keycloak 的 RS256 Bearer 访问令牌、issuer、audience、exp/iat、nbf、主体和租户/单元；丢弃客户端上下文头。内部调用 TLS 1.3 双向认证、可信 CA、单个 SPIFFE URI SAN 和精确调用方白名单；缺配置失败关闭。签名、算法、令牌类型、受众/期限、JWKS换钥、无证书、错误CA、过期证书、未授权服务均有反例 |
| M0-PROBE | 实现及真实组件验收通过 | HTTPS 网关 → mTLS identity/OpenFGA → operations 事务发件箱 → Go中继/Kafka → 独立 Python acquisition/evidence → Go operations 完成 → governance 审计落库；路径为 operations,acquisition,evidence,operations。验证追踪连续、允许/拒绝审计、重复完成去重、同分区重放栅栏与审计故障时业务回滚 |
| M0-CONSUMER-PAYLOAD | 现有四个消费者验收通过 | operations.probe-completion、governance.audit-ingest、acquisition.probe、evidence.probe 均接入离线原始Schema载荷校验。当前全部事件载荷正反例由Go/Python运行；真实Kafka验证四个消费者各自的结构错误和业务语义错误进入死信，且无持久化业务去重行。死信发布被Kafka拒绝时不提交后续记录越过失败位点 |

实际组件：Keycloak 26.3.4（隔离测试realm及HTTPS）、OpenFGA 1.10.0、Kafka 4.1.0、PostgreSQL 18.6。应用以生产认证策略运行；Keycloak测试realm的密码登录客户端只用于本地/CI测试，不是生产客户端模板。CI使用PostgreSQL17.6及相同Keycloak/OpenFGA/Kafka版本，CI结果须由相应流水线确认。

## 2. 认证配置

默认按生产策略启动；只有显式 `MIAOQIA_ENV=development` 才允许实验环境明文传输。配置缺失不退回明文，也不跳过令牌或内部服务身份验证。

| 配置 | 含义 |
|---|---|
| `MIAOQIA_GATEWAY_ISSUER` | Keycloak realm的精确HTTPS issuer |
| `MIAOQIA_GATEWAY_AUDIENCE` | 本资源服务器audience，例如miaoqia-api，必填 |
| `MIAOQIA_GATEWAY_CA_FILE` | 可选私有签发方CA，追加到系统信任根；不会关闭服务端证书验证 |
| `MIAOQIA_PUBLIC_TLS_CERT` / `MIAOQIA_PUBLIC_TLS_KEY` | 对外HTTPS服务证书与私钥文件 |
| `MIAOQIA_INTERNAL_TLS_CERT` / `MIAOQIA_INTERNAL_TLS_KEY` / `MIAOQIA_INTERNAL_TLS_CA` | 内部服务证书、私钥与独立CA信任根 |
| `MIAOQIA_INTERNAL_ALLOWED_PEERS` | 逗号分隔的精确SPIFFE URI SAN白名单，例如spiffe://miaoqia/service/platform-api；不使用通配符 |

上下文仍表达原调用主体；mTLS验证的是发送该上下文的服务。白名单服务可代理已验证的用户请求，授权关系仍由identity/OpenFGA判断。服务身份认证不会自动授予业务资源权限。

当前集成安全配置为[3.0.0](../../contracts/integration.yaml)；既有Check与事件v1结构保持兼容；identity增量接口为1.1.0。租户生命周期和目录核验由[身份验收](07-m0-identity-acceptance.md)单独覆盖，此前验收记录的版本/哈希按审计用途保留。首批C4a/C4b冻结记录按审计用途保留，不改写历史哈希。

## 3. 载荷、语义与故障处理

`mqc generate` 复用事件兼容检查的引用展开，生成两语言相同的 `payloads.json` / `event_payloads.json`；保留原Schema的required、额外字段、条件及format等校验关键字。运行时只读本地制品，不从信封dataschema取网络Schema。每个登记处理函数启动前必须有可编译的Schema；已登记契约不能被私有Schema覆盖。未登记处理函数的未来事件仍可忽略，不能借此让已登记的处理函数跳过校验。

结构校验在去重及业务事务前完成；失败直接死信。来源/主体/路径、审计租户与来源子系统的一致性由相应应用所有者验证，语义错误回滚事务后按重试策略进入死信。审计写入失败不能被隐藏：允许请求的探针、请求事件和审计一起回滚；拒绝请求的审计失败返回运行错误，不能伪装成已经正常记录的拒绝结果。

只有死信投递成功才能确认原消息。Go消费者遇到未确认死信停止当前批次并退出，不让后续同分区记录的提交跨过失败位点；Python失败异常传到运行时并停止进程。恢复后依靠inbox和业务状态幂等处理。

即使不配置OTLP导出，Go/Python运行时也安装真实TracerProvider生成并传递追踪上下文；配置导出时使用正常TLS设置，不硬编码关闭TLS校验。完整探针测试使用同一生产追踪初始化代码。

## 4. 复验

准备隔离PostgreSQL、Kafka、OpenFGA、HTTPS Keycloak，并导入[本地测试realm](../../deploy/local/keycloak/realm-miaoqia.json)。`tools/dev/m0_test_certificates.sh <临时目录>`仅生成测试证书，不能用于实际部署。设置 `MIAOQIA_TEST_PG`、`MIAOQIA_TEST_KAFKA`、`MIAOQIA_TEST_OPENFGA`、`MIAOQIA_TEST_KEYCLOAK` 和 `MIAOQIA_GATEWAY_CA_FILE` 后运行：

```sh
uv sync --project data --locked --all-packages
go -C services test -race -count=1 ./kernel/integration ./kernel/relay ./kernel/transportsecurity ./subsystems/gateway/internal/auth
uv run --project data --locked --all-packages --group dev pytest -q data/kernel/tests
uv run --project tools/contracts --python 3.12 --locked mqc check
uv run --project tools/contracts --python 3.12 --locked mqc generate --check
```

services CI运行完整Go竞态测试和真实跨语言链路；data CI运行Python消费框架与载荷正反例；contracts CI检查运行时制品与源契约的一致性。缺少真实组件时测试会显示skip，不能据此关闭门禁。

## 5. 独立部署门禁

实际生产环境的证书签发/轮换/吊销、反向代理与网络策略、Kafka/数据库服务身份和ACL、生产Keycloak客户端流程、备份恢复及监控告警仍按部署与上线门禁验收。新增消费者必须接入框架并补自己的实际业务语义与集成测试；未来接口、审批/MCP与全国数据来源不由本次四个消费者和探针代证。
