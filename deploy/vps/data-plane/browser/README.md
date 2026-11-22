# Chromium HTTPS 隔离夹具

本目录复用 Acquisition 的 `PageProvider` 与 `PlaywrightBrowserRenderer`，在 Linux 上以合成 HTTPS 发布者验证同次导航的原件和 DOM 投影。夹具与[开发 VPS 部署拓扑](../../../../docs/03-architecture/07-deployment-topology.md)中的发布者准入和 C11 门禁分别验收。

## 运行

从仓库根目录运行。工作站需要 Bash、tar 和 uv。目标为授权的 Linux x86-64 开发环境，需 rootless Podman/Netavark、nftables、nsenter、ip、OpenSSL、jq、Python 3、curl 和 GNU timeout。首次运行会下载固定摘要的 Playwright 镜像、固定校验和的 NSS 工具和 seccomp 输入，并从 PyPI 获取锁定哈希的二进制 wheel。构建与运行使用一次性 Podman 存储。

```sh
bash deploy/vps/data-plane/browser/verify-confinement.sh --ssh "$MQ_FIXTURE_SSH_HELPER" > chromium-receipt.jsonl
```

由运维方在本地提供已授权的 SSH helper。它以一个参数接收远端 Shell 命令，并透传 stdin 和 stdout。在具备上述依赖的本地 Linux 上可省略 `--ssh`。默认模式传输当前工作树的采集源码、对应加锁依赖和夹具源码。浏览器归档事件交付模式另传输数据角色源码和 Go 中继二进制。发布凭据留在原环境。

可将 `--tls-pair` 放在 `--ssh` 前，仅运行同次导航配对与两个证书反例。该收据会标记其余用例未执行。Acquisition 运行时的 [渲染器测试](../../../../data/runtimes/acquisition/tests/test_browser_renderer.py) 检查目标、资源类型、HTTP 方法、部署方路由的继续执行和上下文限制装配失败。

## 拓扑与证据

- 两个无默认路由、无 DNS 的内部桥接网络容纳合成发布者、两个 TCP canary 和浏览器。浏览器仅连接第一个网络，镜像只读，进程无容器 capabilities，启动时无宿主 bind 或 volume。第二个网络模拟业务网络。发布者仅持有本轮合成叶证书和私钥。
- 启动 Chromium 之前在浏览器专用 namespace 装入 nftables 双向默认拒绝。出口许可一个源地址到固定 IPv4 的 TCP/443。浏览器保留 HTTPS 主机名和 SNI。规则在外部安装，浏览器无权更改。socket 探针、独立规则计数和已就绪的监听器共同检查异端口、异地址、UDP 和 DNS。禁止修改宿主防火墙或常驻服务。
- 每次渲染启动新 Chromium，使用一次性信任数据库、profile 和匿名 context。镜像中的浏览器版本与 Python Playwright 绑定。seccomp 配置从固定的 Playwright 上游输入派生，`clone3` 返回 ENOSYS 以走受过滤的 `clone`。允许 Chromium zygote 在其子用户 namespace 内调用 `chroot`，内核仍检查该 namespace 的 capability。
- 夹具调用实际的 `PlaywrightBrowserRenderer` 安装 context HTTP 与 WebSocket 路由。HTTP 仅放行指定页面主 frame 的两个精确 GET，导航要求 Document，正文要求 Fetch 或 XHR。允许的请求继续经过部署方已有的路由及 CDP 许可。WebSocket 直接关闭。路由装配失败或出现上下文拒绝时丢弃配对。
- 成功例分别观测两个 GET 的 CDP 请求、TLS 和 peer，并与发布者接受的本地及对端 socket 比对。独立发布者对发出的 Windows-1252 字节做哈希，渲染器核对原件字节、主 frame/loader、导航和数据请求 ID、许可完成顺序与完整选中 DOM。收据仅保留布尔值、计数、协议、版本和摘要。
- 失败例由收据逐项显示是否实际执行、返回配对、观察到的许可请求与越界到达数。发布者日志不完整时，到达计数记为 `null`。退出时删除本轮容器、网络、临时 profile/证书和 Podman 存储。适合归档的输出是 stdout JSON Lines。非零退出保留测得的收据及失败阶段。

