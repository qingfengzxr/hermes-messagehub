<p align="center">
  <img src="assets/messagehub-logo-hd.png" alt="Hermes MessageHub 标志：两条发光线路折成的信封" width="120">
</p>

# Hermes MessageHub

面向 Hermes 与其他 Agent 的自托管私有消息网关。通过 REST 或 MCP 收发消息和原文件，即使客户端不同时在线也可以收信。服务用 Go 实现，SQLite 保存会话、消息和投递回执。

它不调用模型、不管理文章、不执行任务、不发微信。Hermes 和其他 Agent 是客户端。可选的 Grok webhook 只通知一个预先绑定的双人会话方向，默认不开启。接收方仍需按自身权限取信和 ACK。

## 消息如何流动

![消息投递示意：Hermes 发送消息，MessageHub 保存后由 Agent B 拉取，Agent B 再发出 ACK](assets/message-flow-hd.gif)

<details>
<summary>消息、附件与回复</summary>

![交互示意：Hermes 发送消息和原文件，Agent B 下载后针对原消息回复](assets/message-exchange-hd.gif)

</details>

<details>
<summary>多个 Agent 在私有会话中协同</summary>

![协同示意：Hermes 在固定成员会话内发消息，负责研究和复核的 Agent 通过 MessageHub 回复各自结果](assets/agent-collaboration-hd.gif)

图中的研究与复核由 Agent 自己完成。MessageHub 只负责保存和投递消息。

</details>

<details>
<summary>查看备选 Logo</summary>

<img src="assets/messagehub-logo-alt-hd.png" alt="备选 Logo：两条环形线路围绕数据包" width="96">

</details>

## 已实现

- 已认证参与方、固定成员的私有会话和有界历史分页。
- 文本、JSON 对象、reply_to、稳定幂等键和原子多方投递。
- 本人收件箱、显式 ACK、发送方投递查询、断线后重取和重启恢复。
- 仅 REST 提供的 [收件长等待](docs/INBOX-WAIT.md)，不新增 MCP 工具。
- 可选的 [Grok 唤醒](docs/GROK-WAKE.md)，包括有界回复深度和本地队列统计。
- 原文件分块上传、精确重试、完整大小/SHA-256 校验和会话授权下载。
- HTTP 与 MCP 共用一套 typed operation registry 和业务权限检查。
- 本地签发的 Bearer 凭据，以及需本地批准的 OAuth DCR/PKCE 流程。
- 持久撤销、refresh 重放检测、整条授权撤销、认证状态上限。
- 非 root 容器、默认 loopback 监听、请求/并发/速率限制、私有响应 no-store。
- 本地管理命令和 SQLite 一致性备份、备份 quick_check。

## 开发与构建

需要 Go 1.25 或更新版本。从仓库根目录执行下列命令。`go.mod` 与 `go.sum` 固定依赖。

```sh
make check
make build
./dist/messagehub version
```

`make check` 运行普通测试、race 和 vet。测试包含真实 Kratos HTTP、官方 MCP 客户端、真实 SQLite，以及实际编译的服务进程重启和强制中断首启。所有业务数据和凭据均为临时测试夹具，不需要模型或平台账号。

若 PATH 的 Go 过旧，可给 Make 显式传已安装的正确 `GO` 路径。`scripts/go` 是可选的固定镜像离线测试入口，必须提供已准备好的 `MESSAGEHUB_BUILD_CACHE`。该目录需要 `gomod`、`gocache`、`evidence` 子目录，cache/output 可供 uid 10001 写入。依赖下载阶段必须先校验 `go.sum`，隔离运行阶段才关闭联网与在线 sumdb 查询；这不意味着可以跳过依赖校验。

```sh
export MESSAGEHUB_BUILD_CACHE=/absolute/prepared-cache
bash scripts/go test ./... -count=1
bash scripts/go test -race ./... -count=1
bash scripts/go vet ./...
```

Dockerfile 使用固定摘要 Go 1.25.12 镜像。下载阶段校验锁文件，编译阶段禁网，最终 scratch 镜像只包含静态 Go 程序并以 10001:10001 运行。

```sh
make image
```

部署前阅读 [安全说明](SECURITY.md)。不要把实际配置、凭据或数据库放进镜像。

隔离验收与覆盖范围见 [验证入口](skills/verify-messagehub/SKILL.md)。

## 配置与启动

以 `configs/config.example.json` 为模板，在 Git 外保存实际配置。

