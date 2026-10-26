# 跨 Agent 转换写入验证（命令行 / app-server 级）

> 日期：2026-10-27。macOS arm64，Codex CLI `0.162.1`。设计见 [spec §9.3](spec.md)。

## 验证了什么

把一个 **WorkBuddy 来源**的会话（合成：用户 `SM_P0_ALPHA …`，助手 `SM_P0_BETA noted`）经 `engine.ConvertRestore` 写成**新的 Codex 原生线程**，放进隔离的 `CODEX_HOME`（`config.toml` 指向本地桩模型服务，不使用任何账号），再用真实的 Codex 去读、继续：

| 检查 | 结果 |
|---|---|
| 运行中的 `codex app-server`：`thread/list` | 立即出现新线程（无需重启） |
| `thread/resume` 后 `thread/turns/list` / `thread/read(includeTurns)` | **两个 turn**：转换导入的 `用户 ALPHA / 助手 BETA` 与之后继续的 `用户 GAMMA / 助手 ACK_GAMMA`，items 完整；`preview` 为首条用户消息 |
| `codex exec resume <新线程> "SM_P0_GAMMA …"` | 成功。桩服务看到的请求同时含 `ALPHA`、`BETA`、`GAMMA`：模型上下文里有转换导入的完整历史 |
| 继续之后 `checkpoint` | 只产生一个新 revision，`ParentRevisionID` = 来源 revision、`AgentProfile.Agent = codex`；导入部分没有被当成新记录 |

## 过程中发现并已修正

1. **只写旧式 `user_message` / `agent_message` 事件，Codex 0.162.1 的 turn 是空的**：`thread/read` 里 turn 没有 items（模型能看到历史，界面看不到）。真实会话用 `event_msg item_completed`（`UserMessage` 的 `content:[{type:"text"}]`、`AgentMessage` 的 `content:[{type:"Text"}]`）。序列化改为写 `item_completed`，修正后两个 turn 都完整。这是 spec §9.3 写明 Codex 序列化必须带 UI 层记录的依据。
2. 安装事务恢复前滚时，补发的 receipt 的 `install_mode` 原来固定为 native，现取 binding 的安装方式。
3. 外来工具的参数形状没有验证，原先临时的 `Bash → exec_command` 映射会写出形状不对的原生工具调用。`ToolMaps` 现对两个目标均为空，工具调用/结果渲染成文本（保证历史可读）。

## 自动化测试

- 序列化往返：Codex 与 WorkBuddy 各自的 Serializer 输出，经同一适配器 `Extract` 读回，人类消息文本、顺序、角色不变；记录合法 JSON、`ordinal` 连续、turn 配对；Codex 原生 Writer 接受它作为新线程。
- 引擎往返（`TestCrossAgentRoundTrip`）：Codex → WorkBuddy（含登记行、binding `converted`、导入回执边界 = 文件长度与行数）→ 同一 revision 不会转换两次 → 立即 `checkpoint` 不产生 revision → 在 WorkBuddy 继续 → 新 revision 接在来源 revision 之后 → 转回 Codex 得到**新**线程且包含全部历史；旧 Codex 绑定变 `superseded`、恰有一个 active；原 Codex 线程文件逐字节未动。
- 命令行（`TestCrossAgentPlanAndResumePack`）：缺本地项目目录拒绝；`--plan` 不写文件并列出将创建的文件；真实执行创建线程；重复转换同一 revision 被拒。
- 崩溃矩阵（`-tags failpoint`）：转换安装 14 个点（各状态前后、`mid_write`、`after_registry`）；转回并 supersede 13 个点。`verified` 之前全部撤销（无文件、无登记行、无 binding），之后前滚到完整的 converted binding 且只有一个 active。

## 没有验证（不要当作已支持）

- Codex Desktop **图形界面**里转换会话的显示、继续与重开；WorkBuddy 图形界面里**转换会话**的显示与真实模型续写（只验证了 WorkBuddy 目标的文件/登记/回采与 Codex 目标的真实客户端）。
- 原生工具调用记录（外来工具映射）、附件、子代理、compaction 的原生边界、编辑分支：转换时按文本渲染或丢弃并计入保真度报告。
- 跨账号（E1c）、Windows/Linux 实机、两台物理电脑。

## 复现

门控测试 `TestLiveConvertToCodex`（`internal/source/workbuddy/convert_live_test.go`，`SM_LIVE_OUT` 与 `SM_LIVE_PHASE=convert|capture` 控制）生成并回采；外部脚本在 `$SM_HOME/dev/sandboxes/convert-live-*/` 起本地桩模型服务、写隔离 `CODEX_HOME/config.toml`、驱动 `codex app-server` 与 `codex exec resume`。桩服务按请求里出现的标记回答，请求不含任何凭据。