IPv4/TCP pin 限定远端地址和端口。context 路由与 CDP 许可在固定版本的 Chromium 上共同测试。收据覆盖下列已执行的合成场景，`all_target_http_policy_verified` 保持为 false。[生命周期夹具](lifecycle/README.md)覆盖合成 TCP、租约到期及 Chromium 组合验证。真实 DNS 集合及重绑定、宿主业务地址、真实发布者准入和 C11 使用各自的验收入口。

## Chromium 与监督器组合

从仓库根目录运行，使用已授权的开发 VPS helper。工作站需要 `uv`，目标机额外需要 Python 的 pidfd 接口及 `ss`。

```sh
bash deploy/vps/data-plane/browser/verify-confinement.sh --lifecycle \
  --ssh "$MQ_FIXTURE_SSH_HELPER" > chromium-lifecycle.jsonl 2> chromium-lifecycle.private.log
```

- 同一浏览器首先通过正式渲染器取得两次合成 HTTPS GET 的原件和投影。发布者独立核对请求次序、TLS 对端和正文摘要。渲染器仍负责管理 capture context 的路由和许可。
- 夹具在同一个 Chromium 进程另开两个匿名探针 context，限制为合成发布者的一个固定 GET。第一个 context 在撤销前完成额外一次 GET 并保留连接。第二个在撤销后尝试建立新连接。两个探针均要求原采集页面仍能执行已设置的标记读取。额外探针 GET 单独计数。
- 独立进程通过 pidfd 向监督器自有子控制器发送 SIGKILL。监督器先确认集合清空，再启动两次探针。内核分别计数已建立四元组上的 PSH 包和新 SYN 包。发布者记录每个 TCP 接受和 HTTP 请求，证明观测窗口的到达数。观测窗口覆盖发出信号前的基线至撤销后第二次对端 TLS 健康检查。信号系统调用前至 pidfd 观测的耗时给出实际退出至观测的上界，pidfd 观测至确认撤销单独计时。
- 主脚本验证容器、网络、namespace 和临时存储清除。独立 SSH 复查使用私有 stderr 中 `fixture_cleanup_ids` 行打印的两个目录名。在确认它们分别匹配 `^mq-chromium\.[A-Za-z0-9]{8}$` 和 `^mq-chromium-run\.[A-Za-z0-9]{8}$` 后，检查 VPS 上 `/var/tmp/` 下对应两个目录均已消失。收据归档仅保留检查结果。

夹具覆盖一个自有控制器子进程、一个合成 HTTPS 对端及短观测窗口。监督器、Podman 或 SSH 包装进程强杀后的回收，真实控制组件接线和所有 Chromium 出口路径仍待验证。

## 控制进程边界

Acquisition 控制进程持有页面许可、响应观察和归档权限。隔离端通过由监督器管理的独占 Playwright stdio 会话运行 driver 与 Chromium。会话限单轮和不可续期的期限。控制器经专用 Unix socket 接入，启动器使用固定命令与净化环境，出站许可由独立内核策略约束。浏览器侧只接收页面执行协议，采集数据库、暂存卷和对象存储凭据留在控制侧。Playwright 的上下文路由和 CDP 响应事件由既有 renderer 验证，原始响应和投影沿既有归档路径提交。

本机[协议验证](test_provider_ipc.py)使用[锁定的 Playwright](playwright.requirements)、Chromium 和合成 HTTP 发布者，验证独立 driver 的上下文路由、WebSocket 拒绝、CDP 事件、断开和半帧清理。将 Chromium 安装到 Playwright 的默认缓存或通过 `PLAYWRIGHT_BROWSERS_PATH` 指定缓存，再从仓库根目录用已同步的 data 开发环境运行。

```sh
uv run --project data --locked --all-packages --no-sync --with 'playwright==1.60.0' -- \
  python -m pytest -q -p no:cacheprovider deploy/vps/data-plane/browser/test_provider_ipc.py
```

系统组合验收进一步绑定监督器持有的控制进程身份、Unix socket 对端与单轮出站租约，检查异常断开时撤销网络、回收进程和读回独立发布者记录。通过组合验收和逐源公开依赖检查后，由来源负责人确认真实发布者的启用范围。

