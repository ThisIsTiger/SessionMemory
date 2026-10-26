# SessionMemory：单会话跨电脑同步与跨 Agent 原生转换设计

> 状态：Draft / 待实施  
> 版本：v0.6  
> 日期：2026-10-26  
> 项目：`SessionMemory/`，独立 Go module 与二进制  
> 代码来源：`../summarize/agent-daily/`，仅复用采集基础代码，复制后独立演进  
> 本次调整：落实三项决定——冲突后非后代 head 采用“备份后原位替换”（E1b 证明不可行时才改为新建原生会话）；加密协议简化为共享 vault key，取消设备签名与授权记录；WorkBuddy 等 Agent 的会话读取参照 magpie 项目（§3.3“参考实现”），并据此补充 Codex 线程写锁、多 `CODEX_HOME`、`.jsonl` 与 `.jsonl.zst` 并存及分段 rollout、CodeBuddy Code 与 WorkBuddy 的关系、转换约定的默认值。  
> 实现状态：本文描述目标方案；现有复制代码不代表下述功能已经实现。

## 1. 产品定位与优先级

SessionMemory 是个人使用的会话同步工具。个人用户的客户端数量有限，因此设计以“两三台电脑、两三个 Agent”为规模，不为大规模协作预留复杂度。

| 优先级 | 用户需求 | 本期结果 |
|---|---|---|
| 第一优先 | 公司 Codex ↔ 家里 Codex | 会话数据可靠同步，在受支持的 Codex 客户端恢复原生会话，继续后的记录能同步回来 |
| 第二优先 | Codex ↔ WorkBuddy 等不同 Agent | 用户选择一个会话和目标客户端，该会话的**完整历史转换后出现在目标 Agent 自己的会话列表中**，可继续、可重开，继续后的记录能回采并再切回 |
| 兜底 | 转换不可用时 | 退化为 Resume Pack 上下文接续，并明确标注“仅上下文” |
| 后续 | 同项目下多个独立 session 的整合 | 暂不实现，包括自动归并、项目总时间线和多会话联合摘要 |

同步可以覆盖用户选中的多个 session，但每个 session 独立处理。同目录、同仓库或标题相似，都不会导致两个 session 自动合并。

### 1.1 相对 v0.3 的主要变化

- 跨 Agent 主路径改为 **Canonical Transcript → 目标 Agent 原生转换写入**；转换后的会话是目标 Agent 的原生会话，不再只是“新会话加一份说明”。
- Portable Records 升格为 **Canonical Transcript**，成为跨 Agent 转换的依据，而不再只是浏览和查询用的派生视图。
- 新增每次转换产出的**保真度报告**：区分“历史可见”和“目标模型继续时实际收到的上下文”。
- 对厂商存储的写入底线调整为分级路线（见 §6.2）：允许写会话文件，以及经验证的最小登记写入，不允许镜像目录或复制数据库文件。
- 新增体验目标：**客户端未运行时后台自动安装**，用户打开客户端时已是最新；不要求实时刷新，也尽量不要求用户手动重启。
- P0 增加转换写入、回采和客户端运行检测实验；转换写入是否可行，决定第二优先需求能否按“完整历史”交付。
- 沿用 v0.3：不做同项目多 session 合并；检测分歧、保留两个版本、阻止自动覆盖；没有 HLC、字段级 CRDT、项目级记忆。

## 2. “同步成功”的三种标准

### 2.1 同 Agent：原生会话同步

以公司 Codex → 家里 Codex 为例：

1. 公司端选择要同步的 Codex 会话，后台采集已持久化的完整记录。
2. 数据加密写入同步目录，家里端下载并校验。
3. 家里端绑定本地项目目录，检查客户端兼容性和代码状态。
4. 通过经过验证的适配器恢复该会话；用户能在目标 Codex 的历史入口找到并继续它。
5. 家里继续产生的新记录形成下一版本，再同步回公司端。

验收必须包含原生历史可见、上下文可继续、关闭重开仍存在，以及继续后的反向同步。只生成一份摘要或让另一台电脑能读取 Markdown，不算完成第一优先需求。

跨电脑稳定的是 SessionMemory 的 `session_id`。厂商原生 ID 尽量保留；若恢复接口要求新 ID，必须明确显示映射，验证完整历史仍由原生客户端承载。它不能被悄悄替换成“新建会话并粘贴摘要”。

### 2.2 跨 Agent：转换后的原生会话

以公司 Codex → 家里 WorkBuddy 为例：

1. 用户明确选择这个 session，开启到 WorkBuddy 的转换关系。
2. 取固定 revision 的 Canonical Transcript，在家里电脑用 WorkBuddy 的 Native Writer 转换，生成转换计划和保真度报告。
3. 经用户确认，写入 WorkBuddy 的原生会话存储；WorkBuddy 的会话列表里出现一个新的原生会话，包含完整历史，并标注来源于哪个 Codex 会话。
4. 用户在 WorkBuddy 里继续；新记录由 WorkBuddy 采集适配器回采，接在同一个 `session_id` 的 revision 链后面，转换导入的部分不会被当成新消息。
5. 之后可以选择切回 Codex：用 Codex 的 Native Writer 转换，在 Codex 里生成一个新的原生会话。

“成功”的含义分级，不能混为一谈：

| 等级 | 含义 |
|---|---|
| `history-visible` | 完整历史出现在目标 Agent 的原生会话列表，可打开、可继续、关闭重开仍在 |
| `roundtrip-supported` | 在 `history-visible` 之上，继续后的新记录能被采集并再转换回另一个 Agent |
| `context-only` | 只有 Resume Pack（兜底），目标里是一个新会话加一份上下文，不展示完整历史 |

第二优先需求的验收要求 `roundtrip-supported`。`history-visible` 只算部分完成，`context-only` 不算完成。

上表是对**一次转换结果**的验收等级，由 binding 的 `install_mode`（§5.2）加上验证结果推导，不单独存储；§11.3 的 `*-ready`、`installed` 是同一流程中各设备的进度状态。三套词汇含义不同，不互相替代。

转换不保证模型内部状态、工具运行态相同。“历史能看见”和“目标模型继续时实际带着这些上下文”是两件事，保真度报告需要分别说明（见 §7.2）。

跨 Agent 是用户选择开启的能力。默认不向其他 Agent 写入历史，也不把同项目的所有 session 打包到一起。

### 2.3 体验目标：不要求实时刷新，也尽量不要求手动重启

- 数据同步在后台持续进行，目标客户端的历史**不要求实时刷新**。
- 客户端未运行时，后台自动完成安装；用户下次打开客户端，看到的就是最新版本。这是主要的体验路径。
- 客户端正在运行时，不覆盖它的存储；状态显示 `waiting-client-close`，并提供 `open` 命令：先 pull、安装，再替用户启动客户端。
- “运行中”不只是窗口开着：桌面客户端关闭窗口后，进程或后台辅助进程仍可能存活，这种状态同样按运行中处理。适配器必须区分“窗口打开 / 进程存活 / 仅后台辅助进程 / 未运行”，无法区分时按 `Unknown` 处理（§9.3）。每个客户端的实际体验分为“无需重启 / 仅需刷新 / 需退出重开”三档，由 P0 实验 E2b 实测后写入支持矩阵。
- 若某个客户端提供官方的导入、刷新或线程读取接口，使运行中也能出现新会话，则优先使用，并在支持矩阵中标注“无需重启”。是否存在这样的接口，以及运行中放入文件后刷新或切换项目能否看到新会话，属于 P0 实验 E2b。
- 无法避免的情形（客户端整天开着、又没有官方接口）会如实告知用户“需关闭并重开一次”，不隐瞒，也不为此冒险写入运行中的存储。

## 3. 范围与当前能力依据

### 3.1 本期范围

- 单个 session 的发现、选择、完整持久化记录采集、增量同步、历史查看和恢复。
- 同 Agent 在不同电脑间交替继续，优先 Codex；每个受支持的客户端形态单独验收。
- 用户选择的跨 Agent 转换，首个目标是 Codex ↔ WorkBuddy 双向，并要求往返。
- 同步目录传输、本机加密、新设备加入、完整性验证、断点重试和冲突保留。
- 本地路径映射、Git HEAD 检查，以及同步和恢复状态的清楚反馈。

### 3.2 本期不做

- 多个独立 session 的内容合并、自动关联和项目级总摘要。
- 同一会话两台电脑同时写入后的自动对话合并。
- 多人协作、中心账号服务、实时 token 镜像和远程控制 Agent。
- 运行中 shell、PTY、端口、容器、审批和客户端 UI 状态迁移。
- 自动同步源码或整个 worktree；默认不上传 dirty patch。
- 自动删除另一台电脑的原生历史、云端对象 GC 和完整密钥轮换体系。
- 在运行中的客户端里热更新会话（除非官方接口明确支持，见 §2.3）。

### 3.3 已核实能力与待验证边界

#### 官方资料（核实于 2026-10-26）

| 目标 | 官方资料支持的能力 | 本项目仍需验证 |
|---|---|---|
| Codex | 保存会话恢复；App Server 提供线程读取、历史分页和恢复等接口 | 把另一台机器的持久化会话安全导入目标安装，并在 Desktop 历史中显示、继续、重新打开 |
| WorkBuddy | 支持 MCP 连接器，包括本地 stdio，以及 Skill 接入 | 外部写入的原生会话能否被识别、显示、继续；会话记录采集与来源识别；官方导入接口是否存在尚未据此确认 |
| 微云 | 提供把本地目录同步到云端的同步助手 | 目标系统上的乱序到达、冲突副本、按需下载和 rename 行为 |

