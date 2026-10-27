# 跨 Agent 转换写入与真实客户端往返验证

> 日期：2026-10-27。macOS arm64，Codex CLI `0.162.1`、WorkBuddy `5.8.3`。设计见 [spec §9.3](spec.md)。

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

## 第四轮：主 CLI 的 Codex → WorkBuddy → Codex 往返（2026-10-27）

本轮使用正式构建、同一 Mac 上两套独立状态目录与系统钥匙串、共享加密目录。全部原生写入位于 `~/.session-memory/dev/sandboxes/live-20261026-pw1nxsm0/`，WorkBuddy 由用户在隔离窗口登录；没有复制账号凭据。WorkBuddy 使用真实 GLM-5.3-Flash，Codex 使用真实 CLI / app-server 与本地受控 Responses 服务，后者验证的是客户端上下文传递。

| 步骤 | 实测结果 |
|---|---|
| Codex 原生三轮 → 同步 → WorkBuddy 转换安装 | 新会话出现在侧边栏；GUI 展示完整三轮及来源的昨日时间 |
| WorkBuddy 第四轮真实模型续写 | 正确列出 `ACK_ALPHA`、`ACK_BETA`、`ACK_GAMMA`，另答 `ACK_CROSS_WB_OK`；未调用工具 |
| 退出并重开隔离 WorkBuddy | 同一会话完整保留四轮及时间，无需重新导入 |
| WorkBuddy `sync` → 源端 `sync` → 转回 Codex | 无显式 checkpoint，自动捕获 1 个 WorkBuddy 子 revision，父版本等于转换来源版本 |
| Codex app-server 加载转换线程 | 四轮历史完整，包含 WorkBuddy 的真实模型回答 |
| Codex CLI 第五轮续写、app-server 重开 | 模型请求含全部历史标记；回答 `ACK_CROSS_BACK_OK`，重开后完整五轮；本地请求无 Authorization |
| Codex `sync` 回采、另一端接收、Resume Pack | 自动捕获 1 个 Codex 子 revision，父版本为 WorkBuddy 续写版本；导出完整五轮 |
| 防重复与运行保护 | 同一来源版本重复转换拒绝；WorkBuddy 运行时即使声明关闭仍拒绝安装 |

另用真实 Codex CLI / app-server 验证了包含字面 `<user_query><b>…</b></user_query>` 的用户问题：转换、续写、回采和 Resume Pack 均保留用户正文，历史时间保留为 `2026-10-26T07:33:20.000Z`。

本轮补齐的实现与回归：

- `sync` 同时采集 native 与 converted binding；同一逻辑会话有多个 active Agent 时，单次 `checkpoint` 需 `--agent`，避免随机选中旧客户端。
- 替换旧目标绑定前，以只读 Capture 检查未采集数据、半行、Unsafe 与运行状态；安装锁内和提交绑定前再次检查，拒绝后只撤销新安装，保留旧文件和 active binding。回归覆盖 Verify 期间旧会话追加数据的竞争情形。
- 两端独立转换会生成不同原生 ID；同 Agent 恢复这类后续版本时，创建匹配快照的新身份并 supersede 旧绑定，保留本地 cwd、旧文件和旧登记行，防止 ID 与游标错配。
- 历史消息时间不再被导入时间覆盖；Codex `item_completed` 的 UserMessage/AgentMessage 与模型记录关联，`Text` 正确提取。仅有匹配的明确用户 UI 记录才提升正文的人类消息身份，孤立注入上下文仍过滤；Codex AdapterVersion 为 0.1.1。
- Codex provider 使用 `go-toml/v2` v2.4.3 解析目标 TOML 与选中 profile，覆盖根配置；配置读取/解析失败拒绝转换，诊断不包含配置正文。两个 Serializer 版本均为 0.2。

## 第五轮：Codex Desktop（ChatGPT.app）自带后端（2026-10-27）

Desktop 为 `ChatGPT.app`，自带 Codex `0.164.0-alpha.11.1`（早于本报告其余部分验证的 CLI `0.162.1`）。在隔离 `CODEX_HOME` + 本地受控 Responses 服务下，用该内置二进制的 `app-server` 验证新转换的会话：

| 步骤 | 结果 |
|---|---|
| `thread/list` | 转换线程出现在列表中；隔离的 Desktop 实例（独立 `--user-data-dir`）启动后其本地后端也已对该线程发起 `thread/list`、`thread/attachment/list` |
| `thread/resume`（Desktop 打开线程的路径） | 返回完整历史（轮次与 `userMessage`/`agentMessage` 条目齐全），内置后端同步写入 `thread_history` |
| `turn/start` 续写 | 成功；模型请求含历史标记（`SM_P0_ALPHA`/`SM_P0_BETA`），无 Authorization；回答写回并可再读取 |
| 对照：Codex 原生 rollout 复制进全新目录 | 与转换会话表现一致：`thread/read` 在首次 `resume` 前返回 0 轮，`resume` 后完整。这是 0.164 对外部放入文件的通用行为，不是转换写入的问题 |

**没有验证**：Desktop **主窗口界面**。隔离实例没有账号登录状态，主窗口停在启动画面（日志为 `Sign in to ChatGPT to start a durable thread`）；按安全规则不登录账号、不使用日常客户端做实验。因此侧边栏显示与点开后的渲染仍需有登录状态的实例人工确认。

## 没有验证（不要当作已支持）

- Codex Desktop **主窗口界面**里转换会话的显示、真实模型续写与重开；Codex 当前证据限 CLI 及 Desktop 内置后端的 app-server（见第五轮），WorkBuddy GUI 的显示、真实模型续写和重开已通过。
- 原生工具调用记录（外来工具映射）、附件、子代理、compaction 的原生边界、编辑分支：转换时按文本渲染或丢弃并计入保真度报告。
- 跨账号（E1c）、Windows/Linux 实机、两台物理电脑。

## 复现

门控测试 `TestLiveConvertToCodex`（`internal/source/workbuddy/convert_live_test.go`，`SM_LIVE_OUT` 与 `SM_LIVE_PHASE=convert|capture` 控制）生成并回采；外部脚本在 `$SM_HOME/dev/sandboxes/convert-live-*/` 起本地桩模型服务、写隔离 `CODEX_HOME/config.toml`、驱动 `codex app-server` 与 `codex exec resume`。桩服务按请求里出现的标记回答，请求不含任何凭据。

第四轮现场脚本及脱敏断言结果保存在沙箱的 `workbuddy/cross_e2e.py`、`workbuddy/cross_back_live.py`、`workbuddy/cross-e2e/results.json`；字面 HTML 与历史时间验证见 `workbuddy/convert_html_live.py`、`workbuddy/html-convert-live/results.json`。脚本仅可对该隔离夹具分阶段执行，重复安装会按幂等规则拒绝；不应直接用于日常客户端。回归测试见 `TestSyncCapturesConvertedContinuation`、`TestCrossAgentRoundTrip`、`TestNativeRestoreAcrossConvertedIdentities`、`TestTargetProviderUsesTomlAndSelectedProfile`。