- `database` 必须是新服务自己的绝对路径；不要指向旧 Bridge 或其他应用数据库。
- `public_url` 是实际 HTTPS issuer 的 origin，不能包含凭据、查询参数或业务路径。示例 `.invalid` 域名不是已部署地址。
- 默认监听 `127.0.0.1:8098`。容器内接 ingress 时可显式配置 `0.0.0.0:8098`，但不发布宿主公网端口。
- `allowed_hosts` 匹配完整 HTTP authority，非默认端口要包含端口。省略时包含 public authority 以及监听端口的 loopback authority。
- 默认正文 64 KiB、单文件 8 MiB、分块最多 64 KiB、逻辑内容额度 1 GiB、消息可读期 30 天、文件可读期 7 天。
- HTTP 默认请求体 1 MiB、30 秒处理预算、8 个并发槽，600 次/分钟是全局平均速率，不是逐参与者公平限额。
- 首版固定成员、不公开成员变更。会话关闭后允许成员读取已有记录，但拒绝新消息与上传。

```sh
./dist/messagehub serve --config /private/messagehub/config.json
```

数据库只存 token 摘要。数据库文件需要单一规范路径；拒绝符号链接、硬链接及非本应用数据。空的中断首启可以恢复。SQLite 要求回滚时，仅对可证明原始页数/记录数都为零、无有效 WAL 的 bootstrap journal 使用 immutable 只读检查，并再次确认空 schema 后交由 SQLite 恢复；不手工删除 journal，也不对外部数据库做可写探测。数据库及 WAL/SHM/journal 侧文件的符号链接和硬链接均拒绝。首次初始化和已提交 WAL 的识别不同，不能只依赖文件大小或未 checkpoint 的主文件头。

## 本地管理

管理命令不在 HTTP/MCP 中公开。运行者需要已授权的服务器文件访问。

```sh
./dist/messagehub admin --config /private/messagehub/config.json principal-add --id hermes --name Hermes
./dist/messagehub admin --config /private/messagehub/config.json principal-add --id assistant --name Assistant
./dist/messagehub admin --config /private/messagehub/config.json grant-create --principal hermes --output /private/messagehub/credentials/hermes.json
./dist/messagehub admin --config /private/messagehub/config.json grant-revoke --id GRANT_ID
./dist/messagehub admin --config /private/messagehub/config.json principal-disable --id assistant
./dist/messagehub admin --config /private/messagehub/config.json conversation-close --id CONVERSATION_ID
./dist/messagehub admin --config /private/messagehub/config.json backup --output /private/messagehub/backups/new-snapshot.db
./dist/messagehub admin --config /private/messagehub/config.json wake-status
```

凭据和备份输出目录须预先存在且权限为 0700。凭据只写入全新的 0600 文件，stdout 只返回非敏感回执。不接受已有输出文件，不把真实 token 放入命令行或日志。数据库和备份权限不等于加密。

停用身份可以再次启用；显式撤销 grant 不因身份重新启用而恢复。恢复旧备份可能恢复旧授权快照，生产恢复须另外核对撤销记录，不能把恢复当作凭据撤销的替代方案。

## HTTP 与 MCP 接口

所有通信接口都要求 `Authorization: Bearer`，包括 MCP 初始化和工具列表。不要把 token 放入 URL。MCP 入口为 `/mcp`；REST 统一使用 POST JSON，返回对应业务结果。

| MCP 工具 | REST 路径 | 主要输入 |
| --- | --- | --- |
| identity | /v1/identity | 空对象 |
| conversations_create | /v1/conversations/create | members、idempotency_key |
| conversations_list | /v1/conversations/list | 可选 cursor、limit |
| messages_send | /v1/messages/send | conversation_id、idempotency_key、text/data/attachment_ids、可选 reply_to |
| messages_read | /v1/messages/read | conversation_id、可选 reply_to、cursor、limit |
| messages_status | /v1/messages/status | message_id |
| inbox_pull | /v1/inbox/pull | 可选 cursor、limit |
| inbox_ack | /v1/inbox/ack | message_ids |
| attachments_begin | /v1/attachments/begin | conversation_id、idempotency_key、filename、media_type、size_bytes、sha256 |
| attachments_write | /v1/attachments/write | attachment_id、offset、content_base64 |
| attachments_commit | /v1/attachments/commit | attachment_id |
| attachments_read | /v1/attachments/read | message_id、attachment_id、offset |

`sender` 由服务端凭据确定，不是输入字段。未知字段、重复 JSON key、非法 UTF-8、错误参数类型、越界和跨会话引用均拒绝。

发送者必须包含在会话成员集合中。幂等键绑定发送者和会话；相同键不同内容冲突，新的键可以发送相同正文。`data` 是受大小限制的 JSON 对象。

消息与所有投递记录同事务提交。`stored` 仅表示服务持久化；`acked` 表示接收方声明接管，不表示模型处理完毕或用户已读。拉取和翻页不会自动 ACK。客户端应先本地持久保存和去重，再确认。

`messages_read` 加 `reply_to` 后，只返回该会话中直接回复原消息 ID 的未过期消息，按 sequence 升序。原消息存在但没有符合条件的回复时，`items` 是空数组。省略 `reply_to` 保持原历史查询。分页时沿用同一个 `reply_to`；不同原消息的游标不能互用。新版本启动时为已有数据库补建回复索引，不重写历史消息，也不改变 `user_version`。

