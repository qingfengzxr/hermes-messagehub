# 原文件传输

文件经 begin、顺序 write、commit 后，通过消息绑定才可由会话成员读取。传输的是原字节，不抓取 URL、不解压、不执行文件。

## Sub-features

- files.begin：声明文件名、媒体类型、大小和小写 SHA-256，按声明大小预留逻辑额度。
- files.write：按解码后的字节 offset 追加，完全相同的原块可重试，不接受不同字节覆盖。
- files.commit：收齐后校验完整长度和摘要，ready 可重复提交。
- files.link：通过 messages_send 的 attachment_ids 绑定到真实消息，不通过内部 setter 或 SQL。
- files.read：接收者按 next_offset 逐块取得原件，校验全字节、大小和摘要。
- files.privacy：未绑定、未完成、错消息关联和非成员不能下载；staging 操作只允许上传者。

## How to get to it (user POV)

| MCP tool | REST POST |
| --- | --- |
| attachments_begin | /v1/attachments/begin |
| attachments_write | /v1/attachments/write |
| attachments_commit | /v1/attachments/commit |
| attachments_read | /v1/attachments/read |

绑定文件使用 `messages_send` 或 `POST /v1/messages/send`。不存在匿名下载 URL。

## Driving it with verify-messagehub

Preconditions: 隔离 AB 会话、非成员 carol、可用的 identity 限额。只使用驱动生成的合成样本，不读用户文件。

通过 `terminal` 从仓库根目录执行：

```sh
bash skills/verify-messagehub/scripts/verify -feature files
```

- 两条完整路径。REST 和 MCP 各执行一遍 begin/write/commit/send/read，不能用一条混合 happy path 代替四工具的双入口覆盖。
- 上传。合成包含零字节和非 UTF-8 的多块样本，实际计算大小和 SHA-256。检查声明回显、累计 received_bytes，以及原块精确重试不增加计数。
- 拒绝。错误 offset、不同内容重试、提前 commit 必须失败。失败后按正确 offset 继续，不重新生成逻辑上传冒充恢复。
- 绑定。ready 尚未绑定到消息时拒绝读取。调用公开 messages_send 建立链接后，bob 才能读取。
- 下载。按 next_offset 分块，逐块 Base64 解码后拼字节。对完整下载件校验大小、全字节和 SHA-256；保留原样本和下载样本作证据。
- 权限。carol、匿名或错误 message/file 配对拒绝；另一个协议读取同一已绑定文件应得到相同内容。

## Gotchas

- offset 是原字节位置，不是 Base64 字符数。不得拼接带 padding 的 Base64 再一次解码。
- 原块的同 offset、同长度、同字节才是重试。短前缀或重叠不是可接受的断点续传。
- commit 并不开放文件；必须有正确的消息绑定和成员身份。
- 下载时服务不重新计算整文件摘要，客户端必须校验重组结果。
- 文件 TTL 从 begin 起算，不因重试续期；消息或文件任一过期都可能拒绝读取。时间边界在补充测试层验证。
- 逻辑配额包括未完成和过期保留记录，不等于 SQLite/WAL 物理磁盘上限；当前还保存完整 BLOB 与分块。
