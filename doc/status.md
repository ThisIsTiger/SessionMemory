# 实施状态

> 日期：2026-10-26  
> 依据：[spec.md](spec.md)（实现规格）、[dev.md](dev.md)（开发计划）、[设计文档](unified-agent-session-sync-design.md)（v0.6）

本文记录**代码已经做到哪里、与规格有哪些偏差、什么还不能做以及为什么**。规格仍是规范性文档；这里的偏差中，已影响格式的都已回写 spec。

## 1. 已实现并有测试

| 里程碑 | 内容 | 位置 |
|---|---|---|
| M0 | 命令行骨架（`version`、`config`、`agent probe`）、`errcode`/`logx`（含 lint 测试）/`failpoint`、CI 的 failpoint 任务 | `cmd/session-memory`、`internal/{errcode,logx,failpoint}` |
| M2 | zstd/plain `jsonl.Source`、合成夹具生成器、Codex 发现/探测/采集（孪生、分段、归档、压缩、半行、损坏行、同长度改写）、多 `CODEX_HOME` 配置、流式采集内存断言 | `internal/{jsonl,testutil/codexgen,source/codex,streamcap,config}` |
| M3 | 对象库、journal 与恢复、binding、`sessions.db`（含重建）、采集事务（崩溃矩阵）、原生恢复事务（备份/回滚，崩溃矩阵）、双设备回环 | `internal/{store,engine,sync}` |
| M4 | vault 加解密与加入包、凭据存储接口（系统钥匙串）、FolderTransport、发布/接收算法、隔离与 `device_id` 克隆检测、发布崩溃矩阵 | `internal/{vault,transport,sync,device}` |
| M5 | `init`、`transport attach-folder`、`device invite/join/regenerate-id`、`discover`、`track`、`list`、`status`、`checkpoint`、`sync [--watch] [--auto-install]`、`revisions`、`choose`、`project map`、`restore [--plan]`、`open`、`verify [--deep]`、`auto-install`；冲突检测与落地 | `cmd/session-memory`、`internal/session` |
| M6（读取侧与规则） | WorkBuddy 适配器（发现含软删除、采集含旁路文件与 blob、探测）；Canonical 提取（Codex 主流/链接、WorkBuddy 活动路径）；`convert.Convert` 全部规则与保真度报告；`convert --plan`、`fidelity`、`continue` | `internal/{source/workbuddy,convert,canonical}` |
| M7（兜底） | Resume Pack（确定性，不依赖 LLM）、`resume-pack` | `internal/bridge` |
| P0 工具 | `dev sandbox/inventory/scrub/codex write/codex append/codex locks/report`（`-tags dev`，发布构建没有），沙箱安全守卫 | `internal/p0`、`cmd/session-memory/dev_*.go` |

端到端测试覆盖：两台“电脑”（独立状态目录、独立凭据存储、共享目录）上的 init → 纳管 → 同步 → 加入 → 接收 → 原生恢复 → 继续写 → 回传 → 快进，含路径映射、篡改/错误 vault/缺对象/序号缺口、克隆检测、conflict/需要处理、运行状态未知时拒绝安装。

## 2. 与规格的偏差

已回写 spec 的：

- `device.json` 增加 `vault_id`；新增 `device/outbox.json`（等待发布的 receipt/choice/convert-request）；新增 `store/revisions/<revision_id>.json`（本机已提交 revision 的事实源，`sessions.db` 重建需要它）。
- 安装事务的 `verified` 状态含义收紧为“文件已验证**且**待提交的 binding 已准备好”（崩溃矩阵测试发现旧定义无法前滚）。

仅记录（行为细节，不改变格式）：

