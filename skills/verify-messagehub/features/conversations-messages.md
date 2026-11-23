# 私有会话与消息

参与者在固定成员会话中交换文本、JSON 和回复。发送成功表示服务存储成功，不表示对方已读或 Agent 已处理。

## Sub-features

- conversations.create：成员包含创建者，初始成员存在且 active，集合排序后参与幂等判断。
- conversations.list：仅列出自己所属的会话，包括关闭的会话。
- messages.send：身份决定发送者，文本、JSON 对象和附件引用至少有一种。
- messages.retry：发送者、会话和幂等键共同定位一次逻辑发送，内容冲突不得覆盖。
- messages.history：按会话 sequence 升序读取，reply_to 关联同会话消息，JSON 大整数保真。
- messages.visibility：发送者查看全部收件回执，其他成员只看自己，非成员拒绝。
- conversations.close：本地关闭阻止新发送，不删除历史。

## How to get to it (user POV)

| MCP tool | REST POST |
| --- | --- |
| conversations_create | /v1/conversations/create |
| conversations_list | /v1/conversations/list |
| messages_send | /v1/messages/send |
| messages_read | /v1/messages/read |
| messages_status | /v1/messages/status |

关闭使用真实程序的本地 `admin --config` 加 `conversation-close`。没有公开 close 工具。

## Driving it with verify-messagehub

Preconditions: alice/bob 可通信，carol 是本次 AB 会话的非成员；每个 ID 都来自实际响应。

通过 `terminal` 从仓库根目录执行：

```sh
bash skills/verify-messagehub/scripts/verify -feature messages
```

- 两协议创建与列表。REST 创建 AB，MCP 用同键和换序成员重试得到相同 ID；另一新键由 MCP 创建。两协议列表都读到本人会话。
- 内容与幂等。发送文本及含 `9007199254740993` 的 JSON 对象；另一协议重试同请求，ID/sequence 不变。相同键改内容拒绝，历史仍只有原逻辑消息。
- 回复与分页。bob 引用真实 message_id 回复，两协议读取历史，核对文本、原数值和 reply_to。分页以 has_more 判定尾页；完整排序边界另见补充测试。
- 隐私。非成员读取和状态查询被拒绝，匿名失败，传 sender 的请求被拒绝。消息幂等冲突后的历史数量在前面的内容步骤中读回验证。
- 关闭。关闭专用会话后历史仍可读，新键发送拒绝；原键和原内容重试仍返回已有消息。关闭回执的 closed 为 true。

## Gotchas

- 会话幂等作用域是创建者加 key，消息幂等作用域是发送者加会话加 key。不要把两个层次混成全局去重。
- 成员和附件顺序有规范化，不承诺任意 JSON 键序都视为同一幂等内容。重试保留原请求。
- 末页也可能带 next_cursor；不能以 cursor 非空断言还有数据。
- 读取历史不 ACK，status 返回当前回执而非初次发送时的静态副本。
- 关闭检查在已存在消息的幂等命中之后。拒绝新发送不意味着拒绝所有旧请求重试。
- 过期墓碑与全部参数边界由 source-index 中的补充测试覆盖；不把默认 30 天 TTL 写成这次实时驱动已等待验证。