组合夹具采用独立的内网浏览器容器与无网络的控制器容器。由宿主侧监督器创建一次性 socket，控制器容器只读挂载该 socket 所在的私有目录。监督器用 Linux 对端凭据核对本轮控制器进程与宿主映射身份，并用 pidfd 监视退出。浏览器容器通过由监督器启动的固定 driver 命令和标准输入输出接收执行协议，无控制 socket 挂载。租约在进程核验后放行，控制端退出或通道断开时由监督器撤销，内核到期独立生效。

```sh
bash deploy/vps/data-plane/browser/verify-confinement.sh --provider-ipc \
  --ssh "$MQ_FIXTURE_SSH_HELPER" > chromium-provider-ipc.jsonl 2> chromium-provider-ipc.private.log
```

运行先验证通过真实许可的两次 HTTPS 请求和配对摘要，再由监督器验证另一次控制器在正文许可尚未释放时被终止。负例核对浏览器进程清除、租约撤销、独立发布者健康检查和正文零到达。按本页的临时目录复查步骤核对清理。

公共测试 VPS 的 SELinux 处于 Enforcing。孤立 socket 对照确认客户端 UID 与文件权限匹配时，默认容器标签仍拒绝连接宿主监听进程。跨容器 IPC 夹具的合成控制器使用 Podman 的 `label=disable`，其 socket 目录仍独占且只读挂载，控制器只保留回环网络。浏览器容器沿用原隔离设置。常驻角色需要独立验证实际 SELinux 控制通道装配。

## 采集角色与归档验收

角色夹具由同一 `acq-browser` 进程领取一条计划，依次采集列表和正文。每次渲染开启独立 operator provider 会话，由监督器针对同一个已固定进程核对 IPC 对端并签发一次内核租约。控制器只连接夹具私网中的 PostgreSQL 与 S3 兼容服务。浏览器只连接合成发布者。列表和详情各用两次精确 GET，原站键来自当前合成来源规则。分别核对列表字节、正文和投影的 S3 回读、已结算计划、浏览器观察绑定与浏览器归档事件发件箱。归档收据与后续 Kafka 交付、解析和持久环境验收分开记录。

```sh
bash deploy/vps/data-plane/browser/verify-confinement.sh --provider-role \
  --ssh "$MQ_FIXTURE_SSH_HELPER" > chromium-provider-role.jsonl 2> chromium-provider-role.private.log
```

该模式在同一隔离 Podman 存储中额外拉取固定摘要的 PostgreSQL 和 SeaweedFS。数据库初始 owner 凭据只进入初始化进程，采集角色用 fixture app 身份，独立核验容器使用 PostgreSQL 只读事务和对象只读接口。夹具控制器沿用单容器 SELinux 标签例外。首次拉取需要可用镜像源。收据只记录哈希、计数和阶段，按同一临时目录复查步骤核对清理。

## 浏览器归档事件交付与独立解析

交付夹具接续同一个隔离采集实例。采集角色、浏览器与合成发布者退出后，Go 发件箱中继将该次 Chromium 归档生成的浏览器归档事件和来源健康变更写入同一私网内的 Kafka。读回核对两条消息的字节和消费位点，确认 `data-consumer` 只将浏览器归档事件记入 Evidence inbox 和待解析队列。停下消费者后，仅为该合成来源开启独立重放，通过 Evidence 正式命令核对规范化快照与成对原件，再次重放核对幂等。采集和解析的验收分开记账。

```sh
bash deploy/vps/data-plane/browser/verify-confinement.sh --provider-ingest \
  --ssh "$MQ_FIXTURE_SSH_HELPER" > chromium-provider-ingest.jsonl 2> chromium-provider-ingest.private.log
```

工作站还需要 `sha256sum` 以及与当前 [Go 模块声明](../../../../services/go.mod)兼容的工具链。脚本交叉编译当前工作树的 Linux amd64 `platform`，以锁定的 Python 依赖装配数据角色。每次运行使用独立 PostgreSQL 库、消费组和 S3 数据目录。Kafka 在无宿主机发布端口的内部网络上使用明文，数据库和对象存储连接配置由夹具生成。验收长期运行时另验常驻端点的 TLS、访问权限和凭据。

## 实测收据

