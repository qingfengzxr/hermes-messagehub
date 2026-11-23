# MessageHub verification map

本目录按用户能访问的入口维护验证范围。能力存在、某层测试通过和生产已启用是不同状态。索引不写永恒 PASS；每次运行以精确证据报告为准。

## Baseline preconditions

- 在源码仓库根目录，按主 SKILL 的 Launch 执行。
- 驱动使用本次编译的真实程序、独立数据库及 alice/bob/carol 测试身份。
- 一个当前服务实例，调用串行。身份和文件必须经 CLI/REST/MCP 创建，不直接写 SQL。
- 每个分区前 doctor；失败后重新检查，不继续使用状态不明的实例。
- 所有 token、cookie、code 仅留在私有运行态或内存，证据保存在独立目录。

## Features

| ID | Feature | 用户入口 | 实时驱动范围 |
| --- | --- | --- | --- |
| identity | [身份与 OAuth](./identity-oauth.md) | 身份工具、本地身份管理、发现与授权路由 | 两种身份入口、停用/撤销、PKCE、本地批准、refresh 家族 |
| messages | [私有会话与消息](./conversations-messages.md) | 创建/列表/发送/历史/状态、CLI 关闭 | REST 与 MCP、幂等、回复、分页、非成员拒绝 |
| inbox | [收件箱与确认](./inbox.md) | 拉取、长等待、ACK、投递状态 | 主驱动覆盖拉取与 ACK；长等待另由 Go 的 SQLite 和 HTTP 测试覆盖 |
| files | [原文件传输](./files.md) | begin/write/commit/read、消息绑定 | 两协议完整往返、原字节/摘要、重试与授权 |
| recovery | [启动与恢复](./lifecycle.md) | serve/help/version、本地备份、隔离配置切换 | 真实进程重启、真实备份实例、授权快照边界 |

## Driving conventions

完整地图用 `bash skills/verify-messagehub/scripts/verify -feature all`。单分区诊断使用同一入口的 `-feature` 参数，不复制另一套 launch 流程。两个协议的入口须分别有证据，不能用 REST 的结果推断 MCP 通过。

每个 feature 文件恰有四个 H2：`Sub-features`、`How to get to it (user POV)`、`Driving it with verify-messagehub`、`Gotchas`。代码入口与现有测试集中在 `../references/source-index.md`，不把内部函数当用户入口。

## Proof and boundaries

- 记录本次实际动作及后置读取，不只记录最后画面或工具列表。
- Sub-features 描述源码能力；Driving 描述本驱动实际断言。全图通过表示五个分区和双协议入口都执行，不表示每个参数组合、负例或子分支都已穷尽。
- 通过记录中的源码/二进制指纹绑定结果。旧报告不是当前源代码的证明。
- 每个 API 成功检查响应结构及关键值，拒绝检查响应码或 MCP tool 错误；不能把空结果当拒绝。
- 文件比较完整字节、大小和 SHA-256。ACK 后无 cursor 重新读取；不能用翻页跳过消息冒充确认成功。
- CLI 备份回执、存储层打开快照、真实程序从快照启动是三个证据层，本驱动要求最后一层。
- 时间推进、资源耗尽、并发竞态有现有隔离测试，但不是本实时驱动的全部覆盖。补充检查见 source-index，执行时单独记录结果。
- 本 Go 驱动不覆盖真实 Bot 接入、生产 Agent 唤醒、网页 SSO、生产迁移、端到端加密或自动物理清理。Hermes 接收器的会话与恢复行为须在客户端单独验证。