- 对象先写成最终名（临时文件 + rename，内容寻址、幂等），采集事务的 `staged` 与 `stored` 两个状态因此实际上紧挨着；崩溃矩阵对两个状态点都验证。
- 故障注入测试用 `error` 动作而不是真的 `crash`（进程内断言恢复结果）；点位与 spec §13.2 的命名一致。
- Codex Writer 按 spec §8.3 的**默认行为**实现（只写会话文件、不碰数据库、仅未压缩 `.jsonl`），**未在真实客户端上验证**——这正是 P0 的 E1a/E1b。因运行检测目前保守地返回 `Unknown`，安装需要用户显式 `--client-closed`；Codex 持有线程写锁时即使带该参数也拒绝。
- 转换写入目标 Agent（`convert` 不带 `--plan`）返回 `unsupported`（退出码 5），WorkBuddy 的 Writer 同理，直到 E3/E4 决定写入路线。
- 工具名映射（`engine.ToolMaps`）是临时值，E3/E4 修正。

## 3. 未完成及原因

| 项 | 原因 |
|---|---|
| Codex 真实客户端上的显示/继续/重开验证 | 需要你在隔离环境里操作 Codex Desktop（P0 E1a/E1b/E1c/E2a/E2b） |
| WorkBuddy Writer、转换写入（Codex↔WorkBuddy 的完整历史出现在目标里） | 依赖 E3/E4/E5/E6；WorkBuddy 没有外部写入先例 |
| 客户端运行检测的最终信号（进程枚举、应用包路径） | 依赖 E2a；`platform` 的进程枚举（macOS/Windows）未实现 |
| 后台常驻（登录项注册、钥匙串解锁策略） | 设计 §6.4 第 6 点，M5 的系统集成部分 |
| `mcp serve`（兜底路径的只读 MCP） | M7，低优先级；Resume Pack 文件已可用 |
| Claude Code 适配 | P3；`internal/source/claude`、`checkpoint`、`conversation` 保留冻结（dev §7） |
| 微云真实行为验证 | E7，需要在真实微云同步目录上跑 |
| `doc/acceptance.md` 人工验收清单 | 随 P0 报告产出 |

## 4. 你接下来可以做什么：开始 P0

```bash
# 1. 构建带实验工具的二进制（发布构建没有 dev 命令）
go build -tags dev -o /tmp/sm-dev ./cmd/session-memory

# 2. 只读结构清单（不要求沙箱；不读任何对话内容、认证文件）
/tmp/sm-dev dev inventory --agent codex --home-dir ~/.codex --out /tmp/codex-inventory.json
/tmp/sm-dev dev inventory --agent workbuddy --home-dir ~/.workbuddy --out /tmp/wb-inventory.json

# 3. 创建隔离沙箱，并按 env.sh 指向它启动客户端
/tmp/sm-dev dev sandbox new e1 --agent codex
source "$HOME/.session-memory/dev/sandboxes/e1/env.sh"      # 然后从这个终端启动 Codex

# 4. E1a：把一个脱敏 rollout 作为新线程放进沙箱，看 Codex 是否列出它
/tmp/sm-dev dev scrub --in <真实 rollout> --out /tmp/fixture.jsonl
/tmp/sm-dev dev codex write --sandbox e1 --rollout /tmp/fixture.jsonl

# 5. E2a：探测线程写锁（打开/关闭线程时分别运行）
/tmp/sm-dev dev codex locks --home-dir "$CODEX_HOME"

# 6. 为每个实验建报告模板
/tmp/sm-dev dev report new E1a
```

实验的通过标准、失败处理见设计 §15 与 dev §9。结论回写后，对应的 Writer 默认行为（spec §17）才可确认或调整。

## 5. 验证记录

见本文末尾“验证结果”一节（由最近一次全量运行填写）。

## 验证结果

最近一次全量运行（2026-10-26）：

- `gofmt -l`：无输出；
- `go vet ./...`，以及 `-tags failpoint`、`-tags dev`，并对 `GOOS=linux`、`GOOS=windows` 交叉 vet：通过（Windows 上的测试本身未在 Windows 实机运行，只在 CI 三平台矩阵里跑）；
- `go test ./...`、`go test -race ./internal/...`、`go test -tags failpoint ./internal/...`、`go test -tags dev ./cmd/... ./internal/p0/...`：全部通过；
- 崩溃矩阵：采集 10 个点、发布 9 个点、安装 11 个点，逐点注入失败 + 恢复后断言；
- 大会话采集：144 MiB 的合成会话，峰值堆增长 26 MiB。
