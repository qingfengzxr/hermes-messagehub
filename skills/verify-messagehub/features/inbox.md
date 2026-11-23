# 收件箱与投递确认

收件箱返回本人尚未 ACK 的投递。拉取、读历史和查状态不会自动确认；ACK 表示接收者声明接管，不代表业务处理完成。

## Sub-features

- inbox.pull：读取本人未确认项，包括仍需确认的过期墓碑。
- inbox.wait：REST 专用认证长等待，1..20000毫秒，不ACK。独立等待配额，保持撤销检查，返回与pull相同的收件页。
- inbox.page：按本人 delivery.sequence 分页，游标绑定身份和操作。
- inbox.ack：只能确认自己的投递，重复 ID 去重，重复 ACK 保留第一次时间。
- inbox.atomic：批次中任一项无权或不存在时整批回滚。
- inbox.visibility：发送者见全部收件回执，接收者仅见自己；ACK 不影响其他接收者。

## How to get to it (user POV)

| MCP tool | REST POST | 主要参数 |
| --- | --- | --- |
| inbox_pull | /v1/inbox/pull | cursor、limit 可选 |
| 无，后台接收器专用 | /v1/inbox/wait | cursor、limit 可选，wait_ms 必填 |
| inbox_ack | /v1/inbox/ack | message_ids |
| messages_status | /v1/messages/status | message_id |

通过 `/mcp` 使用工具或通过对应 REST 请求，均携当前测试身份凭据。

## Driving it with verify-messagehub

Preconditions: 有新的隔离会话和多份可区分投递，结果不受其他消费者干扰。ID 和游标取实际响应。

通过 `terminal` 从仓库根目录执行：

```sh
bash skills/verify-messagehub/scripts/verify -feature inbox
```

- 两协议拉取。创建待确认消息，REST/MCP 拉取相同记录，再查 status，仍为 pending。
- 状态可见域。群消息发送者见各收件者，接收者只见本人。发送者代 ACK 的负例另见补充存储层测试。
- 失败批次。使用合法消息加格式有效但不存在的 ID，确保合法 ID 排在失败 ID 前；请求必须失败，随后 status 仍 pending，无 cursor 拉取仍在。没有排序证据就不能声称测到了先更新后回滚。
- 幂等确认。REST ACK 重复 ID 得到去重回执，MCP 重复 ACK，acked_at 不变，其他接收者仍 pending。
- 确认后的读取。从首页重新拉取确认本人记录移除，历史消息仍在；不要用旧 cursor 看不见来冒充 ACK 生效。
- 长等待不在上述串行双协议驱动内。在仓库根目录运行 `bash scripts/go test -race ./internal/hub ./internal/server -run 'Test.*Wait' -count=1`，检查真实 HTTP 空等待、提交唤醒、配额、撤销和不提前 ACK。主驱动通过不代表长等待测试或外部接收器验证通过。

## Gotchas

- pending 的产品文案不代表 inbox 每项都处于 pending，过期未 ACK 的项仍可出现。
- delivery.sequence 与 conversation message.sequence 是不同序列，不能混用排序口径。
- 实现先按 ID 排序再更新，因此一次失败批次可能只是第一项提前拒绝。验证回滚必须保证有效项先被处理。
- ACK 不是删除消息，更不是 Agent 自动处理。没有接收器执行证据不能声称业务完成。
- 默认 TTL 不在本轮等待。墓碑、跨重启游标及过期 ACK 使用 source-index 中受控时钟测试，作为独立证据层报告。
