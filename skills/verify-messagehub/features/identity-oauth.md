# 身份与 OAuth

参与方用自己的授权访问通信数据。登记 OAuth client 只登记应用元数据，不创建参与方；本地管理员明确批准后才绑定身份。

## Sub-features

- identity.self：REST/MCP 返回当前凭据对应的身份和可发现的活跃团队身份，不接受请求自选 sender。
- identity.local：本地创建、停用、重新启用参与方，签发或撤销 grant。停用不是永久撤销。
- oauth.discovery：客户端读取授权服务器和 protected-resource 元数据。
- oauth.approval：DCR、S256、cookie、精确 redirect/resource、本地批准和单次 code。
- oauth.rotation：refresh 正常轮换，重放已消费 token 撤销完整 grant；旧 refresh 可用于显式撤销。

## How to get to it (user POV)

- 业务入口 `identity` 和 `POST /v1/identity`；MCP 位于 `/mcp`，初始化和工具发现也要凭据。
- CLI `principal-add`、`principal-disable`、`principal-enable`、`grant-create`、`grant-revoke`、`oauth-pending`、`oauth-approve`。统一前缀为本次程序的 `admin --config`，不公开 HTTP 管理接口。
- GET `/.well-known/oauth-authorization-server`、`/.well-known/oauth-protected-resource`、`/.well-known/oauth-protected-resource/mcp`。
- POST `/register` 使用 JSON。GET `/authorize`、`/authorize/continue`；POST `/token`、`/revoke` 使用 form。

## Driving it with verify-messagehub

Preconditions: 主技能创建新的隔离身份与实例，doctor 可用。凭据不进入报告，不使用真实 callback。

命令通过 `terminal` 从仓库根目录执行：

```sh
bash skills/verify-messagehub/scripts/verify -feature identity
```

- 自读与拒绝。使用 REST 和官方 MCP SDK 获取本人的 principal；匿名两入口均拒绝。
- 本地生命周期。对独立 probe 身份依次停用、启用、撤销，观察 401、恢复成功、再次 401。不要撤销后续 doctor 使用的身份。
- 登记与批准。register 返回 client_id 而非 principal/secret；authorize 返回 pending 和浏览器 cookie；缺 cookie 拒绝、未批准仍 pending。通过真实 CLI 读取请求并批准到已有身份。补充回归 `TestOAuthHTTPRegistrationApplicationType` 覆盖 Hermes 原生注册元数据、字段省略、无效类型、重复字段及伪造身份字段；真实平台接入仍需另行验收。
- 参数与浏览器。真实驱动使用 native 注册元数据、长 opaque state、空 scope 与扩展参数；同 cookie 的 HTML 继续页等待本地批准，不输出 state/cookie。补充 `oauth_compatibility_test.go`、`oauth_state_test.go`、`oauth_browser_test.go` 覆盖重复参数、非空 scope 拒绝、体积与 JSON/HTML 协商。
- 交换与使用。手动携原 cookie 继续，收到 303 但不跟随；校验完整 state，以正确 verifier 和空 client_secret/scope 兑换 code。重复 code 拒绝；新 access 经 REST 和 MCP 自读成功。
- 轮换与重放。刷新成功后重放旧 refresh，随后明确断言后继 access 为 401、后继 refresh 为 400。不能只打印 access 状态却不检查。
- 单独撤销。建立另一个有效家族、轮换，用旧 refresh 调用 revoke，再验证后继 access 与 refresh 均不能用。不能复用已经因重放而撤销的家族制造假通过。

## Gotchas

- 没有网页批准接口或密码登录。审批只有本地命令，DCR 不授予聊天权限。
- 只支持 public client、PKCE S256；没有命名 scope 权限模型，非空 scope 返回 invalid_scope；没有客户端密码或 password grant。核心参数须与元数据精确绑定，未知 OAuth 扩展不会被用于身份。
- 本地 HTTP 驱动手动回送 Secure cookie，只证明协议路径，不证明浏览器 HTTPS cookie 或真实平台回调。
- 正常轮换不立即撤销旧 access；重放撤销整个家族是另一种行为。不能并发重用 refresh，结果不确定时不盲重试。
- refresh 历史上限、活跃 access 上限、请求及 client 容量有独立单测，不从本轮少量授权推断上限行为已实时覆盖。
- revoke 对未知 token 返回 200；必须读回凭据失效才能证明撤销效果。
