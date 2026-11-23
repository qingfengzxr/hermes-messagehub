---
name: verify-messagehub
description: "Use when verifying MessageHub CLI, REST, and MCP."
version: 0.1.0
author: qingfengzxr, Hermes Agent
license: GPL-3.0-only
platforms: [linux]
metadata:
  hermes:
    tags: [verification, go, mcp]
    related_skills: []
---

# Verify MessageHub

本技能是 MessageHub 的项目内验证入口，随源码维护。只驱动本次创建的隔离实例，不连接生产或真实 Agent。功能地图以 [features/README.md](features/README.md) 为索引；不要把旧验收报告当成本次运行结果。

## When to Use

- 改动 MessageHub 后验证实际 CLI、REST 和 MCP 行为。
- 核对功能地图是否遗漏新增入口或夸大已有能力。
- 维护本技能的驱动、证据和恢复步骤。
- 不用于生产切换、真实 Bot 派单、现有数据库维护或外部 OAuth 登录。

## Prerequisites

用 `terminal` 确认当前 Git 工作树、分支和变更，再将工作目录设为仓库根目录。此处的命令均相对该根目录，不依赖固定机器路径。

- Docker 可用，模块 `scripts/go` 使用的固定摘要 Go 镜像已在本机。
- `MESSAGEHUB_BUILD_CACHE` 指向已准备的绝对缓存目录，内有 `gomod`、`gocache`、`evidence`。输出目录可由 uid 10001 写入。
- 缓存与 `go.mod`、`go.sum` 相符。缺依赖时报告 BLOCKED，不自动联网补包、不升级依赖、不改宿主 Go。
- 复用产品的隔离入口，强制源码只读、CGO=0，容器无网络和生产凭据挂载。仅本次临时目录中的数据允许变更。

## Launch

先用 `terminal` 执行索引与源码入口检查。

```sh
bash skills/verify-messagehub/scripts/verify -mode check
```

然后运行完整地图。

```sh
bash skills/verify-messagehub/scripts/verify -mode run -feature all
```

驱动编译当前 `cmd/messagehub`，创建私有临时配置、数据库和测试身份，启动一个长期服务子进程。所有调用串行执行。恢复分区会先精确停止旧进程，再启动同库实例和备份恢复实例，绝不同时驱动共享数据库。

不要另开第二个服务配合驱动，不传入已有端口、配置、数据库或凭据。驱动没有连接外部实例的参数。

## Doctor

独立诊断通过以下命令执行，结束时自动清理。

```sh
bash skills/verify-messagehub/scripts/verify -mode doctor
```

它会创建隔离运行态，因此整个命令并非纯只读。实例启动后的 doctor 检查本身只读，要求进程仍存活、健康检查成功、身份正确，以及官方 MCP SDK 发现的工具集与源码注册表一致。

完整运行在每个分区前再次 doctor。出现意外失败时先复查健康，停止继续驱动未知状态。修复的是技能漂移时，只改本目录并重跑；发现产品回归时记录 ISSUE，不修改产品代码以让地图通过。

## Drive

完整维护必须使用 `-feature all`，不能用一个分区的 PASS 代替全图覆盖。定位问题时可以分别执行：

```sh
bash skills/verify-messagehub/scripts/verify -feature identity
bash skills/verify-messagehub/scripts/verify -feature messages
bash skills/verify-messagehub/scripts/verify -feature inbox
bash skills/verify-messagehub/scripts/verify -feature files
bash skills/verify-messagehub/scripts/verify -feature recovery
```

每次命令使用新的隔离运行态。一次完整运行共享一个当前实例，按地图顺序执行。预期的 401/404/409 等拒绝属于测试步骤；意外失败不能被记成“空结果”。MCP 还必须检查 JSON-RPC/SDK 错误和 tool `isError`。

## Evidence

输出位于容器 `/out/verify-messagehub/` 下本次唯一运行目录；宿主对应 `$MESSAGEHUB_BUILD_CACHE/evidence/verify-messagehub/`。以命令返回的精确目录为准，不选择“看起来最新”的旧结果。

报告记录本次模式、源码/二进制指纹、分区、步骤期望与实际观察、入口覆盖和清理结果。完整 PASS 要求五个分区都执行，源码注册的业务操作在 REST、MCP 均有成功证据，且清理成功。文件验证还保留合成原件、下载件及实际摘要。

证据只允许非敏感状态、对象 ID、数量、摘要和合成样本。不得归档 token、cookie、授权码、原始 OAuth 响应、凭据文件、数据库或含未知内容的服务日志。健康 200 不是身份和数据权限证明，HTTP 200 也不是 MCP 工具成功证明。

## Cleanup

驱动在成功、失败和受控中断后关闭自有 MCP session，仅对自己的服务进程发送 SIGTERM 并等待。必要的强杀与回收应计入结果，不按进程名批量杀进程。

清理只删除本次临时运行态，证据目录独立保留。返回后用 `read_file` 读取精确报告，并核对报告记录的进程回收、运行态删除、样本与报告仍存在。不能仅凭 CLI 退出码宣称清理完成。

## Helpers

`scripts/verify` 是可执行入口，复用模块的 `scripts/go`。`scripts/driver/` 是技能自有 Go 驱动，不导入产品内部存储包，不写 SQL、不添加产品测试后门。

用 `terminal` 在模块目录运行驱动自身的单元测试与静态检查：

```sh
bash scripts/go test ./skills/verify-messagehub/scripts/driver -count=1
bash scripts/go vet ./skills/verify-messagehub/scripts/driver
```

## Pitfalls

- 本地图没有声明网页 UI、真实 Agent 会话映射、自动唤醒或生产部署已经验证。
- OAuth 是 loopback 协议驱动，手动回送 Secure cookie 不等于真实浏览器在 HTTPS 下的行为已验收。
- 默认消息可读期为 30 天。实时驱动不等到期，不改系统时间；时间与容量边界见补充测试，不能冒充实时验证。
- 备份包含授权状态。旧快照可能恢复快照之后撤销的 token，不能把恢复成功写成跨旧快照撤销保持。
- 源码、动态工具发现与地图三者必须一致；新增入口不能静默跳过。

## Verification

维护结果分开报告源核对、实际驱动、补充单测和外部未验收项。每次地图修改后跑 check、doctor 和全图 run；驱动改动还要先 RED、后 GREEN。最后确认产品文件未改、精确证据仍在、没有遗留自有服务。

后续用 `/maintain-verification-skill` 指向本目录进行维护。本次源码依据与范围见 [references/source-index.md](references/source-index.md)，设计选择见 [references/design.md](references/design.md)。