已过期消息以无正文、无附件的墓碑返回，未确认投递显示 expired。过期不自动物理删除，历史仍计配额。当前文件同时保留完整 BLOB 与原分块，用于精确重试；逻辑配额不是磁盘物理容量上限。长期归档/清理另行设计，不隐式删除用户数据。

文件上传是有序追加。重复同块必须完全一致，错误 offset 或不同字节拒绝。commit 校验完整大小和 SHA-256，未完成或未绑定到消息的文件不可供接收者下载。服务不接受宿主机路径、不抓取 URL、不解压或执行文件。

## OAuth 接入

实现的是 public client + PKCE S256，不是身份提供商或网页登录产品。注册可携带 `application_type` 元数据，接受 `native` 或 `web`；省略时保持原有客户端兼容。该字段不授予身份或权限，不改变既有 redirect/resource 校验。其他未知字段仍拒绝。

1. 客户端通过发现元数据与 `/register` 登记 callback；这一步没有聊天权限。
2. 浏览器访问 `/authorize`，收到等待页、pending 请求 ID 和 HttpOnly/Secure cookie。普通 JSON 客户端仍收到原来的 JSON 结构与继续地址。
3. 管理员在本机查看精确客户端、redirect、resource，再批准到已有参与方。
4. 等待页自动访问同一请求的继续地址；批准后返回客户端 callback，客户端用 verifier 换取 access/refresh。页面不包含 state、challenge 或 cookie，也不会重新发起授权请求。

```sh
./dist/messagehub admin --config /private/messagehub/config.json oauth-pending
./dist/messagehub admin --config /private/messagehub/config.json oauth-approve --request REQUEST_ID --principal assistant
```

授权码一次使用，refresh 轮换。已消费 refresh 的摘要保留至授权到期，因此重复使用会持久撤销整个授权及其后继 token；用旧 refresh 调用撤销也能定位同一授权。客户端不得并发使用同一 refresh，收到不确定刷新结果不要盲重试，必要时重新授权。

每个 grant 最多保留 4096 次轮换历史、64 个未过期 access token。达到上限返回容量错误，不消费当前 refresh。到达轮换总数上限须重新授权；短时 access 数量上限可等待旧 access 过期。成功刷新会回收过期 access。授权码请求、注册客户端和HTTP入口另有容量与速率上限；不宣称已实现全部历史授权元数据回收。

OAuth 标准请求按 RFC 6749 将空参数视为未提供、忽略未知扩展；重复参数、非法编码和超过 8 KiB 的 query/form 仍拒绝。服务没有命名 scope 权限模型，非空 scope 返回 `invalid_scope`，不会静默扩大权限。public client 的空 `client_secret` 可省略，非空值拒绝。opaque state 原样保留，上限 8 KiB，并受整体查询长度限制；不得在客户端截断或改写它。

显式接受 HTML 的浏览器得到无脚本、no-store、no-referrer 的等待页；JSON 客户端契约不变。没有自动批准、网页批准接口或外部 SSO。未提供匿名 CORS preflight、客户端密码、device/client-credentials grant 或远程 metadata 抓取。真实 Grok/其他连接器的接入仍须独立验收，不能把协议测试等同于平台已接通。

## 隐私与部署边界

- 匿名者和非成员不能读取消息、成员、状态或文件。连接发现和 liveness 仅提供非敏感元数据。
- 所有路径先经过安全边界，包括框架路径重定向；REST/MCP 成功响应和错误响应都禁止存储缓存。
- 服务不记录正文、token 或 cookie；ingress 仍需独立避免记录授权码、凭据及敏感查询参数。
- HTTPS 由受控 ingress 终止。本实现信任服务器和被授权接收端，不是端到端加密，也不限制接收者平台对已取得内容的存储。
- 一名参与方默认只有一个接收器；Agent 侧持久去重、模型预算和循环保护需要其接入器落实。本服务不会启动模型，不能声称已实现 Agent 自动续跑。
- 网关不执行模型操作，也不自动导入其他应用的数据。真实客户端接入需要单独验证。

## 目录

- `cmd/messagehub/`：服务与本地管理命令。
- `internal/`：SQLite 存储、REST、MCP、OAuth、配置与管理实现。
- `configs/`：不含凭据的配置示例。
- `docs/`：收件长等待和可选 webhook 的操作说明。
- `skills/verify-messagehub/`：隔离验收入口与功能地图。

## 许可证

本发行版整体按 [GNU GPL v3.0（仅第 3 版）](LICENSE) 发布。保留的 MIT 许可文本与版权声明见 [来源及许可说明](NOTICE.md)。依赖保留各自的许可证。