依据：[Codex 会话命令](https://learn.chatgpt.com/docs/developer-commands)、[Codex App Server](https://learn.chatgpt.com/docs/app-server)、[WorkBuddy 连接器](https://open.workbuddy.cn/docs/connector)、[微云同步助手](https://www.weiyun.com/download.html)。

#### 本机结构观察（2026-10-26，开发机）

以下只来自对本机存储结构的只读观察（目录、表名、字段名、记录类型，未读取对话内容）。它们是推断依据，**不代表导入已被验证**，版本号和稳定性也未确认。

| 对象 | 观察到的结构 |
|---|---|
| Codex 会话文件 | `~/.codex/sessions/YYYY/MM/DD/` 下的 rollout 有两种存储形式：未压缩的 `rollout-*.jsonl`，以及 zstd 压缩（单帧）的 `rollout-*.jsonl.zst`；`archived_sessions/` 下本机全部是 `.jsonl.zst`。本机 189 个线程中 25 个是未压缩文件、164 个只存在压缩文件：近期活跃的多为前者，旧会话多为后者，已恢复使用的旧会话可再次出现为未压缩文件，由谁压缩尚未确认；`threads.rollout_path` 始终写 `.jsonl` 名。记录一行一条，信封为 `{timestamp, ordinal, type, payload}`，`ordinal` 等于从 0 起的行号；类型含 `session_meta`、`turn_context`、`response_item`（`message`、`reasoning`、`function_call`、`function_call_output`、`custom_tool_call` 等）、`event_msg`（含 UI 层的 `item_completed`）、`compacted`、`token_usage_record` 等。同一段对话同时以模型层（`response_item`）与 UI 层（`event_msg/item_completed`）两套记录出现，数量不一一对应。推理记录带 `encrypted_content`，`session_meta` 含创建者账号标识，每个 `turn_context` 含权限与沙箱策略 |
| Codex 会话登记 | `state_5.sqlite` 的 `threads` 表（含 `rollout_path`、`cwd`、`title`、`archived`、`git_*` 等）；`session_index.jsonl` |
| Codex 派生投影 | `thread_history_1.sqlite` 含 `thread_history_projection_state`（每线程的 `next_rollout_byte_offset`、`next_rollout_ordinal`）、`thread_turns`、`thread_items`（带 `rollout_byte_offset`、`rollout_ordinal`，`item_type` 与 `item_completed` 的条目类型是同一套命名），表现为由 rollout 文件派生的分页投影；`threads.history_mode` 本机均为 `paginated`。Codex 能否自行重建投影、外部追加 rollout 后是否会重新投影，均待验证（E1a、E1b） |
| Codex 其他登记 | `state_5.sqlite` 另有 `thread_dynamic_tools`、`thread_attachments`、`thread_spawn_edges`（父子线程；子代理线程是独立 rollout）、`projects`、`thread_sections` 等；`~/.codex/.codex-global-state.json` 含 `thread-project-assignments`、`projectless-thread-ids`、`pinned-thread-ids` 等项目归属信息；`session_index.jsonl` 的条目少于线程数，不是完整登记。其中哪些由 Codex 自行补齐、哪些必须写入，由 E1a、E1b 逐项记录；子线程的处理见 E1c |
| WorkBuddy 会话文件 | `~/.workbuddy/projects/<编码后的目录>/<会话id>.jsonl`；本机 93 个会话文件中只有 14 个有 `<会话id>.meta.json`（meta 并非通用）。另有旁路文件：`<会话id>.file-rollback.ndjson`、`<会话id>/tool-results/`（溢出的工具输出）、`<会话id>/subagents/`（子代理会话），以及 `~/.workbuddy/file-history/<会话id>/`。目录名是 cwd 的有损编码，cwd 必须取自记录或登记，不能从目录名反推 |
| WorkBuddy 记录类型 | `message`、`reasoning`、`function_call`、`function_call_result`、`file-history-snapshot`、`ai-title`，另有 `session-meta`、`custom-title`、`summary`、`resend-fork-notice`；条目含 `sessionId`、`cwd`、`parentId`、`callId`、`providerData` 等字段。绝大多数条目带 `parentId`，历史是带编辑/重发分支的消息树而不是单链；`summary` 的语义未确认 |
| WorkBuddy meta | 含 `createdAt`、`updatedAt`、`cwd`、`itemCount`、`messageCount`、`migratedFrom`、`sourceConversationId` 等，说明它自身存在“迁移会话”的概念 |
| WorkBuddy 会话登记 | `~/.workbuddy/workbuddy.db`（SQLite，WAL；用普通 `-readonly` 会打不开，需用 `immutable=1` 或 `mode=ro` 的 URI 只读打开）的 `sessions` 表：含 `id`、`cwd`、`user_id`（非空）、`title`、`status`、`created_at`/`updated_at`、`deleted_at`（软删除）、`transport`（`local`/`cloud`）、`agent_dirty`、`agent_last_synced` 等列；本机 102 行，其中 63 行已软删除；93 个会话文件全部有对应登记行，没有“只有文件、没有登记行”的会话。该库有按版本递增的迁移脚本（`.workbuddy-sqlite-migrations/`）。同库还有 `session_usage` 表；`~/.workbuddy/edge-sync-mapping-v4.db` 的用途未知。`~/Library/Application Support/WorkBuddy/codebuddy-sessions.vscdb`（键值表，仅 2 行，最近修改在 2026-04，早于 `workbuddy.db`）看起来是遗留存储；`~/.codebuddy` 是同族命令行产品 CodeBuddy Code 的数据目录，会话格式与 WorkBuddy 相同（据 magpie，见下文“参考实现”）；二者可以是同一个目录，读取时须按目录身份去重。WorkBuddy 的会话列表是否由 `workbuddy.db` 驱动、仅放文件是否可见，**未验证**，由 E3 判定 |

两个 Agent 的会话都以 JSONL 为主体，但并不是简单的一一对应：Codex 同一段对话有模型层与 UI 层两套记录，WorkBuddy 是带分支的消息树，并依赖旁路文件和登记行。这使“经统一中间格式做转换”仍是可行的候选路线，但以下都需要 P0 实验确认：外部写入的会话是否被客户端认可，是否需要同时写登记或索引，以及转换后的条目是否被目标模型接受。

#### 参考实现：yetone/magpie

magpie（MIT 许可，Go）是一个读取多个 Agent 会话的本地工具：它只读各 Agent 的目录，并带有 Claude Code ↔ Codex 的会话转换。以下是 2026-10-26 阅读其源码得到的线索，**来自他人代码，未在本机验证行为**，且它的转换只在同一台电脑上进行，不涉及跨设备同步和路径映射。本项目把它当作事实线索和行为参考，不复制代码（如需复制须保留其 MIT 声明），结论以自己的 P0 实验为准。

| 对象 | 线索 |
|---|---|
| Codex 目录 | `$CODEX_HOME`，否则 `~/.codex`。一个 ChatGPT 账号对应一个 `CODEX_HOME`，会话写进它所属账号的目录（可有 `~/.codex-work` 之类的多个目录）。rollout 文件名为 `rollout-<YYYY-MM-DDTHH-MM-SS>-<线程 ID>[_<分段 ID>].jsonl[.zst]`，同一线程可有多个分段文件 |
| Codex 压缩 | `.jsonl.zst` 由 Codex 应用的“压缩本地聊天历史”设置产生，整文件写入；同一线程的 `.jsonl` 与 `.jsonl.zst` 可能并存（“孪生”文件），magpie 逐块比较内容后只算一份；用 `klauspost/compress/zstd` 解压 |
| Codex 线程写锁 | `~/.codex/thread-writer-locks/<线程 ID>.lock` 与 `.coordination.lock` 是 Codex 自己发布 rollout 的写入协议。magpie 改写 rollout 时先取协调锁，再尝试取该线程的锁，取不到说明 Codex 正持有该线程，就放弃；改写前后还比较文件大小和修改时间，改写后保持文件权限和修改时间；没有 `thread-writer-locks` 目录的旧版 Codex 一律不改 |
| Codex 会话名 | `session_index.jsonl` 被当作线程名称（改名）记录读取，不是完整登记 |
| WorkBuddy 目录 | `$WORKBUDDY_CONFIG_DIR`，否则 `~/.workbuddy`；会话在 `projects/**/<会话 ID>.jsonl`，子代理会话在 `<会话 ID>/subagents/` 下；`<会话 ID>.meta.json` 只为迁移自旧历史的会话提供 cwd |
| WorkBuddy 记录 | OpenAI Agents SDK 风格的条目：`message`、`reasoning`、`function_call`、`function_call_result`，另有 `ai-title`、`custom-title`（用户自拟标题优先于 AI 标题）；时间字段 `timestamp` 为毫秒；用量在 `providerData.usage`，用 `providerData.messageId` 归并同一次回复的多行，迁移自旧历史的会话用旧字段名（`input_tokens` 等） |
| CodeBuddy Code | 命令行同族产品，`$CODEBUDDY_CONFIG_DIR`，否则 `~/.codebuddy`，会话格式与 WorkBuddy 相同；它的目录就是 WorkBuddy 的目录时只读一次，否则会重复列出 |
| WorkBuddy 的限制 | 它是桌面应用，没有从终端恢复会话的方式；magpie 只读它的文件（没有用到 `workbuddy.db`），也**没有任何写入 WorkBuddy 的做法**，所以“外部写入的会话能否显示”没有先例，仍由 E3 判定 |
| 其他 Agent（P3 参考） | Claude Code（`$CLAUDE_CONFIG_DIR`，`projects/*/<id>.jsonl`）、OpenCode（数据库或旧 JSON 文件）、Pi / omp、Cline、Droid、Cursor CLI、Kimi Code、Grok Build 等各有目录与格式；P3 扩展时先看 magpie 的 `internal/sessions` 对应实现 |

magpie 的 Claude Code ↔ Codex 转换约定，可作为本项目转换规则的默认值，仍须经 E3–E6 验证：

- 来源只读；按用户请求才转换；结果写入目标 Agent 自己的目录，由目标自己的恢复命令打开；同一份来源内容（路径、大小、修改时间）重复请求，返回已转换的结果，不产生第二个会话；
- 不携带推理（Claude Code 的 thinking 用签名校验，Codex 的用 `encrypted_content`，对方厂商都读不了）；
- shell 命令及其输出转为目标自己的工具调用与结果（Claude Code 的 Bash ↔ Codex 的 `exec_command`），其他工具的调用与结果转为一段带围栏的文本（`[工具名]`、`[result]` / `[error]`）；单个工具输出截断到 64 KiB 并标注；
- 图片按 data URL 携带；来源是消息树（Claude Code 的 `parentUuid`）时，从最后一个叶子沿父链取活动分支，压缩边界之前的内容由压缩摘要代替；
- Codex 的 `compacted` 记录带 `replacement_history` 或 `message`，转换时据此重建压缩后的历史；Codex 自带压缩项的 `encrypted_content` 只有 OpenAI 能读，不能转换；
- 非人类输入的判定起点：Codex 的 user 消息以 `<` 或 `# AGENTS.md` 开头，是 Codex 自己写入的上下文；WorkBuddy 的 user 消息以 `<` 开头（如 `<system-reminder>`），或匹配 `^@[\w-]+:\S*$`（场景标记）；
- 写入 Codex：新建 `sessions/YYYY/MM/DD/rollout-<本地时间>-<uuid v7>.jsonl`，`session_meta` 含 `session_id`、`id`、`timestamp`、`cwd`、`originator`、`cli_version`、`source`、`model_provider`，其中 `model_provider` 取 Codex 当前使用的提供商（否则历史列表不显示）；每条用户消息和回答在 `response_item` 之外再写一条 `event_msg`（`user_message`、`agent_message`）。据 magpie 的注释，命令行 TUI 靠它显示恢复后的历史，且 Codex 打开该线程时会自己把它登记进状态库——这只证明命令行恢复，Desktop 是否如此由 E1a 判定。

Codex 的“恢复已存在的线程”不等于“任意机器导入接口”。官方资料还描述了分页历史，不能假定所有版本只靠一个 JSONL 就能完整恢复。现有 JSONL parser 只是待适配的基础。

“同 Agent”须细分为产品、客户端形态、版本和存储格式，例如 Codex Desktop 与 Codex CLI 分开记录支持状态。CLI 恢复成功不自动等于 Desktop 验收成功。

## 4. 整体架构

```text
公司电脑                                                  家里电脑
Codex 原生会话                                      Codex / WorkBuddy 原生会话
      │                                                        ▲
      ▼                                                        │
源适配器 → 本地 Session Store                     Native Writer（安装/转换写入）
              │  native snapshot                              ▲
              │  Canonical Transcript                         │
              ▼                                               │
        加密对象 + manifest   ← 同步文件夹 →   校验 → 本地 Session Store
```

本期由五个部分组成：

1. **Agent Adapter**：发现、采集、完整快照、兼容性探测、客户端运行检测、验证。
2. **Session Store**：独立 session 的版本、原始对象、Canonical Transcript、设备绑定和本地任务。
3. **Sync Engine**：加密对象发布、下载校验、增量重试和 revision 分歧检测。
4. **Native Writer**：把某个 revision 写入目标 Agent 的原生存储。同 Agent 时安装原生快照；跨 Agent 时由 Canonical Transcript 转换。每个 Agent 一个 Writer，各自带兼容矩阵。
5. **Resume Pack 生成器（兜底）**：转换不可用时，生成可读的上下文接续包。

只有源数据（原生快照、repo 状态）进入同步目录。Canonical Transcript 和转换产物都是派生数据，在使用端本地按需生成，不同步；这样日后适配器或 Writer 改进时，可以对同一 revision 重新提取、重新转换。

CLI 是首个控制入口。MCP 只服务于兜底路径，不在第一阶段范围内。

## 5. 最小数据模型

### 5.1 SyncSession

`session_id` 是 vault 内随机生成的 128 位标识（形式 `ss_<base32>`），第一次纳管一个原生会话时创建，随后随同步数据传播。不要用本地绝对路径作为身份。

```text
SyncSession
├── session_id
├── origin_agent / origin_native_session_id
├── project_ref?                  # 可选标签和目录映射键
├── revisions                     # 独立版本链；发生冲突时保留分支
├── native_bindings               # 各设备/Agent 的原生会话映射
└── conversions                   # 用户明确建立的跨 Agent 转换记录
```

从本工具恢复或转换出来的原生会话携带可信导入记录，再次扫描时沿用已有 `session_id`。仅凭相同原生 ID、cwd 或文本相似，不自动认定两个未经纳管的来源是同一会话。重复纳管候选可以提示，不能靠内容拼接来消除。

可信导入记录存放的位置（本地 binding 表、目标会话自带的 meta 字段，或两者并用）由 P0 实验 E8 决定：若目标原生格式有合适的来源字段（如 WorkBuddy meta 中的 `migratedFrom` 一类），可作为辅助证据，但不能是唯一依据；设备重装后必须能通过同步数据重建绑定。

### 5.2 NativeBinding

一个同步会话可以有公司 Codex、家里 Codex、家里 WorkBuddy 等本地映射：

```text
binding_id, session_id, device_id, agent_profile, native_session_id
install_mode (native | converted | context)
state (active | superseded | detached)
base_revision_id, installed_revision_id, capture_cursor
import_receipt, fidelity_report_ref?
```

- `agent_profile` 记录产品、客户端形态、版本、原生格式和适配器/Writer 版本。
- `install_mode` 表明该原生实例来自同 Agent 原生安装、跨 Agent 转换，还是仅上下文接续。
- `base_revision_id` 表示这个原生实例从哪个已知版本开始继续。
- `installed_revision_id` 表示最后验证安装到该实例的版本；不等同于云端最新版本。
- `import_receipt` 记录写入目标的条目边界（见 §8.3），用于界定“导入部分”与“用户之后的真实新记录”。
- 原生绝对路径、cursor 和安装位置属于设备本地数据，不作为跨电脑路径使用。
- 一个原生会话只绑定一个同步会话，拒绝把它附加到另一个独立 session。
- `state`：每个 (session, 设备, Agent) 同时最多一个 `active` 绑定，只有 `active` 绑定被采集和更新。`superseded` 是被冲突后落地（§9.2 第 6 点）或切回来源 Agent（§7.1）取代的旧原生实例：保留记录、不再采集，若它出现新记录，`status` 给出提示，用户可选择把它重新激活为分支；`detached` 见 §8.2。`list` 与 `status` 显示该状态。

跨 Agent 转换创建多个原生实例，是一个被用户选中会话的转换映射，不是同项目独立会话的合并。

### 5.3 Revision

每次安全采集产生一个不可变 revision，正常情况下只有一个 parent：

```json
{
  "schema_version": 1,
  "session_id": "ss_...",
  "revision_id": "rev_<payload-sha256>",
  "parent_revision_id": "rev_...",
  "producer_device_id": "dev_home",
  "producer_binding_id": "bind_...",
  "agent_profile": { "agent": "codex", "surface": "desktop", "format": "adapter-defined" },
  "native_snapshot_ref": "obj_...",
  "repo_state_ref": "obj_...",
  "capture_state": "checkpoint",
  "created_at": "2026-10-26T18:00:00+08:00"
}
```

`revision_id` 按规范化 payload（不含自身 ID）计算；object ref 指向已持久化的密文对象。父链缺失、对象缺失或哈希不符时不提交可恢复状态。

revision 链给出单会话的接续顺序，时间戳只用于显示。本期不需要跨独立会话的 HLC 和因果图。

快照可以引用已经存在的分块；“每个 revision 都可还原完整状态”不意味着每次重传全部历史。

### 5.4 Canonical Transcript

Canonical Transcript 是跨 Agent 转换的中间格式，由适配器的 `Extract` 在使用端从 Native Snapshot 派生（记录 `adapter_version`），缓存在本机、可重建，**不作为 revision 的同步对象**：原始快照是事实源，适配器修复缺陷后可对已发布的 revision 重新提取。因此 `item_id` 必须跨适配器版本稳定（优先使用原生记录 ID 或原生行序号，如 Codex 的 `ordinal`），import receipt 引用的标识不得随提取器升级而改变。

```text
Item
├── item_id                # 稳定记录键：原生记录 ID 或原生行序号，跨适配器版本稳定；两者都没有时才用 (generation, offset, hash)
├── kind                   # message | reasoning | tool_call | tool_result | compaction
│                          # | attachment_ref | session_meta | unknown
├── role?                  # user | assistant | system | developer（message）
├── parent_item_id?        # 因果/调用关联
├── call_id?               # tool_call 与 tool_result 配对键
├── timestamp
├── normalized             # 归一化字段（文本、工具名、参数、状态等）
└── raw_ref                # 指向来源 Agent 原始条目的引用（原始内容留在 native snapshot）
```

约束：

- **原始数据不丢**：归一化字段是派生的，原始条目始终可由 `raw_ref` 找回；识别不了的记录保留为 `unknown` 并带原始引用，不静默丢弃。
- **保序**：保持来源 Agent 的原始顺序；不因转换重排。
- **调用配对**：`tool_call` 与 `tool_result` 必须能按 `call_id` 配对；采集时发现孤立的一方要标记，转换时不得制造孤立条目（见 §7.2）。
- **隐私类条目**：推理内容、system/developer 指令、附件正文等默认以加密对象存放；是否写入目标由转换规则决定，不因“要转换”而放宽。
- 不同 Agent 的归一化字段取并集的最小公共语义，Agent 私有字段放在 `raw_ref` 之后，不污染公共字段。
- **主流与链接**：同一段对话在来源里有多套记录时（如 Codex 的模型层 `response_item` 与 UI 层 `item_completed`），适配器声明一个主流作为 Canonical 条目的来源，另一套以 `raw_ref` 列表链接到对应条目，不重复产出，也不丢弃。`tool_call` 需覆盖各来源的变体（如 Codex 的 `custom_tool_call` 自由格式输入）。
- **活动路径**：来源是消息树时（如 WorkBuddy 的 `parentId`、编辑/重发分支），适配器选定活动叶子并给出 `active_path`；转换只写活动路径，其他分支留在 Canonical 中并计入保真度报告。
- **人类输入**：不能仅凭 `role = user` 判定人类输入（Codex 的 rollout 中存在注入或继承的 user 角色消息，需要看记录的 metadata）；适配器须声明“人类输入”的判定规则，规则由 M2 的真实样本与 E4 确定；起点是 magpie 使用的启发式（Codex：以 `<` 或 `# AGENTS.md` 开头的 user 消息是客户端自己写入的上下文；WorkBuddy：以 `<` 开头，或匹配 `^@[\w-]+:\S*$` 的场景标记），见 §3.3“参考实现”。

2026-10-26 的隔离实测修正了 WorkBuddy 的起点规则：5.8.3 将注入的 `<system-reminder>` 与人类问题的 `<user_query>` 放在同一个 `input_text` 中。适配器须提取后者作为人类输入，保留完整原始记录，不能因为整段以 `<` 开头而丢弃问题；只向转换及 Resume Pack 提供 query 正文。具体规则见 spec §8.4 与 [P0 第二轮报告](p0-live-report.md)。

## 6. 同 Agent 原生同步流程

### 6.1 源端采集

1. 用户纳管一个会话，或显式开启某 Agent 的新会话自动纳管。
2. 适配器确定这个版本需要的最小原生持久化数据及依赖。
3. 在一致性边界采集快照，生成原始对象（Canonical Transcript 不在此生成，见 §5.4）。
4. 本地持久化 revision、对象和 cursor 事务记录，然后发布到同步目录。
5. 后台持续采集新记录；阶段结束可手动执行 checkpoint。

自动采集不等于自动控制客户端。支持在会话活跃时保存安全记录；只有满足适配器检查的稳定边界才标记为可原生恢复。

### 6.2 目标端恢复

1. Pull 并验证选定 revision 及全部依赖对象。
2. 检查 Agent 版本、存储格式、目标路径、代码状态，以及目标客户端是否正在运行或仍在加载目标会话。
3. 在 staging 构建本地原生恢复包，生成可查看的安装计划。
4. 按下列优先级选择写入路线（**写入底线**）：
   1. 厂商支持的导入/恢复/线程接口；
   2. 只写会话文件，让客户端自己回填或重建登记与索引；
   3. 适配器声明、已在隔离配置中验证、版本在白名单内的**最小行级登记写入**。
5. 安装前检查目标当前版本，建立备份和恢复 journal；客户端必须满足适配器要求的未运行状态（见 §9.3）。
6. 原子安装并让客户端通过自己的机制登记或重建索引。
7. 验证历史、恢复能力和重开持久性；成功后记录 binding、receipt 和基线。

写入底线：

- 允许写：会话文件，以及路线 3 所限定的登记条目。
- 不允许：把整个 `~/.codex`、`~/.claude`、`~/.workbuddy` 或 WorkBuddy 的应用目录镜像到另一台电脑；复制认证、客户端全局 SQLite/WAL 或跨设备覆盖索引数据库；整库替换。
- 登记写入只能是事务内的最小行级写入，不能复制或替换数据库文件、WAL 文件；写前必须备份，可回滚。
- 单会话使用专属存储时，由版本适配器定义一致性导出，不直接复制活跃数据库文件。
- 路线 3 默认关闭，需用户对该 Agent 显式启用；P0 需证明路线 1、2 不可行才启用，并在支持矩阵中标注。WorkBuddy 的会话列表很可能由 `workbuddy.db` 驱动（§3.3，未验证）：E3 先单独测路线 2，再仅在隔离环境测路线 3，并记录所需的最小行内容；是否启用仍由 P0 决策点交用户逐 Agent 决定。
- 某个版本若三条路线都没有安全做法，则该版本暂不列为支持。P0 必须找到可用路线，否则第一优先需求仍是阻塞项，不能靠摘要回退宣布完成。

### 6.3 路径、版本与恢复保真度

- 公司 `/work/app` 可以映射到家里 `D:\code\app`；原始对象保持不变，安装副本才应用适配器声明的路径变换。
- 只变换已知结构化路径字段，不对历史文本和命令做全局字符串替换。历史命令中的旧路径仍是原始记录，不能自动重放。
- 路径变换若改变记录的字节长度，会使 Codex 投影中保存的 `rollout_byte_offset` 失效（`ordinal` 不受影响）。对已被客户端投影过的前缀，路径变换必须不改变字节长度，或由 E1b 证明客户端会重新投影，否则不得对该会话原地应用。
- 原生快照是适配器声明的**文件集合**（附相对路径清单），不限于单个会话文件：例如 WorkBuddy 的溢出工具输出、子代理会话和回滚文件，Codex 的附件、动态工具和子线程。需要的会话附件、压缩检查点、子代理引用或其他原生依赖须列出；缺失时显示具体缺口。
- Codex 子线程（独立 rollout，经 `thread_spawn_edges` 关联）先作为依赖列出，缺失时显示缺口；是否必须随父会话一并采集和安装，由 E1c 判定（本条不改变产品范围）。
- Writer 契约：目标路径只由已校验的 ID 与固定模板生成，不使用归档携带的路径；拒绝 `..`、绝对路径、盘符和符号链接组件；设单会话大小与文件数上限（细则见开发文档 §4.2）。
- 每个已发布适配器维护客户端形态、版本区间、操作系统和格式的兼容矩阵。条目按“已验证的版本区间 + 能力指纹”判定，指纹包括已见的记录类型、必需键、`history_mode` 等：本机 189 个 Codex 线程跨 42 个版本号，精确版本白名单没有可用性。区间外或指纹不符的格式拒绝写入厂商目录；未知记录类型在采集时保留为 `unknown`。路线 3 的条目仍须精确到登记库的 schema 级别。
- 认证、权限策略、模型可用性和本地工具配置由目标设备自行提供，恢复数据不能覆盖这些设置。
- 上一条在 Codex 上目前没有机制保证：原生记录里含账号绑定内容（推理记录的 `encrypted_content`、`session_meta` 的创建者账号标识）和每轮的权限/沙箱策略（`turn_context`）。跨账号回放能否继续、目标是否会沿用最后一个 `turn_context` 的策略，由 E1c 验证；在此之前不宣称“原生恢复”已覆盖这些字段。若 E1c 要求剔除或改写其中某些字段，由 Writer 按适配器声明执行，列入安装计划并计入报告，同时下调“原生恢复”的保真度表述。

默认命令要求 `native` 模式。同 Agent 兼容性不足时可以另行选择 `context` 兜底，但结果必须标记为 `context-only`，并保留原生恢复失败原因。

### 6.4 客户端未运行时的后台安装

后台同步与目标原生安装是两件事，前者可以一直运行，后者受安全边界约束：

1. 后台 pull 并验证新 revision，在本地准备好 staging 的安装计划。
2. 检测到目标客户端未运行（检测信号由适配器声明，见 P0 实验 E2a），且满足 §9.3 的快进条件，则自动安装并验证，更新 `installed_revision_id`。
3. 客户端正在运行：保持 `waiting-client-close`，不安装；客户端退出后自动继续。
4. 自动安装需用户按 Agent 单独启用，默认关闭；首次启用前展示安装计划样例。自动安装只用于同 Agent 的原生恢复与更新（`install_mode = native`）；跨 Agent 转换写入始终要用户在 `convert --plan` 之后显式确认，不自动执行。
5. `session-memory open SESSION_ID --agent X`：先 pull，再等待或要求客户端关闭，完成安装后启动客户端。适合“整天开着客户端”的用户在切换时使用。
6. 后台常驻由 `sync --watch` 以各系统的登录项承载（macOS LaunchAgent、Windows 计划任务）。注册命令和钥匙串访问策略（登录后凭据存储可能尚未解锁、后台进程的访问授权）在 P1 实施前（M5）定稿；常驻进程取不到 vault key 时不安装，状态显示等待解锁，不反复报错。
7. 安装竞态：安装前先取该目标的互斥锁；写入之后、提交 binding 之前再检测一次运行状态，客户端在写入期间启动视为失败并回滚，回滚后回到 `waiting-client-close`。`installed` 的含义是适配器级验证通过（文件和登记行可被适配器读回，必要时借助官方的无界面读取接口）；“用户打开客户端后确实看到”由 P0 与 P1 验收，不增加单独的 `confirmed` 状态。

自动安装前依然执行备份和 journal，失败时回滚并提示，不重复尝试无限次。

## 7. 跨 Agent：转换写入与兜底接续

### 7.1 显式建立关系

用户为某个 `session_id` 选择目标 Agent 和目标设备。关系包含来源 revision、目标 binding、转换方式（`converted` 或 `context`）和使用的 Writer 版本。

```text
公司 Codex：R1 → R2
                   │ 用户选择转换到家里 WorkBuddy
                   ▼
家里 WorkBuddy：新的原生会话（R2 的完整历史，由转换写入）→ R3 → R4
                                                 │ 用户选择切回 Codex
                                                 ▼
公司 Codex：新的原生会话（R4 的完整历史，由转换写入）→ R5
```

R3 只新增 WorkBuddy 的真实后续记录；转换导入的 R1/R2 内容通过 import receipt 划界，不会被当成新消息重复收录。

跨 Agent 切换默认在目标 Agent 创建新的原生会话并绑定同一个 `session_id`。旧的 Codex 会话保留在 R2，不向它强行追加 WorkBuddy 的记录。只有具备独立验证的原生追加能力时才提供其他模式。

切回来源 Agent 时，会在该设备上产生新的原生会话，旧会话的绑定标记为 `superseded`（§5.2）：它仍留在客户端里，但不再采集。

### 7.2 转换写入（主路径）

流程：

1. 确认目标电脑已为该会话绑定本地项目目录（§10）。WorkBuddy 等按目录组织原生存储的 Agent，写入位置由本地目录决定，未绑定时只能生成计划，不能写入。然后取选定 revision 的原始快照（由适配器在本机提取 Canonical Transcript，见 §5.4）和 repo 状态。
2. 目标 Agent 的 Native Writer 逐条转换，生成目标原生条目、会话元数据和转换计划。
3. 生成**保真度报告**，并让用户在 `--plan` 中查看。
4. 经确认后，按 §6.2 的写入底线和安全检查安装到目标存储。
5. 验证：目标会话出现在历史列表、能打开、条目数与计划一致、关闭重开仍在。
6. 记录 binding、`install_mode = converted` 和包含条目边界的 import receipt。

同一个 revision 对同一目标设备和同一 Writer 版本只转换一次：重复发出请求时返回已有的转换结果，不产生第二个会话。

转换规则（原则；具体字段映射由 P0 实验确定，并随 Writer 版本发布）：

| Canonical 条目 | 转换原则 |
|---|---|
| `message`（user/assistant） | 写为目标原生消息，保序保时间戳；user 消息中注入或继承的上下文类内容是否写入，按 §5.4 的人类输入规则和 E4 结果决定 |
| `message`（system/developer 等指令类角色） | **不写入**目标，仅保留在 Canonical 中；源 Agent 的权限、项目指令不能被当作目标 Agent 的指令 |
| `reasoning` | 默认**不写入**目标（推理内容常为厂商私有或加密，跨厂商无法回放）；保留在 Canonical 中，报告中计数 |
| `tool_call` + `tool_result` | 工具名与参数能映射到目标已知工具，且通过目标格式校验时，写为目标原生调用与结果；否则渲染为带标记的文本消息（如“[来自 Codex 的工具调用]”），保证历史可读；文本渲染对工具输出设大小上限（默认 64 KiB，magpie 采用同一数值，可调），超出部分截断并标注“已截断，完整内容见 session_id/item_id”，不把超大输出整段写入目标 |
| 孤立的 `tool_call` 或 `tool_result` | **不写为原生条目**，避免目标客户端或模型拒绝；渲染为文本并在报告中列出 |
| `compaction` | 写为摘要类消息并标注来源，不伪装成原始对话；目标若有原生的压缩/摘要边界，能否做到“历史可见 = 全部、模型上下文 = 摘要 + 尾部”由 E6 判定（WorkBuddy 的 `summary` 记录语义尚未确认），做不到时在报告中注明上下文容量风险 |
| `attachment_ref` | 附件可传输则一并写入；否则保留引用并在报告中列出缺失 |
| `session_meta`、UI 状态、token 统计、文件快照等客户端私有记录 | 不写入；标题尽量沿用，来源信息写入目标支持的来源字段。例外：目标客户端的历史渲染若依赖其 UI 层记录或连续序号（如 Codex 的 `item_completed` 与 `ordinal`），Writer 须按该目标的格式生成这些记录，是否需要由 E4 判定 |
| `unknown` | 不写入，计数并列出类型 |

#### 保真度报告

每次转换生成一份报告，至少包含：

- **条目层**：各类条目保留、转换为文本、丢弃的数量，以及未能转换的具体原因；
- **历史可见度**：目标会话中可见的消息数量、工具调用的呈现方式；
- **模型上下文说明**：用户在目标里继续时，目标模型实际会收到什么、没收到什么（例如被丢弃的推理、转为文本的工具调用）；
- **上下文容量**：转换后历史的估算 token 数，以及目标常用模型的窗口；可能超出时在报告中提示，并说明目标客户端的处理方式（压缩、截断或报错，由 E6 验证），不为“看起来完整”而静默截断历史；
- **环境缺口**：项目目录、代码状态、附件、目标缺少的工具；
- **写入路线**：使用了 §6.2 的哪条路线，是否涉及登记写入。
- **数据去向**：用户在目标里继续时，完整历史（含工具输出）会发送给目标 Agent 所用的模型服务，可能是与来源不同的厂商。该提示在 `--plan` 中显示；转换前对来源 revision 复扫 secret（§12），命中则阻止转换，除非用户明确确认。

报告不含对话正文和 secret，只含统计和类型信息，随转换记录保存在本地，可选择同步给其他设备以便查看。

保真度不足时（例如目标拒绝转换后的条目，或报告显示关键信息丢失），默认不继续写入，由用户决定停止或改走兜底路径。

### 7.3 Resume Pack（兜底）

仅在以下情形使用：目标 Agent 的 Writer 未通过验证或版本不在矩阵内；转换写入被目标客户端拒绝；用户主动选择“只带上下文”。结果标记为 `context-only`，并保留转换未采用的原因。

```text
Resume Pack
├── 身份：session_id、revision_id、来源 Agent、生成时间
├── 当前目标、用户约束与明确偏好
├── 已完成工作、关键决策和未完成项
├── 本地项目映射、branch、HEAD、dirty/依赖缺口
├── 最近若干完整 turn
├── 工具/文件/产物引用及早期记录查询入口
└── 信息来源引用与内容完整性说明
```

- 只基于选定 session 的固定 revision 生成，不读取同项目的其他会话。
- 先提供确定性的会话索引、近期对话和用户填写的交接说明，不把 LLM 摘要作为第一版的必需依赖。
- 语义摘要可选；记录模型与生成版本，每条结论引用记录 ID。无摘要时如实显示“未提炼”，不从截断片段宣称完整掌握全部进展。
- 明确 token budget、已省略的内容和查询入口；工具输出可分页读取，图片或大附件不能假装已由文本完整表达。
- 来源历史是参考数据，不能成为目标客户端的系统指令；不自动执行历史工具调用或继承源端审批。
- 明文包只在本地按需生成，使用私有权限；同步目录只存加密对象。
- 兜底路径通过本地 stdio MCP 或本地 Markdown 提供给目标客户端；MCP 在主路径稳定之前不是必需项。
- 若 Resume Pack 之后目标 Agent 的新记录要回采，需要能识别注入边界；不能识别边界的目标不开放兜底路径的往返续写。

### 7.4 WorkBuddy 的首期接入

优先实现 WorkBuddy 的 Native Writer 与采集适配器，使 Codex ↔ WorkBuddy 能走转换写入并往返。

- WorkBuddy 能读取 MCP，不意味着它能导入外部会话，也不意味着新对话会自动回传；转换写入和回采都要单独验证。
- `~/.workbuddy` 是 WorkBuddy（桌面应用）的目录，`~/.codebuddy` 是 CodeBuddy Code（命令行）的目录，两者会话格式相同；用户可以把二者指到同一目录，所以按目录身份去重，并让会话归属取决于使用它的产品，不取决于目录名（§3.3“参考实现”）。
- 外部写入的会话是否需要同时登记到 `~/.workbuddy/workbuddy.db` 的 `sessions` 表（`codebuddy-sessions.vscdb` 看起来是遗留存储，不作首选候选），由 P0 实验 E3 决定，路线按 §6.2 的优先级选择。本机观察到每个文件型会话都有登记行，所以需要登记行的可能性较大，但未验证。E3 须记录所需的最小行内容（`user_id` 取目标安装当前登录的用户，另有 `deleted_at`、`status` 等），以及 WorkBuddy 自身的同步/脏标记（`agent_dirty`、`transport`）在重启后的行为。WorkBuddy 采集适配器的 `Discover` 须识别 `deleted_at`，已软删除的会话不当作活跃会话。
- WorkBuddy 采集适配器需要识别 import receipt 划定的边界，回采边界之后的真实新记录。

跨 Agent 支持分开报告：`context-only`、`history-visible`、`roundtrip-supported`（定义见 §2.2）。第二优先需求的完整验收要求后者，不能只演示第一次转换。

### 7.5 持续同步与切换规则

数据同步可以持续运行；“让哪个 Agent 继续”是显式接续动作。用户建立的跨 Agent 关系会保留，后续可以再次选择已同步版本继续，无需重新配置。

- 目标还未续写时，可以在安全状态下基于更新的 revision 重新转换；已加载旧上下文时不能静默改变它。
- 目标开始续写后，以该绑定的基线形成后续版本；旧端继续写入属于分歧，不自动把两份消息塞进同一原生历史。
- 第一版不在两个活跃 Agent 之间自动推送新消息、不双向自动注入上下文。
- 用户可以从 A 切到 B，再从 B 切回 A；每次明确版本和缺口。它是交替继续，不是多个独立 session 的内容合并。

## 8. 采集、完整记录与增量提交

### 8.1 原生快照与 Canonical Transcript 各自的职责

| 数据 | 用途 | 保真要求 |
|---|---|---|
| Native Snapshot | 同 Agent 原生恢复 | 保留适配器判定的必要原生记录和依赖（以文件清单声明，可含多个文件），不能只保留 user/assistant 文本 |
| Canonical Transcript | 跨 Agent 转换写入、浏览、按需查询 | 保序的消息、推理、工具调用/结果（可配对）、压缩标记、附件引用；未知记录保留原始引用；原始条目始终可找回。由适配器从 Native Snapshot 在使用端提取，缓存可重建，不同步 |
| 转换产物 | 写入目标 Agent | 由 Canonical 在目标电脑本地按 Writer 版本生成，不同步，可重新生成 |
| Resume Pack | 兜底的首轮上下文 | 可重建的派生数据，不作为原生恢复或转换依据 |

原始记录和 revision 是事实源。SQLite 中的视图、搜索索引、Canonical 缓存和同步任务可重建；采集 cursor、安装 receipt 等设备状态须有本地 journal，不从云端猜测。

现有 `conversation.Batch` 主要为按日期提取文本服务，不作为新的同步协议。其清洗、格式化和合并逻辑不能改变用于原生恢复和转换的原始数据。现有 Codex parser 对推理、工具调用和压缩记录是显式丢弃的，不能直接作为 Canonical 提取的实现；可复用的只是“完整行读取与偏移推进”这一层。

### 8.2 JSONL 与其他存储

对经过验证的追加式 JSONL：

1. 从已提交 offset 读取，只纳入以完整换行结尾、可解析的记录。
2. 保存 `(binding, generation, start, end, raw_hash)`，按安全边界切分 immutable segment。单个 segment 有大小上限（默认 8 MiB，由 M2 实测调整；单条记录超过上限时独占一个 segment）；首次采集大会话时流式处理，不把整个会话读入内存。
3. 验证已采集前缀，记录文件身份，识别截断、替换和同长度重写；不能只比较文件大小。每轮用头尾抽样哈希快速判定；checkpoint、安装前，以及文件大小/修改时间/文件身份出现异常时，对已存 segment 做全前缀哈希复核（头尾抽样发现不了前缀中部的同长度重写）。
4. 发布新 revision 的快照描述，引用原有段和新增段。

Codex 的 rollout 可能以未压缩的 `.jsonl` 或 zstd 压缩的 `.jsonl.zst` 存在（§3.3）。offset、cursor、segment 和哈希都定义在**解压后的字节流**上；同一个流在两种形式间转换（压缩、解压）只是存储形式的变化，不是 `Replaced`，不新增 generation。压缩文件没有随机访问，校验一律用全前缀哈希。目标端安装默认写为未压缩的 `.jsonl`，不得让同一线程同时存在两种形式；目标已有该线程的 `.zst` 时如何处理，由 E1b 验证。

对分页历史或专属存储，适配器提供一致性快照、稳定记录键和增量游标。没有安全快照方式时只能导出验证过的只读上下文，并标记原生恢复不支持。

压缩、回滚或重写不解释为“自动合并”：保留旧 revision，记录新的 generation 和替代关系；只有确认属于同一绑定的正常格式演进才接受新快照，不静默截掉旧历史。

遇到完整但无法解析的行时，不静默跳过，也不让整个会话永久卡住：该会话标记 `unsafe`，保留之前的安全 revision，报告具体偏移；后续记录是否继续采集由适配器的兼容矩阵决定，未知格式拒绝安装。

原生会话文件被移动、归档、压缩或删除（例如 Codex 把会话移入 `archived_sessions/`，或把旧会话压缩为 `.zst`）时，binding 以原生会话 ID 为准重新定位，不以路径为身份；文件消失且无法重新定位时，binding 标记为 `detached`，既不当作截断，也不把删除传播给其他设备（§3.2）。

### 8.3 崩溃一致性、幂等与导入边界

对象先写本地 staging，经 fsync 和校验后发布，再提交 revision 与 cursor；崩溃遗留对象可以重试或回收，不能先推进 cursor 再丢失正文。跨文件与 SQLite 的提交用 journal 恢复，不假设它们共享事务。

同一采集段由原生记录 ID 或 generation/offset/hash 去重；重复 push/pull 和重启不新增一份相同历史。

**导入边界**：同 Agent 恢复和跨 Agent 转换写入完成后，import receipt 记录写入的条目数量、最后一条条目的标识（或偏移）和边界之前内容的哈希。采集适配器据此：

- 验证边界之前的内容未被改写；
- 只把边界之后的记录当作新记录；
- 边界之前的内容不重新进入 revision 链。

识别依据是写入时留下的确切记录标识和哈希，不是文本相似；边界被改写或无法确认时，不凭内容去重，而是标记异常并保留原始数据。

导入边界按原生文件的记录顺序和偏移判定，不按消息树位置判定：用户在目标里编辑或重发一条已导入的消息，会在边界之后追加新的分支记录，这是正常使用，按边界之后的新记录进入 revision 链（带分支标记），不是异常；只有边界之前已写入的记录被改写才算异常。

转换写入的会话，其 Writer 在写入条目时应尽量留下可识别的来源标记（如目标格式支持的来源字段），便于边界验证；无法留下标记的目标，需要靠记录标识和哈希。无法确定导入边界的目标，不开放往返续写。

Resume Pack（兜底）的注入同理：通过 pack ID、来源 revision 和精确注入记录识别，只有可验证的导入内容才跳过。

## 9. 交替工作与分歧保护

### 9.1 正常模式：交替续写

推荐离开当前电脑前执行 checkpoint，到另一台电脑后先 pull，再继续。工具可以保存最后活跃设备和交接提示，但同步文件夹不能提供强一致分布式锁，也不能禁止用户绕过工具打开旧会话。

第一版不实现远程 lease，也不宣称能绝对保证单写。安全性由不可变版本保留和恢复前检查承担。

### 9.2 两台电脑同时续写

```text
           R2
          /  \
公司产生 R3A  家里产生 R3B
```

这是同一同步会话的版本分歧，不是需要马上实现的项目多 session 合并。处理规则：

1. 两个 revision 都保存，不按修改时间覆盖其中一个。
2. 两个 head 标记为 `conflict`，停止自动原生安装、自动转换和自动选择“最新”。
3. 用户比较后选择一个 head 继续；另一个保留在冲突历史中。
4. 选择记录引用用户已经看到的 head 集合，属于 continuation choice，不生成一个合并两边内容的 revision。
5. 离线旧设备之后上传新后代，或两台设备作出不一致选择，重新显示冲突，不自动认定此前选择涵盖未知版本。
6. `choose` 之后，各设备按下列方式落地：
   - 选定的 head 是该设备已安装 revision 的后代：按 §9.3 快进；
   - 否则（例如公司选了 R3A，而家里已安装 R3B）：不能快进，也不自动覆盖。该设备的状态显示“需要处理”，须用户显式确认后才落地：要么在备份和 journal 保护下，把该原生会话替换为选定 head（同一原生 ID，要求该设备本地分支已完整采集为 revision），要么新建一个原生会话（原绑定标记为 `superseded`，需要新原生 ID 时按 §2.1 显示映射）。**v1 采用前者（已定）**：备份并替换为选定 head，保持同一原生 ID；仅当 E1b 证明 Codex 无法对被替换或追加的文件重新投影、且没有已验证的补救写法时，才改为后者（新建原生会话，新 ID，原绑定 `superseded`）。两种方式都须用户显式确认，不自动执行。

分歧发生在跨 Agent 的情形下同样适用：一个来自公司 Codex 原生会话，另一个来自家里 WorkBuddy 转换会话，两个都从 R2 继续，则按同样规则保留两个 head。

### 9.3 原生快进条件

只有远端版本是本地安装版本的后代、本地没有未采集新记录、适配器确认目标未被加载/写入，并且安装前再次比较基线，才能对**已有**原生会话执行更新（`choose` 之后的非后代落地见 §9.2 第 6 点，须用户显式确认）。

如果客户端不能协作锁定或安全卸载目标，要求关闭客户端后再安装；文件几秒未变化不等于客户端已经停止写入。检测信号（进程、文件锁、官方接口之一或组合）由适配器声明并经 P0 实验 E2a 验证；窗口已关闭但进程或后台辅助进程仍存活按运行中处理，进程身份以应用包路径或用户数据目录判定，不凭进程名。无法确认时显示 `waiting-client-close`，不冒险覆盖。

创建**全新**原生会话（如首次转换到 WorkBuddy）不覆盖已有数据，风险较低，但在客户端运行时写入仍可能被客户端的索引忽略或覆盖，因此同样遵循“客户端未运行时安装”的规则，除非适配器证明运行中写入安全。

## 10. 目录映射与代码状态

项目只作为 session 的标签和恢复前检查信息。首次选择本地目录时建立随机 `project_ref`，其他设备显式映射；remote identity、branch 和 HEAD 用于匹配提示，不凭路径或 remote 自动合并 session。

每次 checkpoint 保存 branch、HEAD、dirty/untracked 摘要及必要产物引用。源码继续通过 Git 或用户原有方式同步。

- 本机没有项目目录：可以查看会话，继续前需绑定目录。
- 缺少 commit、HEAD 不一致：默认阻止直接按原代码状态继续，给出 fetch/checkout 等修复信息。
- 用户明确选择在不同代码状态下继续时，记录 override，并在恢复结果或保真度报告中显示差异。
- dirty 文件未传输、大附件缺失或工具不可用：明确列出；会话数据完整和工作环境就绪是两个独立状态。
- dirty patch 默认关闭；未来开启也需单独预览与应用，不自动覆盖目标代码。

## 11. 同步目录与传输协议

### 11.1 FolderTransport

第一版继续复用用户已登录的微云同步助手，只操作本地目录。同步层不依赖微云私有接口，也不直接操作厂商原生会话目录。传输接口保持窄：`Put`、`List`、`Get` 和一个“文件是否稳定可读”的判定，平台差异收在实现里。

```text
SessionMemoryVault/v1/
├── public/vault.json
├── objects/ab/<ciphertext-sha256>.obj
└── manifests/<device-id>/<sequence>-<hash>.manifest
```

全部发布文件不可变；没有多台电脑共同覆盖的 `latest.json`，也不同步本地 SQLite。每设备 sequence 分配和发布需本机互斥锁及 journal；恢复系统备份不得让两个活跃设备共用同一个 device ID：设备启动时若发现同步目录里本设备 ID 已有比本地记录更大的 sequence，视为 ID 被克隆或本地状态回退，拒绝发布并要求重新生成 device ID。

manifest 的加密 payload 由 §11.4 列出的条目类型组成，并带前一 manifest hash。外层只暴露格式版本、device_id、sequence 和密文 hash；内容的真实性由 AEAD 保证（关联数据绑定 vault、device_id、sequence 和前一 manifest hash），没有单独的签名。

### 11.2 Push / Pull

Push：安全采集并提交本地 revision → 加密对象 → 临时文件 fsync/close → 发布最终文件 → 发布 manifest → 记录本地 published。

Pull：扫描 manifest → 验证 AEAD（只有持有 vault key 的设备能产生）、链和密文 hash → 等待依赖到齐 → 解密并检查 schema、revision 父链和对象内容 → 原子提交本地副本 → 检测分歧 → 按需准备恢复或转换。

- 用 `.partial-<uuid>` 写临时文件，importer 忽略未完成文件。
- manifest 可以先于 object 到达，保持 `pending-objects`，不导入半份会话。
- 乱序、重复、微云冲突副本按内容验证去重；损坏数据隔离到 quarantine。
- watcher 只降低延迟；定期完整 reconcile 保证最终发现，不能仅靠 mtime 或 watcher。
- 自适应重试文件被占用、按需下载和暂时缺失；FolderTransport 不假定各平台 rename 行为完全一致。

### 11.3 状态与触发

| 状态 | 含义 |
|---|---|
| `captured` | 本地采集已持久化 |
| `published` | 已发布到本机同步文件夹，尚不等于另一台电脑收到 |
| `received` | 目标设备已下载并验证 |
| `native-ready` / `converted-ready` / `context-ready` | 指定设备和 Agent 已具备原生安装、转换写入或上下文接续所需的材料 |
| `installed` | 指定目标已安装并验证指定 revision，含 `install_mode` |
| `pending-objects` / `waiting-client-close` / `conflict` | 仍有缺口或需要处理 |

目标将针对 revision 的 received/installed receipt 作为自己的加密 manifest 发布；源端收到 receipt 后才能报告该设备已收到/安装。设备长时间离线时显示最后确认版本和时间，不声称全局最新。这些“最后活跃”“长时间离线”等展示只是提示，不进入冲突判定。

建议默认每 60 秒检查一次新记录并 push/pull；活跃会话只在稳定边界（一个 turn 结束，或空闲约 10 秒）且距上次 revision 不少于约 5 分钟时才生成新 revision（间隔数值待 M3 实测）；`checkpoint` 后立即生成并 push。各设备在本地记录已处理的最大 manifest sequence，reconcile 只处理更大的。打开前短时 pull，超时显示 stale 和最后确认版本。目标原生安装遵守安全边界，不能用后台同步频率替代。

### 11.4 Manifest 条目类型

manifest 的加密 payload 由下列条目组成。字段在 M4 冻结信封之前定稿；每类条目都有发布者规则和幂等键，重复条目按幂等键去重。

| 类型 | 内容 | 发布者 | 幂等键 |
|---|---|---|---|
| `revision` | revision 引用、依赖对象、父 manifest hash | 产生该 revision 的设备 | `revision_id` |
| `receipt` | 某 revision 在某设备 received / installed（含 `install_mode`） | 目标设备自己 | `(device_id, revision_id, 状态)` |
| `choice` | continuation choice：用户已看到的 head 集合和所选 head（§9.2） | 做出选择的设备 | choice 内容的 hash |
| `convert-request` | `continue --device` 生成的转换请求：来源 revision、目标 Agent、目标设备（§13） | 发起设备 | 请求 ID |
| `binding` | `session_id` 与各设备原生会话的映射摘要、import receipt 摘要，用于设备重装后重建绑定（E8）；不含本地路径和 cursor | 绑定所在设备 | `binding_id` + 版本 |

项目目录映射（§10）是设备本地数据，不发布；`project_ref` 只作为标签出现在 revision 中。

## 12. 加密与数据边界

- 首个跨电脑版本即对同步目录中的原始快照、repo 状态和私有 metadata 加密。Canonical Transcript、转换产物和 Resume Pack 都是派生数据，不进入同步目录。
- 使用成熟库提供的 AES-256-GCM 等 AEAD；每对象用随机 salt 经 HKDF 从 vault key 派生对象密钥，并用随机 nonce；关联数据绑定 vault、schema 和对象类型。
- 所有持有 vault key 的设备同等受信，不设每设备签名身份和授权记录；`device_id` 只是标签，不是经过认证的身份。manifest 的真实性来自 AEAD（§11.1），云端或第三方无法伪造或篡改。
- 初始版本通过可信线下渠道传递一次性设备加入包，内含加密的 vault key；加入包用独立高强度口令保护（argon2id 派生密钥），口令与包分开传递，不放在同步目录。加入成功后新设备生成自己的 `device_id`。
- vault key 保存在本机凭据存储，提供离线加密恢复包；两台电脑各自登录 Agent，不同步 token、cookie、认证数据库或 keychain。
- 不遍历打包整个 workspace、完整环境变量、`.env` 或凭据文件。会话引用外部文件不意味着默认上传文件正文。
- transcript 本身可能包含用户粘贴的 secret：不能保证“完整原文”和“自动脱敏”同时成立。风险扫描命中时暂停该会话的首次发布，允许排除会话，或选择另行脱敏的上下文导出；脱敏副本不能标为字节保真的原生副本，也不能作为转换写入的来源冒充完整历史。跨 Agent 转换前对来源 revision 复扫一次（§7.2 报告的“数据去向”）；同一用户自己设备之间的后续增量不逐个扫描。
- 本地明文暂存（staging、转换产物、备份）使用私有权限，安装日志不输出正文、secret 和密钥；跨 Agent 读取范围限于用户选择的会话及允许的本地目录。
- 转换写入的备份里含目标客户端的原始会话数据，同样按私有权限保存并设置保留期限。

本期是个人受信设备共享 vault。移除设备可停止正常参与，但持有旧密钥的设备仍可解密已获得的数据；完整安全撤销需要新密钥和剩余设备重新加入。第一版给出迁移到新 vault 的操作流程，不承诺仅删一条设备记录即可安全吊销。自动历史重加密、GC 后置。

## 13. CLI 与 MCP 草案

以下均为拟议命令，不代表当前已经存在。

```bash
# 初始化与两台电脑接入
session-memory init
session-memory transport attach-folder "/path/to/SessionMemoryVault"
session-memory device invite --out device-invite.bundle
session-memory device join device-invite.bundle

# 发现与选择原生会话
session-memory agent probe codex
session-memory agent probe workbuddy
session-memory discover --agent codex
session-memory track --agent codex --native-session NATIVE_ID
session-memory list [--agent codex] [--project PROJECT_REF]

# 同 Agent 跨电脑同步
session-memory checkpoint SESSION_ID
session-memory sync [--watch] [--auto-install]
session-memory status SESSION_ID
session-memory project map PROJECT_REF --path /local/repo
session-memory restore SESSION_ID --agent codex --mode native --plan
session-memory restore SESSION_ID --agent codex --mode native
session-memory open SESSION_ID --agent codex

# 用户选择的跨 Agent 转换（主路径）
session-memory continue SESSION_ID --agent workbuddy --device DEVICE_ID
session-memory convert SESSION_ID --agent workbuddy --plan      # 展示转换计划与保真度报告
session-memory convert SESSION_ID --agent workbuddy
session-memory fidelity SESSION_ID --agent workbuddy            # 查看保真度报告

# 兜底：仅上下文接续
session-memory continue SESSION_ID --agent codex --mode context
session-memory resume-pack SESSION_ID --revision REVISION_ID --out resume-pack.md

# 冲突保护与诊断
session-memory revisions SESSION_ID
session-memory choose SESSION_ID --revision REVISION_ID
session-memory verify SESSION_ID [--deep]
session-memory auto-install enable --agent codex                 # 按 Agent 单独启用，默认关闭
session-memory mcp serve                                         # 仅兜底路径需要，可选
```

`continue --device` 生成绑定目标的转换请求并发布，不在源电脑远程打开目标 App。目标端查看请求，在本地完成转换写入；`open` 只操作当前电脑。`restore --plan` 与 `convert --plan` 展示版本、路径变化、覆盖范围、保真度和缺口；实际安装必须重新校验基线。

MCP 在兜底路径中只提供选定会话的读取能力：

```text
session_list                   # 受本地授权范围限制
session_get_resume_pack        # 指定 session_id / revision_id / budget
session_read_records           # 稳定记录引用与分页
session_get_sync_status
```

MCP 不承担自动采集对话；采集由 Adapter 完成。项目级 merge、自动代码应用和远程控制工具不进入首版。若以后添加会话绑定写工具，须校验本地调用上下文及唯一原生 ID，不能让模型任意关联独立会话。

## 14. 代码组织与已有代码使用方式

```text
cmd/session-memory/       # CLI
internal/agent/           # capability、capture、write、verify、运行检测契约
internal/source/codex/    # 复用发现/分类，扩展完整快照、原生恢复与 Writer
internal/source/claude/   # 保留基础，排在 Codex / WorkBuddy 验证之后
internal/source/workbuddy/ # 发现、采集、Writer
internal/canonical/       # Canonical Transcript 定义、校验（配对、保序）
internal/convert/         # Canonical → 目标原生条目的转换规则与保真度报告
internal/session/         # identity、revision、binding、分歧检测
internal/store/           # immutable objects、SQLite 视图、事务 journal
internal/sync/            # manifest、receipt、reconcile
internal/transport/       # FolderTransport
internal/vault/           # 加密、加入包、device_id
internal/bridge/          # Resume Pack（兜底）、注入基线
internal/mcp/             # 可选本地读取接口
```

已有代码的定位：

- 复用会话发现、来源识别、完整行处理、checkpoint 和平台文件操作的思路及适用实现。
- 保留现有测试 fixture，再补充实际版本的脱敏恢复与转换样本；旧文本解析测试不能证明原生恢复或转换可用。
- 拆出新的 session/revision/canonical 协议，不让日报的日期 Batch、OperationID 或摘要输出路径决定同步格式。
- 现有 parser 对推理、工具调用和压缩记录的“显式丢弃”逻辑不能用于 Canonical 提取；它的 `event_msg` 回退针对旧版记录形态（本机 189 个线程都不含），不认识 `item_completed`、`token_usage_record` 等记录，旧 fixture 不能代表当前格式；现有发现逻辑只认 `.jsonl`，需同时处理 `.jsonl.zst`；文件替换检测目前只比较文件大小，需按 §8.2 增强。
- 现有发现逻辑基于“最近 N 小时”的日报窗口，同步场景应以用户选中的会话为准，不因会话长时间未变而排除。
- 收敛 config 中 Summary、Report、Blog、Schedule 等遗留字段；这属于新功能实现时的适配工作，不因本次文档调整就宣称已完成。
- Codex 是否纳入 CLI/exec 由支持矩阵和用户选择决定，不能继续沿用日报采集默认排除规则作为产品边界。

## 15. 分阶段实施与交付门槛

### P0：先验证“能写进去、能继续、能回采”

P0 是一组带明确通过标准的实验，产出支持矩阵、脱敏 fixture 和实验报告。实验在隔离的用户目录或测试配置中进行，不在日常使用的客户端上试写。

| 编号 | 问题 | 通过标准 | 失败时 |
|---|---|---|---|
| E1a | Codex 新线程的最小恢复材料：仅放一个新的会话文件（未压缩 `.jsonl`），客户端能否自行回填登记（`threads`）、历史投影（`thread_history_1`）和项目归属；是否还需要 `session_index.jsonl`、`.codex-global-state.json` 或最小登记写入；写入的 `session_meta.model_provider` 是否须等于目标 Codex 当前使用的提供商，会话才在历史里列出（magpie 的做法，待验证）；用命令行 `codex resume` 与 Desktop 分别测 | 目标 Codex 的历史入口出现会话，在正确的项目下、可继续、重开仍在，且不复制数据库文件；逐项记录 rollout、`threads` 行、投影表、`session_index.jsonl`、`.codex-global-state.json` 中哪些由 Codex 自行补齐、哪些必须写入 | 记录具体阻塞；评估路线 3（最小行级写入）；仍无路线则第一优先需求标为阻塞 |
| E1b | Codex 原地追加：向**已登记**的线程安装更长的 rollout（A2 的第二跳），含路径映射改变字节长度的情形，以及存储形式两种情形——安装后 Codex 把它压缩为 `.zst`、目标已有该线程的 `.zst`，以及同一线程的 `.jsonl` 与 `.jsonl.zst` 并存、线程有多个分段 rollout（`_<分段 ID>`）的情形；原地改写按 Codex 自己的发布方式进行：先取 `thread-writer-locks/.coordination.lock`，再尝试取该线程的锁，取不到就放弃，改写后保持权限与修改时间（据 magpie，待验证） | 追加后历史完整，无重复或残缺的尾部，项目归属不变，重开仍在；Codex 对被追加/替换的文件重新投影（或有已验证的补救写法）；压缩后绑定与历史仍有效；新线程不同时留下 `.jsonl` 与 `.zst` | 记录失败形态；改用“替换式安装”或新建线程（需要新 ID，见 §9.2 第 6 点），并更新 §17 待决策；都不可行则第一优先需求标为阻塞 |
| E1c | Codex 账号与子线程：在另一个账号登录的客户端上恢复同一会话（据 magpie，一个账号对应一个 `CODEX_HOME`，会话写进它所属账号的目录，所以这也是跨目录的恢复）；恢复一个带子线程的父会话 | 首个继续的 turn 成功（推理加密内容、创建者账号字段不导致拒绝），且恢复后生效的权限/沙箱策略等于目标设备配置而非源端值；父会话在子线程未安装时可打开、显示缺口并继续；得出子线程是否必须随父会话安装、哪些字段需要剔除或改写（只列键名） | 需要剔除/改写字段时，由 Writer 声明并下调保真度表述（§6.3）；子线程必须随装时，纳入原生快照的文件集合（§6.3） |
| E2a | 客户端“是否运行”的检测信号：能否区分“窗口打开 / 进程存活 / 仅后台辅助进程 / 未运行”；进程身份能否以应用包路径或用户数据目录判定；Codex 的线程写锁（`thread-writer-locks/<线程 ID>.lock` 与 `.coordination.lock`）能否作为“该线程正被 Codex 持有”的可靠信号，以及没有该目录的旧版 Codex 如何处理 | 得出可靠信号；四种状态各有明确的“可否安装”结论；信号矛盾时落入 `Unknown` | 采用“要求关闭客户端”，并在支持矩阵标注 |
| E2b | 运行中的可见性矩阵：客户端运行中放入新会话或向已登记线程追加记录后，是否出现（自动 / 刷新或重新聚焦 / 切换项目 / 需重启）；Codex App Server 的 `thread/list`、`thread/read` 能否读到外部放置的 rollout；WorkBuddy 仅在隔离环境中插入最小登记行后列表是否出现 | 每个客户端得出并写入支持矩阵的结论：“无需重启 / 仅需刷新 / 需退出重开” | 采用 `open` 与 `waiting-client-close`，如实告知需要重启 |
| E3 | Codex → WorkBuddy：转换写入后，WorkBuddy 能否显示、打开、继续、重开；是否需要同时在 `workbuddy.db` 的 `sessions` 表登记（先测仅放文件，再仅在隔离环境测最小行）；WorkBuddy 重启及隔一天后，文件与行是否保留、是否被其自身同步改写或产生云端重复 | 完整历史在 WorkBuddy 会话列表可见并可继续，继续时模型不报错；记录所需的最小行内容；重启后文件与行保持完整，无重复会话 | 区分原因：文件被忽略、需要索引、格式校验失败、被自身同步改写；决定登记写入或降级为 `context-only` |
| E4 | WorkBuddy → Codex：反向转换写入；判定 Desktop 的分页历史是否需要 UI 层 `item_completed` 记录和连续 `ordinal`（magpie 写的是 `event_msg` 的 `user_message` / `agent_message`，据其注释命令行 TUI 靠它显示恢复后的历史） | 同 E3，在 Codex 历史入口（分页历史）可见并可继续；转换后的用户消息数与源端的人类输入数一致 | 同 E3；需要 UI 层记录时由 Writer 生成（§7.2） |
| E5 | 转换并继续后的回采：边界之后的新记录能否被采集，边界之前不重复收录；在导入的消息上编辑/重发（WorkBuddy 分支）是否被正确当作新记录；回到来源 Agent 时，“来源段落原样回放 + 外来段落转换”与“整体转换”的结果对比 | 新记录进入 revision 链，导入部分不重复，分支不被判为异常，往返后再转换仍一致；给出两种回放方式的结论 | 无法划定边界则不开放往返续写 |
| E6 | 模型对转换条目的接受度：工具调用/结果、推理被丢弃后、孤立条目被渲染为文本时，目标模型能否正常继续；长会话超出目标上下文窗口时目标客户端如何处理；含 compaction 的会话能否映射到目标的原生摘要边界（若有）；WorkBuddy `summary` 记录的语义 | 转换后会话可正常继续一轮，无格式拒绝；保真度报告各类计数与实际写入计数一致；记录目标实际发送的 token 数，超窗时客户端行为与报告所述一致 | 调整转换规则（更多转为文本、移除不支持条目），并在报告中注明上下文容量风险 |
| E7 | 微云实际文件行为：乱序、冲突副本、按需下载、文件占用 | 同步逻辑在真实环境中靠完整 reconcile 达到最终一致 | 调整重试与稳定判定，不改协议 |
| E8 | 导入记录与 `session_id` 的绑定位置（本地表 / 目标 meta 来源字段） | 设备重装或迁移后能重建绑定，不重复纳管 | 以同步数据中的 binding 为准，目标字段仅作辅助 |
| E9 | Codex fork/resume 产生新原生 ID 或复制内容时的归属 | 能识别为同一会话的分支或独立会话，不被误并入 | 暂不自动判定，提示用户处理 |

产出：每个 Agent 的支持矩阵、Writer 字段映射草案、保真度报告样例和脱敏 fixture。

**实验轴**：P0 在开发机上进行，Windows 与第二台真实电脑留到 P1 验收。每个写入类实验至少记录：①客户端版本对（含“新版本产生、旧版本恢复”的方向）；②账号对（E1c）；③Desktop 与 CLI 形态分开记录；④路径映射取非恒等映射（E1b）。P0 覆盖不到的轴，在支持矩阵中标为“未验证”，不推断为通过。隔离环境的选择见开发文档 §9.1；行级登记写入（路线 3）的实验只允许在隔离环境中进行。

**P0 决策点**：实验报告交给项目负责人（用户）审阅，按结果决定：第一优先需求走哪条写入路线；第二优先需求按 `history-visible`、`roundtrip-supported` 还是 `context-only` 交付；是否按 Agent 逐个启用路线 3；E1b 的结果还决定 §9.2 第 6 点是否需要改用新建原生会话。结论写入本文档，再进入 P1。

**门槛：**

- 选中的 Codex 会话能在目标 Desktop 历史入口出现、继续并再次打开，新增记录能作为后续 revision 回采；并已完成 E1b（对已登记线程的原地追加）与 E1c（跨账号继续）；
- Codex ↔ WorkBuddy 的转换写入至少完成一个方向的 E3/E4 验证，并明确另一方向的结论；
- 若无安全可用的恢复路线，记录具体阻塞，第一优先目标不降级为摘要。

### P1：交付 Codex ↔ Codex 跨电脑同步

- 实现 CLI、独立本地存储、完整快照、增量 cursor 和安装 journal。
- 实现加密、设备加入、FolderTransport、push/pull/receipt 和断点重试。
- 实现项目路径映射、代码状态检查、原生恢复与分歧保护。
- 实现客户端未运行时的后台自动安装与 `open` 命令。
- 后台同步支持用户选择的多个会话，但各自独立。

**门槛：**公司与家里两台真实电脑交替继续同一 Codex 会话，包含往返、重启、断网恢复和 Desktop 入口验证。P0 本机测试不替代这里的两设备验收。

### P2：交付 Codex ↔ WorkBuddy 转换接续

- 实现 Canonical Transcript、Native Writer（两个方向）、保真度报告和 `convert --plan`。
- 实现 WorkBuddy 采集适配器、导入边界与回采。
- 实现 Codex → WorkBuddy → Codex 往返。
- 实现 Resume Pack 兜底及（可选）本地读取 MCP。
- 同项目其他 session 不进入转换或 Resume Pack。

**门槛：**WorkBuddy 的会话列表里有完整历史，能说明既有目标、关键进展和下一步并继续；产生的新工作可采集并切回 Codex；转换导入的部分不被重复收录；保真度报告与实际一致。

### P3：扩展兼容性与维护

按实际使用需求扩展其他 Agent、版本和系统；完善大附件、可选语义摘要、安装体验、备份恢复和密钥维护。

多 session 合并不属于 P0–P3 的依赖。如将来需要，另开设计，不能在实现同步时顺手引入 workstream/lane 和自动归并。

## 16. 验收场景

| 编号 | 场景 | 必须满足 |
|---|---|---|
| A1 | 公司 Codex → 家里 Codex | 指定会话原生历史可见、能继续，关闭重开后仍可用 |
| A2 | 家里继续 → 公司继续 | 全局 session_id 保持一致，后续内容可往返且不重复 |
| A3 | 两台电脑路径和系统不同 | 映射结构化路径；旧历史不被全局替换；目标格式受支持 |
| A4 | 多个同项目 session | 各自同步、各自恢复，不自动归并 |
| A5 | 公司与家里离线同时续写 | 两份版本保留，显示 conflict，自动安装和转换停止，不自动拼接 |
| A6 | 选择一个冲突版本后旧端再上传 | 新分歧再次显示，未见版本不会被之前的选择隐藏 |
| A7 | 半行、截断、分页变化和源文件重写 | 不推进到不安全 cursor，不丢旧 revision，未知格式拒绝安装 |
| A8 | manifest 先到、object 后到；损坏/重复文件 | pending、重试或隔离；完整前不标记 native-ready |
| A9 | 发布或安装时崩溃、之后重启 | journal 可恢复，旧原生数据可回滚，重试不重复 |
| A10 | 目标客户端还在写入 | 等待安全关闭，不因文件暂时稳定就覆盖 |
| A11 | 目标客户端未运行时有新版本到达 | 已启用自动安装时，后台完成安装并验证；用户打开客户端即看到最新，无需手动操作 |
| A12 | 目标客户端运行中有新版本到达 | 显示 `waiting-client-close`，不写入；客户端关闭后自动继续，或用 `open` 完成安装并启动 |
| A13 | 冲突后选择一个 head | 已安装版本是所选 head 的祖先的设备快进；否则显示“需要处理”，经用户确认后备份并替换为所选 head（同一原生 ID；E1b 证明不可行时改为新建原生会话，旧绑定标记 `superseded`），不自动覆盖，不丢弃任一分支 |
| B1 | 用户选择 Codex → WorkBuddy | 完整历史出现在 WorkBuddy 会话列表，可打开、可继续、重开仍在；附保真度报告；其他 session 不被写入 |
| B2 | WorkBuddy 继续 → 切回 Codex | 采集真实新增记录，转换回 Codex 后完整历史可见，不重复收录导入部分 |
| B3 | 转换写入不可用或被拒绝 | 降级为 `context-only` 并标注原因，不标记 `history-visible` 或往返支持，不算第二优先目标完成 |
| B4 | 历史含工具调用、推理、压缩记录 | 保真度报告列出保留/转文本/丢弃的数量；不写孤立的工具调用或结果；转换后目标可正常继续 |
| B5 | 转换产物被目标客户端拒绝或写入中途失败 | 回滚到写入前状态，不留下半成品会话，错误可诊断 |
| C1 | 两设备传输与状态 | published、received、installed 可区分，状态包含确认 revision 和 install_mode |
| C2 | 新电脑只有同步文件夹，无加入材料 | 无法解密；有正确加入材料后能校验并重建会话副本 |
| C3 | 目标缺 commit、dirty 文件或附件 | 会话数据与环境缺口分别显示，不能声称完整工作状态已恢复 |
| C4 | 不兼容 Agent 版本或无法安全登记 | 保留数据，说明原因，不写厂商私有全局数据库，不暗中降级 |

## 17. 首轮需要验证的问题与默认决策

以下问题通过 P0 实验收敛，不把未验证假设当作已支持能力：

1. **Codex Desktop 的安全导入路线。**（E1a–E1c、E2a、E2b）确定当前版本的存储形式、最小依赖、登记机制和是否需要关闭客户端；默认先支持一组验证过的版本，再扩大矩阵。
2. **跨 Agent 转换写入。**（E3–E6）WorkBuddy 与 Codex 的转换写入、索引需求、模型对转换条目的接受度、回采边界；没有回采不得宣称往返完成，没有转换写入不得宣称“完整历史”。
3. **微云实际文件行为。**（E7）用目标系统和同步助手验证乱序、冲突副本、按需下载及文件占用；同步逻辑以最终 reconcile 为准。
4. **工具输出与附件。**原生恢复保留必需持久化记录；转换按 §7.2 的规则保留或转文本；大附件缺失显式报告。
5. **源码与密钥。**默认不上传 dirty patch，不同步认证；设备加入先用受保护的离线包，不增加中心账号服务。

### 待决策（不阻塞 P0 启动）

| 事项 | 当前默认 | 说明 |
|---|---|---|
| 写入路线 3（最小行级登记写入）是否启用 | 默认关闭，P0 证明路线 1、2 不可行后再议 | 见 §6.2，需用户按 Agent 显式启用；WorkBuddy 的会话列表很可能由 `workbuddy.db` 驱动，E3 结果出来后在 P0 决策点逐 Agent 决定 |
| 加密协议的复杂度 | **已定：简化**——共享 vault key，取消设备签名与授权记录（§12） | 设备身份只是标签；移除设备等于迁移到新 vault；设备 ID 被克隆用 sequence 回退检测（§11.1） |
| 协议试点 Agent | Codex（已定） | Claude Code 不作为协议试点，保持在 P3；协议与 Writer 边界逻辑用 Codex 与本地双设备模拟验证 |
| 厂商原生 ID 保留策略 | 尽量保留；要求新 ID 时显示映射 | 具体取决于 E1a/E1b、E3 的实验结果；Codex 记录内嵌大量 `thread_id`，新 ID 意味着整体改写，不是廉价操作 |
| 冲突后非后代 head 的落地方式 | **已定：A**——显示“需要处理”，经用户显式确认后，备份并原位替换为选定 head（同一原生 ID，要求本地分支已完整采集为 revision） | 仅当 E1b 证明 Codex 无法对被替换/追加的文件重新投影且无补救写法时，改为 B（新建原生会话，新 ID，原绑定 `superseded`）；见 §9.2 第 6 点 |

v0.6 的交付顺序固定为：**先把一个 Codex 会话可靠地带到另一台电脑并带回来；再让用户选择把它以完整历史的形式转换到 WorkBuddy 等 Agent 继续，并能带回来。**转换写入是第二优先需求的主路径，Resume Pack 只是兜底。项目级多 session 合并单独后置。
