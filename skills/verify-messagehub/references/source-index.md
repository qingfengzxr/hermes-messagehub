# 源码与证据层索引

路径相对仓库根目录。这里记录实现和补充检查的入口，不代表本次验证结果。功能地图按用户入口描述行为，本文定位实现。

## 身份和授权

- `internal/hub/store.go` 的 `AddPrincipal`、`IssueGrant`、`Authenticate`、`RevokeGrant`、`transaction`。每次通信重新检查 grant、token 期限与 principal 状态。
- `internal/hub/oauth.go` 的注册、授权请求、本地批准、继续、code 交换、refresh 与 revoke。`ExchangeOAuthToken` 对 refresh_used 提交家族撤销后返回拒绝。
- `internal/admin/admin.go` 的本地身份、grant、oauth-pending、oauth-approve 命令。公开面没有审批管理端点。
- `internal/server/oauth.go` 的路径和参数清单；`handler.go` 的认证入口、安全头和错误码。
- `internal/hub/oauth_test.go`、`oauth_rotation_test.go`、`audit_token_state_test.go` 为独立补充证据，包括受控时间、容量和重启。

限制来自实现，不依赖旧报告：client 默认上限 128，request records 默认上限 256；待批准 600 秒、code 60 秒、access 15 分钟、grant 30 天。每 grant 最多 4096 条消费历史和 64 个活跃 access。实时驱动不填满所有上限，也不等完整授权到期。

现有 `internal/server/audit_regression_test.go` 的 refresh 重放测试记录了 access 状态，但原断言只硬检后继 refresh。验证器另加 access=401 的实际断言，不修改产品测试以冒充本次验证完成。

## 会话和消息

- `internal/hub/messages.go` 的 `CreateConversation`、`ListConversations`、`Send`、`ReadMessages`、`MessageStatus`、`CloseConversation`。
- `internal/server/operations.go` 是 MCP 名称、REST 路径和参数类型的共同注册表。驱动 check 从这里读取实际入口，与地图及动态工具发现比对。
- `internal/hub/types.go` 是 wire 字段与省略语义来源。JSON 数值的验证不得先经 float64 往返。
- `internal/hub/messages_test.go` 覆盖成员、reply、JSON、幂等、游标及原子行为；`recovery_test.go` 覆盖重启和墓碑。

## 收件与 ACK

- `internal/hub/inbox_wait.go` 订阅提交提示后重新拉取；周期复查覆盖不同Store写入及撤销。`internal/server/inbox_wait.go` 单独处理REST wait，未增加MCP工具。`internal/config/config.go` 的 `max_concurrent_waits` 通过启动装配接入独立等待槽。
- `internal/hub/inbox_wait_test.go`、`internal/server/inbox_wait_test.go` 是长等待的真实 SQLite 和 HTTP 补充验收。Hermes 原生接收器不在此验证入口的范围内。
- `internal/hub/messages.go` 的 `PullInbox` 过滤未 ACK 而不是只过滤 pending 状态。
- `Ack` 先排序、去重，再在同事务内逐项更新。验证器用合法 `m_` ID 和字典序在后的合法缺失 ID，读回证明不是提前拒绝假装回滚。
- `messageReceipt` 按发送者或本人收件身份过滤可见回执。
- `internal/server/integration_test.go` 的 `TestExpiredDeliveryIsNotReportedAsStillPending` 使用受控时钟和真实 REST。`internal/hub/recovery_test.go` 的 `TestMessageRecoveryPreservesCursorsReceiptsAndExpiredTombstones` 证明墓碑、游标与 ACK；两者不是等待默认 TTL 的实时时间证明。

## 原文件

- `internal/hub/files.go` 的 `BeginAttachment`、`WriteAttachment`、`CommitAttachment`、`ReadAttachment`。
- `filesOwnedAttachment`、`filesUploadConversation` 和 `filesReserveCapacity` 分别处理上传者、会话和全库逻辑容量。
- `internal/server/operations.go` 解码 Base64，offset 为原始字节。
- `internal/hub/files_test.go` 的部分 fixture 直接构造 SQL 关系，只能记为存储层测试。
- `internal/server/integration_test.go` 是真实 Store 加官方 SDK，但原文件 happy path 混合使用两种协议；本技能驱动补齐每种协议的四个文件入口。

## 启动与恢复

- `cmd/messagehub/main.go` 的 `run` 装配配置、SQLite、OAuth 和 Kratos。`serve`、`admin`、`version`、`help` 的实际参数以该文件为准。
- `internal/config/config.go` 定义严格配置及完整 Host authority，`listen=:0` 不会自动替换 Host 中的端口。
- `internal/hub/store.go` 在修改权限前识别 SQLite/WAL 身份，拒绝硬链接及外部 schema。
- `internal/hub/backup.go` 使用 `VACUUM INTO`、新文件、私有父目录、quick_check 和 Sync。
- `cmd/messagehub/smoke_test.go` 有真实二进制同库重启，但快照最后由 Store 打开。`bootstrap_test.go` 强杀的是初始化 admin 进程。本技能另启动真实备份实例，不能混写这几个证明层。
- `scripts/go` 固定镜像、隔离网络和用户、只读源码/模块缓存；技能入口必须强制 `MESSAGEHUB_SOURCE_MODE=ro`。

## 补充检查

通过 `terminal` 在模块目录运行，需要与主技能相同的已准备缓存：

```sh
bash scripts/go test -json ./internal/hub ./internal/admin ./internal/config ./internal/server ./cmd/messagehub -count=1
bash scripts/go test -race ./... -count=1
bash scripts/go vet ./...
```

统计 JSON 事件时区分 package、顶层测试和子测试，检查实际匹配和 skip/fail。补充测试不能替代真实二进制驱动；真实驱动也不能代替全部并发、TTL 和容量负例。

## 外部未验收项

真实浏览器的 HTTPS cookie 行为、真实平台 OAuth onboarding、真实 Agent 原生线程映射/主动唤醒/预算、生产 ingress、磁盘加密与备份策略、旧服务迁移均不在本次隔离驱动范围。不得为填满地图临时启用这些能力。
