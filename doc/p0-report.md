# P0 实验报告（第一轮，命令行 / app-server 级）

后续进展见 [第二轮真实客户端测试](p0-live-report.md)：已补 Codex CLI 往返、WorkBuddy GUI / 真实模型与登记路线实验。本文保留第一轮的历史结论和当时未完成项。

> 日期：2026-10-26  
> 环境：开发机 macOS（arm64），Codex CLI `0.162.1`，Codex Desktop（ChatGPT.app 内的 Codex Framework 155/156）；全部写入实验在 `$SM_HOME/dev/sandboxes/` 下的隔离 `CODEX_HOME` 中进行，用 `codex app-server`（stdio JSON-RPC）作为被测的“客户端”。本机的日常 `~/.codex` 只做了只读结构清单和只读的锁探测。  
> 夹具：从本机一个 465 KB 的真实 rollout 经 `dev scrub` 脱敏得到，再把 `cwd`、`runtime_workspace_roots` 改成合法路径。

## 范围与局限

**已验证的层面**：Codex 自己的 app-server（Desktop 和 CLI 共用的后端）如何看待我们放置的文件。**未验证**：Desktop 图形界面里会话是否出现在侧边栏、点开显示效果、真实模型继续对话（需要登录凭据，实验环境没有，也不应该复制）。这些仍需你在 Desktop 里肉眼确认一次。

## E1a Codex 新线程只放文件：通过（app-server 级）

- 在空的沙箱 home 里只放一个 `rollout-*.jsonl`（无任何数据库写入）后，`thread/list` 立刻返回该线程，Codex 自己创建了 `state_5.sqlite` 的登记；`thread/read` 成功。
- `thread/resume`（`excludeTurns:false`）之后，Codex 自己生成了 `thread_history_1.sqlite` 的分页投影（`thread_turns`、`thread_items`），之后的 `thread/turns/list` 返回完整的 turns 与 items，且重启 app-server 后仍在。
- **结论：路线 2（只写会话文件，客户端自己登记并投影）在 app-server 级可行，不需要路线 3。**
- 新发现（必须写进 Writer）：
  1. `session_meta` 的 `history_mode`（本机均为 `paginated`）是枚举，写成别的值会被拒绝读取（`unknown variant`）；
  2. `session_meta.payload.runtime_workspace_roots` 必须是本机合法的绝对路径，否则 `thread/resume` 报 “cannot restore workspace root”。**路径映射必须同时改 `cwd` 和 `runtime_workspace_roots`**（已实现并有测试）；
  3. `thread/list` 默认只列“交互”来源，并带有“扫描并修复元数据”的行为（`useStateDbOnly` 默认关）；
  4. Desktop 另有自己的缓存 `sqlite/codex-dev.db` 的 `local_thread_catalog`（本机 116 行），未验证 Desktop 是否自动扫描补齐（应在 Desktop 里确认一次）。

## E1b Codex 对已登记线程原地追加：通过（app-server 级）

- 先放前 50 行并 `resume`（投影位置 `43931 / ordinal 51`），再用 Codex 的发布协议（同目录临时文件 + rename）把文件替换成完整版本：`thread/turns/list` 本身**不会**重新投影，但再次 `thread/resume` 后投影推进到 `86210 / ordinal 99`，items 增加。
- **结论：原地追加后，客户端在下一次加载线程（resume）时自行补齐投影，不需要我们碰数据库。**
- 未测：路径映射改变字节长度的情形（投影里保存字节偏移）、安装后被 Codex 压缩为 `.zst`、目标已有 `.zst`。这些保持规格里的保守处理（拒绝 / 需要处理）。

## E2a 运行检测：已定（线程写锁）

- 本机 189 个线程，`~/.codex/thread-writer-locks/` 里只有 4 个线程锁文件，且全部被持有（对应正在打开的 4 个线程）；协调锁空闲。说明**锁文件只为“已加载”的线程存在，加载期间被持有**。
- **规则**：锁被持有 → `Running`；锁文件不存在或空闲 → 没有 Codex 进程加载该线程 → `NotRunning`；没有 `thread-writer-locks` 目录（旧版 Codex）→ `Unknown`。已实现。
- 影响：自动安装对 Codex 现在可以触发（只要目标线程未被加载）；用户不再需要 `--client-closed`（仍保留作为对 `Unknown` 的显式确认）。
- 未测：窗口已关但后台进程仍加载线程时的锁状态；Windows。

## E2b 运行中可见性：通过（app-server 级）

- 一个**正在运行**的 app-server，在其运行期间放入新 rollout 后，同一进程里的 `thread/list` 立即看到它（0 → 1），`thread/read` 成功。**不需要重启客户端。**
- 未测：Desktop 图形界面的侧边栏是否自动刷新（需肉眼确认）。

## 端到端（我们自己的流水线）：通过

合成并脱敏的真实 rollout → `track` → 加密发布 → 另一状态目录接收 → `restore`（路径规则 `/tmp/p0proj → /tmp/p0mapped`，映射了 4 处 `cwd`，历史文本中的旧路径原样保留）→ 对结果目录运行真实 `codex app-server`：`thread/list`、`thread/resume`、`thread/turns/list` 全部成功。

## 未做的实验

| 实验 | 状态 | 原因 |
|---|---|---|
| E1c 跨账号、子线程 | 未做 | 需要第二个登录账号；实验环境不应复制凭据 |
| E3/E4/E5/E6 WorkBuddy 与转换写入 | 未做 | 需要操作 WorkBuddy 图形界面，且其写入路线（登记行）需要隔离的 WorkBuddy 配置目录；WorkBuddy 无命令行接口可替代 |
| E7 微云 | 未做 | 需要真实微云同步目录 |
| E8/E9 | 未做 | 依赖上述 |
| Desktop 图形界面确认（E1a/E2b 的最后一步） | 待你确认 | 无法在不使用你的账号的前提下自动化 |
