# 外部控制器退出与 TCP 出口撤销

在授权的 Linux 开发环境上，从仓库根目录运行。工作站需要 Bash、tar 和 OpenSSL。目标机需要 Python 3 的 pidfd 接口、rootless Podman、Netavark、nftables、nsenter、ip 和 GNU timeout。

```sh
bash deploy/vps/data-plane/browser/lifecycle/verify-lifecycle.sh --ssh "$MQ_FIXTURE_SSH_HELPER" > lifecycle-receipt.jsonl
```

在满足依赖的本地 Linux 上可省略 `--ssh`。脚本拉取与上级夹具相同的固定摘要镜像，只运行合成 TCP 对端和客户端。两个容器使用同一个无 DNS 的内部桥接网和独立存储。控制器、SIGKILL 注入进程和监督器分别运行于容器外。监督器只跟踪自己启动的子控制器。

## 出口与恢复

监督器保留客户端网络命名空间的文件描述符，每次调用 `nsenter` 前校验 Podman `SandboxKey` 的 inode。双向默认拒绝规则先于客户端探针安装。带超时的 nftables 集合允许唯一合成 IPv4 地址的 TCP/443，入站和出站规则均查该集合。控制器退出时，监督器从 pidfd 观察退出，清空集合并验证结果。集合超时独立于监督器运行。该原语接受夹具拥有的命名空间和子进程。[Chromium 组合夹具](../README.md#chromium-与监督器组合)复用同一原语。

运行有 280 秒总超时，容器中的临时进程各有 90 秒上限。保留 stderr 输出的本次恢复标记。运行意外中断后，可在同一目标和用户下重复执行清理命令。清理校验目录所有者、目录标记和排他锁，按本次标签删除隔离存储中的容器及网络。

```sh
bash deploy/vps/data-plane/browser/lifecycle/verify-lifecycle.sh --cleanup "$MQ_LIFECYCLE_RUN_ID" --ssh "$MQ_FIXTURE_SSH_HELPER"
```

## 实测

- SIGKILL 注入进程在 watchdog 开始等待后发信号。pidfd 报告子进程退出，136 毫秒后监督器确认集合为空。撤销前有两次连接被对端接受，持续使用的连接收到两次应用消息确认。撤销后，客户端在仍处于 `ESTABLISHED` 的连接上发送新消息，同时另测新建连接。两个容器仍在运行，已有连接和新连接的丢包计数分别增加 3 和 1。对端收到零条新消息，接受零条新连接。随后监督器删除客户端，外层清理检查网络、命名空间和隔离存储。见 [生命周期收据](receipt-verified.jsonl)。
- 在单独的超时阶段，4 秒租约下新建连接成功。控制器仍活跃时租约自然过期，下一次连接触发 SYN 丢弃。见同一收据中的 `expiry` 记录。
- 首次运行在策略装配和存储清理阶段失败。对该次目录执行定向恢复后复查目录已不存在。另用当前清理命令删除预先创建的独立测试网络，再执行确认幂等。见 [首次运行](receipt.jsonl)和[恢复收据](receipt-recovery.jsonl)。

本页的独立 TCP 测量从 pidfd 观察到退出起算。监督器强杀、Podman 崩溃、SSH 包装进程强杀后的输入暂存目录、外部 PID 和长驻 provider 均待验证。Chromium 的证据见组合夹具，真实发布者准入与 C11 按各自的验收入口核验。
