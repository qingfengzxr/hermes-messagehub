# 启动、重启与备份恢复

同一程序可从自己的数据库重新启动，并创建一致性快照。恢复是停掉当前实例后切换到本次隔离快照，不是产品内置的 restore 子命令。

## Sub-features

- runtime.launch：真实程序的 serve、version、help 和认证后的 doctor。
- runtime.config：本轮专用配置、具体 loopback 端口、匹配 Host、独立绝对数据库路径。
- runtime.restart：精确停止并回收原进程，同库重启后保留消息、幂等、待确认收件和文件。
- runtime.backup：本地备份返回 verified，目标为新私有文件，重复目标不覆盖。
- runtime.restore：真实程序从快照副本启动，验证消息、完整文件及 ACK 的快照视图。
- runtime.cleanup：关闭自有 session/进程和临时数据，证据仍在。
- runtime.wake-status：本地读取唤醒队列汇总，不读取 webhook 凭据，也不返回消息或路由 ID。

## How to get to it (user POV)

- 本次编译程序的 `serve --config`、`version`、`help`，健康入口 `GET /healthz`。
- CLI `admin --config` 下的 `backup`；身份管理使用 `principal-add`、`grant-create`、`grant-revoke`。
- CLI `admin --config` 下的 `wake-status`；不通过 HTTP 或 MCP 暴露。
- 数据验证仍通过消息、收件和附件的 REST/MCP 入口，不调用内部 Store。
- 本地 `conversation-close`、`principal-disable`、`principal-enable`、`oauth-pending`、`oauth-approve` 的业务证明分别位于消息及身份分区，不把它们当作 HTTP 管理接口。

## Driving it with verify-messagehub

Preconditions: 使用主技能启动的隔离服务；fixture 中的配置、凭据、快照目录私有。三个实例顺序运行，不同时持有同一测试服务生命周期。

通过 `terminal` 从仓库根目录执行：

```sh
bash skills/verify-messagehub/scripts/verify -feature recovery
```

- 原实例。创建可辨认的消息和原文件，记录真实 ID、幂等请求、pending 状态和字节摘要。
- 同库重启。SIGTERM 后等待原进程退出，再启动同库程序并重新 doctor。用原 token、原幂等请求和公开读取验证状态保留。
- 在线备份。明确 ACK 后调用真实 CLI backup，检查 verified、新文件及私有权限；相同输出路径再次备份应拒绝。
- 制造快照差异。备份之后新增消息并撤销 bob，验证原库中 bob 401，确保恢复检查能区分快照与原库。
- 快照实例。停止当前进程，复制本次快照到独立 restore.db，只更新 fixture 配置，启动同一个程序并 doctor。公开读取确认快照前消息、文件和 ACK 保留，快照后消息不存在，未过期 bob token 回到快照时的可用状态。
- 收尾。精确回收最终实例、删除本次临时运行态，确认报告和合成样本不随之消失。
- `wake-status` 由 `cmd/messagehub/grok_wake_test.go` 的 CLI 测试覆盖。恢复驱动不启用 webhook，不验证外部例程投递。

## Gotchas

- healthz 只有 liveness，必须同时验证认证身份和数据读回。
- 配置 listen 的端口 0 不会自动修正默认 Host 列表。驱动先选定具体端口再写配置。
- 默认回环监听不等于禁止显式公网 bind；本驱动固定在无网容器的 loopback，不代替生产 ingress 审查。
- principal-add 重复是冲突，grant-create 换输出路径会新建授权；它们不是消息层的幂等重试。
- 备份包含授权快照，快照之后的撤销会被回滚。该现象要明确记录，不写成“撤销在旧备份恢复后仍保证有效”。
- 首次建库的空 hot journal 回归见 `TestEmptyBootstrapHotJournalRecovers` 与 `TestBinaryBootstrapCrashRecovery`；外部库、非空 journal 和侧文件链接必须拒绝且不改数据。
- 存储层打开备份或初始化子进程强杀测试，不等于真实服务的备份启动/完整崩溃恢复。本分区证明有序重启与快照实例；并发/崩溃边界另见补充测试。