[同次采集交付收据](receipt-provider-ingest.jsonl)记录公共测试 VPS 上的合成发布者、Chromium、Go 中继和正式 `data-consumer`。Kafka 分区 0 的位点 0 和 1 分别保存浏览器归档事件和来源健康变更，实际消费组提交位点 2。关闭解析时保留一条待解析观察和一条 inbox 记录，快照和死信均为零。停止消费后显式重放产生一个绑定同一浏览器归档事件、原件及投影的规范化快照，第二次重放处理零条。独立复核核对输入摘要和临时目录清理。[C11 收据](../../../../contracts/reviews/c11-dev-operations.yaml)

[采集角色归档收据](receipt-provider-role.jsonl)记录两个独立浏览器会话、四次 HTTPS GET、一条已结算计划和一条带稳定原站键的观察。64 字节列表、18 字节正文和 185 字节投影分别从 SeaweedFS 回读，列表与正文摘要同独立发布者记录一致。PostgreSQL 登记列表原件 30 天到期，并在观察提交后保留一条待发浏览器归档事件。采集角色收到 SIGTERM 后退出 0。[C11 收据](../../../../contracts/reviews/c11-dev-operations.yaml)

[跨容器控制收据](receipt-provider-ipc.jsonl)记录 Unix 对端与 pidfd 核验、两次 TLS 1.3 GET、18 字节原件和 185 字节投影。第二次会话在正文许可阶段终止真实渲染控制器，监督器观察退出并确认撤销，浏览器进程回收，合成发布者维持健康且正文零到达。公共测试 VPS 的 Linux 会话测试另有 17 个用例，覆盖凭据不符、无效 pidfd、背压、断开、超时和失败恢复。[C11 收据](../../../../contracts/reviews/c11-dev-operations.yaml)

[Chromium 生命周期收据](receipt-chromium-lifecycle.jsonl)记录正式渲染器取得 18 字节原件及 185 字节投影。以信号系统调用前为起点，退出至观测的上界为 677 微秒。监督器在观测后 135 毫秒确认撤销。原采集页面仍可执行脚本时，内核记录旧连接 4 个 PSH 包及新连接 2 个 SYN 包被丢弃。发布者的两次 TLS 健康检查通过，浏览器新增 TCP 接受和 HTTP 请求均为零。独立 SSH 复查确认本轮临时目录已删除。[首次组合运行](receipt-chromium-lifecycle-attempt.jsonl)保留 TLS 协议名称比较未通过时的独立网络和清理证据。

[运行时上下文收据](receipt-runtime-context.jsonl)记录 20 个合成用例通过。两个弹窗分别复用准许的导航和正文地址，发布者未收到任何新增未许可请求。WebSocket 在出站前关闭。另加的部署方路由拒绝了原本可继续的主页面导航，发布者收到零请求。合法配对的两次许可、原件字节和固定 TLS 对端全部通过。

[基线 VPS 收据](receipt.jsonl)保留 18 个场景及各自原判定。未安装 context guard 时，popup 和 WebSocket 各有一次未许可请求到达，均未返回配对。Service Worker 注册没有到达发布者，页面仍返回已许可配对，基线用例判定为失败。

[Context VPS 收据](receipt-context-exact-popup.jsonl)记录 18 个用例通过及逐项清理验证。popup 直接请求允许作为 Fetch/XHR 的精确正文地址。popup 正文到达数和 WebSocket 升级到达数均为零，两例都未返回配对。正例还从第二个页面访问精确导航地址，收到 context 拒绝，publisher 只看到指定页面的一次导航 GET 和一次正文 GET。两次成功响应由 Chromium CDP 与发布者 socket 校验固定 peer 和 TLS 1.3，保留 18 字节原件和 185 字节规范化投影的摘要。两个证书反例分别记录 `ERR_CERT_AUTHORITY_INVALID` 与 `ERR_CERT_COMMON_NAME_INVALID`，HTTP 到达均为零。Service Worker API 可调用，注册被阻止，发布者只收到许可的两次请求。

审计保留初步的 [TLS 配对](receipt-context-tls-pair.jsonl)、[WebSocket 收口前](receipt-context.jsonl)、[发布者计数不可用](receipt-context-final.jsonl)和[日志校验恢复](receipt-context-verified.jsonl)四份 context 收据。旧计数不可用的收据中的零值不构成到达证据。

历史审计收据保留了[Playwright 回调注册失败](receipt-attempt-06.jsonl)、[修正回调后的首次网络证据](receipt-attempt-07.jsonl)和[证书反例及命令行探针失败](receipt-attempt-08.jsonl)。
