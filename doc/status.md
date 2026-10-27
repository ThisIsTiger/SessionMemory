# 实施状态

> 日期：2026-10-27
> 依据：[spec.md](spec.md)（实现规格）、[dev.md](dev.md)（开发计划）、[设计文档](unified-agent-session-sync-design.md)（v0.6）

本文记录**代码已经做到哪里、与规格有哪些偏差、什么还不能做以及为什么**。规格仍是规范性文档；这里的偏差中，已影响格式的都已回写 spec。

## Review 修复（2026-10-26）

本轮处理 5 项 P1、4 项 P2：

- 原生安装在厂商锁内复核文件/代码基线，保护到文件采纳和 binding 提交；失败在锁内回滚，恢复回滚也取得厂商锁；不把自身线程锁误判为客户端。
- CLI 先注册 publish/pull resumer 再恢复；恢复失败不继续分配序号；恢复发布使用原 sealed manifest，拒绝同序号不同哈希。
- manifest、entries、revision、receipt、choice 在同一个 SQLite 事务提交；pull 的已验证字节进入 staging，重启可完成导入和 received 回执。
- capture 恢复完整 binding，包含 InstalledRevisionID、游标、Unsafe 和本地目录；防止单写恢复生成重复根或假冲突。
- native 恢复拒绝不同 Agent、未知原生格式及无法采纳的安装结果。
- 重建设备 ID 采用可恢复迁移，本地绑定、待发条目及未发布历史保持可用；不修改历史 revision。
- 新设备 list/status 可见收到但尚未安装的 session。
- checkpoint 写入代码状态对象；恢复检查目录、HEAD、commit 与 dirty 缺口，新增 `--cwd`、`--allow-code-mismatch`，后者写入导入回执。

新增回归包括原 review 的 8 个隔离复现、代码状态与覆盖审计、设备迁移恢复、pull 崩溃矩阵；CI 的 failpoint 任务扩展到 CLI。真实 Desktop / WorkBuddy / 微云验收仍按下文的 P0 边界执行。

本轮完整验证：`go test ./...`、`go test -tags failpoint ./...`、`go test -race ./internal/...`、`go test -tags dev ./cmd/... ./internal/p0/...`、默认/dev/failpoint 三种构建的 `go vet`、Windows/Linux 交叉 `go vet` 全部通过。`gofmt` 和 `git diff --check` 通过；交叉 vet 不代表已在 Windows/Linux 实机执行测试。

## 真实客户端测试（2026-10-26，第二轮）

详见 [p0-live-report.md](p0-live-report.md) 与 [acceptance.md](acceptance.md)。真实 Codex CLI / app-server 在两个隔离配置间完成原生 A→B→A→B 往返、续写和重开；模型端为受控本地替身。WorkBuddy 隔离登录后，真实模型对话、采集、Resume Pack 及 Codex → WorkBuddy 的 context-only 交接通过。

实测修复了 WorkBuddy 把 `<system-reminder>` 与 `<user_query>` 放在同一输入块、导致用户问题全部被过滤的问题（适配器 0.1.1）；以及 macOS 缺默认钥匙串时初始化可能挂起的问题。新增回归、完整默认/failpoint/dev 测试和相应 race/vet 已复核。

第二轮中，WorkBuddy 仅恢复文件后重启不显示；补充同账号完整登记行后成功续写，确定了路线 3 的候选。第三轮已把原生 Writer 接入主 CLI，详见下文；当时跨 Agent 原生转换待验收，现已由第四轮补齐。Codex Desktop GUI、真实双机及微云仍待验收。

## WorkBuddy 原生 Writer 接入与验收（2026-10-26～27，第三轮）

已实现默认关闭、限 macOS / WorkBuddy 5.8.3 / 已知 schema 的手动原生 Writer：目标账号与本地私有设置、文件和登记行基线、路径映射、进程检测、备份/journal、行级 CAS 与回滚、adoption 后完整后置条件验证。CLI `restore --agent workbuddy` 已接通，默认 home 改为 WorkBuddy 配置，不再落到 Codex home。

正式二进制在隔离配置中完成 track → 加密同步 → restore → GUI 两轮历史 → 真实模型第 3 轮 → checkpoint/同步 → 源端 restore，模型返回 ACK_WRITER_E2E_OK；新增记录 cwd 为目标目录。客户端仍有后台进程时，即使显式 client-closed 也拒绝。新增/快进各 14 个中断点、共 28 项恢复断言通过；全量默认/failpoint/dev 测试、race、vet 和交叉 vet 通过。

此能力仍须手动 opt-in 并完全退出客户端；不启用 WorkBuddy 自动安装，不声称支持其他版本、任意路径编码和超大安装。具体限制、命令及实测证据见 [p0-workbuddy-writer-report.md](p0-workbuddy-writer-report.md)。

## 跨 Agent 续写闭环（2026-10-27，第四轮）

正式 CLI 在隔离配置中完成 Codex 三轮 → WorkBuddy GUI 导入 → GLM-5.3-Flash 真实模型第四轮 → 自动同步回传 → 转回 Codex → CLI 第五轮 → app-server 重开完整五轮；WorkBuddy 退出重开后四轮完整。Codex 模型端使用本地受控服务，WorkBuddy 使用真实服务。详见 [跨 Agent 报告](p0-convert-report.md)。

补齐 converted binding 的自动回采、checkpoint Agent 选择、旧目标未采集进展/半行/运行状态的提交前保护、不同转换原生 ID 的同 Agent 恢复。修正历史时间、字面 HTML 用户消息与 Codex UI 记录关联；provider 改为完整 TOML 与 profile 解析，拒绝无效配置且不泄露正文。多 session 合并仍按设计延期。

## 微云传输（2026-10-27）

本机没有微云本地同步目录，因此新增直接走微云 MCP 接口的传输（`internal/transport/weiyun.go`，`transport attach-weiyun`）：本地缓存目录 + 按名字镜像，复用原有的同步与加密逻辑。上传的分块 SHA1 状态与技能自带的 Python 参考实现逐项一致（测试向量）。已用真实微云跑通两台独立配置的完整流程（发布、加入、接收、增量第二个版本，`TestLiveWeiyunTwoMachines`，`SM_LIVE_WEIYUN=1`）。真实接口的差异：文件项不返回所在目录 key（用列出时的目录 key 下载）；删除目录需要 `dir_name`。测试清理只移入回收站。仍未验证：延迟/乱序/离线重连的长时间行为、两台物理电脑。

## 按项目同步（2026-10-27）

`project track --cwd DIR` 把目录（含子目录）下所有会话一次纳管并打同一个 `project_ref`，之后每次 `sync` 自动纳管该目录下新建的会话；另一台机器用 `project restore REF --cwd 本机目录` 一次装进整个项目（cwd 自动改写为本机目录，已是最新的跳过，单个失败不中断）。`TestProjectTrackAndRestoreAcrossMachines` 覆盖：项目内外会话区分、重复执行幂等、同步时自动纳管、B 机映射恢复与二次恢复全部 up_to_date。

**B 机（Intel、真实 Codex）暴露的问题与修复**（会话装进去了，但侧边栏看不到）：
1. 用临时 `CODEX_HOME` 做对照实验：配置加载成功（生效 provider = `custom`）时 `thread/list` 不列出 `model_provider=openai` 的会话，配置加载失败退回默认 `openai` 时能列出。结论：`thread/list` 只列 provider 等于客户端当前生效值的线程。此前 P0 都在空 home 里做，没发现。现在原生恢复写入 Codex 时，把 `session_meta.model_provider` 与 `thread_settings_applied.model_provider_id` 改成目标 `config.toml`（含 profile）的 provider；目标目录里已装文件则沿用它自己的 provider（之后切换 provider 也不影响追加）；配置读不到则不改。
2. 补登记出来的 `cwd` 仍是 A 机路径：路径改写原先只覆盖 `session_meta`、`turn_context`，漏了 `event_msg/thread_settings_applied`（`thread_settings.cwd`、`runtime_workspace_roots`）和 `world_state` 的 `cwd`，且 `runtime_workspace_roots` 只改第一处。已全部覆盖。
3. 桌面客户端启动时只读自己的登记，不会扫描新放进去的 rollout；`thread/list` 的默认扫描才会让 Codex 自己补登记。现在 `restore`/`project restore`/自动安装成功后，用桌面应用自带的 Codex（`ChatGPT.app/.../codex`，可用 `SM_CODEX_BIN` 指定）起一个短命 stdio `app-server`，对目标 `CODEX_HOME` 做 `thread/list` 分页直到列出刚装的线程（`codex.Register`，不直接碰任何数据库）；失败只告警。真实 Codex 0.164 上 `TestRegisterLive`（`SM_LIVE_CODEX`）通过；测试默认把 `SM_CODEX_BIN` 指向不存在的路径，避免启动本机真实客户端。
4. 已装文件是旧版写出的，无法追加；现在 binding 的原生文件被删除后再次 `restore` 会重新安装。

B 机实测（project2 版 + 手动触发登记并重启应用后）：会话出现在「最近」、点开历史完整、输入框可用。未验证：会话归入「项目」分组（需在应用里添加该项目目录，工具不改应用的项目列表）；登记后不重启应用是否直接显示；`runtime_workspace_roots` 里指向 A 机 `~/.codex` 下目录的第二项在 B 机 `resume` 时是否被接受。跨 Agent（转换）暂不按项目批量处理。

## Codex 会话自动归入项目（2026-10-27，B 机只读确认后实现）

**机制**（A 机 0.164 与 B 机 26.901/0.153.4 都确认）：会话属于哪个项目只记在桌面应用私有的 `~/.codex/.codex-global-state.json`：`thread-project-assignments[会话 id] = {projectKind:"local", projectId:<local-projects 里的项目 id>}`。`state_5.sqlite` 的 `threads.project_id` 全是 NULL，app-server 也没有对应请求。没有这条记录的会话进「最近」，并出现在 `projectless-thread-ids`。`thread-project-membership-host-ids` 只在 A 机存在（值 `"local"`），B 机没有这个键。

**实现**（`internal/source/codex/projects.go`、`cmd/session-memory/cmd_assign.go`）：
1. 选项 `agents.codex.assign_projects`，默认开；`session-memory config set agents.codex.assign_projects false` 关闭。
2. 会话装进 Codex 后（restore / project restore / adopt / relay / 自动安装）进入待办队列；之前装的会话用 `session-memory file-projects` 补进队列并立即尝试 `<state>/codex-assign-pending.json`，每次同步也会重试。
3. 只在桌面应用没在运行时写（`ps` 里没有 `ChatGPT.app` / `Codex.app` 主进程；写前再查一次）。应用开着就留在队列，下一轮应用关闭时补上。
4. 按会话 rollout 第一行的 `cwd`，选应用里 `local-projects.rootPaths` 覆盖它的最深一个项目；没有项目覆盖时留在队列（最多 14 天），提示用户在应用里把该文件夹添加为项目一次，之后自动归入。
5. 已有归属记录的会话不动（用户或应用已决定）。只加三处：`thread-project-assignments`；已有 `thread-project-membership-host-ids` 键时加 `"local"`；从 `projectless-thread-ids` 里去掉。其他键原样保留。
6. 写前把原文件备份到 `<state>/backups/codex-global-state.<时间>.json`（保留最近 10 份），用临时文件加原子替换写回。
7. 测试：`projects_test.go`（最深项目、已有归属、无项目、应用运行时不写、没有改动不备份、无 membership 键不新建）、`assign_test.go`（应用运行时排队，关闭后同步补上；选项关闭时不排队）。

B 机实测（Intel、26.901 / 0.153.4、project12）：应用关闭后 `sync` + `file-projects` 写入 `01a17781` 的归属，备份生成，除 `thread-project-assignments` 与 `projectless-thread-ids` 外无其他键变化；用户重新打开应用后，该会话出现在 SessionMemory 项目下（已确认）。排查中发现两处体验问题并修复：后台同步写入成功时不打印 `filed N`；应用在运行时即使会话已归好也一直留在队列。现在只有真要写入才检查应用是否运行，提示里带上命中的进程。未解释：应用已关闭时 `file-projects` 仍两次报"应用在运行"，新版会打印命中的进程以便下次定位。新增命令 `session-memory file-projects` 补归此前已装的会话。

## 本机目录自动匹配与跨机器路径改写（2026-10-27）

1. **路径改写补全**：会话装进本机时，除 `cwd`、`runtime_workspace_roots` 外，`turn_context.workspace_roots`、`world_state` 和环境说明块里的 `<root>…</root>`、`<cwd>…</cwd>` 也按"源目录→本机目录"改写（普通对话文字不动）。原因：A→B→A 回到 A 的会话里残留了 B 的路径（A 机实测 4 行）。已装好的旧会话不会被改，`adopt` 重装后生效。
2. **本机目录自动匹配**（`internal/engine/autodir.go`）：装会话时没有 `--cwd`、本机没有该会话的目录、也没有项目映射，且会话原目录在本机不存在时，找本机同名文件夹：先看 Codex 应用已有的项目文件夹（`local-projects.rootPaths`），再扫 `~/Documents`、`~/Desktop`、`~/Developer`、`~/Projects`、`~/Code`、`~/src`、`~/work`、`~/workspace`、`~/repos`、`~/GitHub`、`~/dev` 下最多 3 层（跳过隐藏目录、`node_modules`、`Library` 等）。恰好找到一个才用，计划里带 `cwd_auto_matched:<路径>` 提示；多个或没有都不猜，报错并说明原因。`project restore` 在项目未映射且没给 `--cwd` 时同样先自动匹配，找到就保存映射。
3. 测试：`internal/engine/autodir_test.go`（唯一匹配、多个不猜、应用项目列表优先、太深不找、根目录）、`paths_test.go`、`project_test.go`（无目录时报"no folder named …"、两个同名时报"2 folders named …"、唯一时不带 `--cwd` 成功并保存映射）。

## 一条命令接力（2026-10-27）

`session-memory relay`：离开前、到达后各跑一次，等价于 `sync` + 对本机每个已映射项目的 `project restore`（快进安装，装完让 Codex 登记）；只剩「新电脑上每个项目选一次本地目录」这一步。`TestRelayHandsWorkOverAndBack` 覆盖 A→B→A→B 两圈：B 首次提示映射、B 续写后回传、A 收到并装入（A 的原生文件里出现 B 的新内容）、空闲时无事可做、A 再续写后 B 同样快进。真实两机运行（A 机 Apple 芯片 + B 机 Intel，微云）已跑通一圈：A 纳管项目 → B 加入并恢复 → 两边在同一版本后各自续写形成冲突 → A 机 `choose` → B 机把旧分支文件移走后 `relay` 取到所选版本，历史完整、投影条目数与文件一致 → B 机新建的会话自动回到 A 机并装入 Codex。期间修了两处：本机文件已与目标版本字节一致时计划里的写入列表为 nil 导致规范 JSON 报 null（现在规范成空列表，并把绑定推进到目标版本避免日后分叉）；放弃分支的原生文件被用户移走后允许装入所选版本。随后补了两个命令：`adopt`（冲突后输掉的一台一条命令取所选版本，自己的副本备份、失败回滚；`TestRelayConflictChooseThenLoserTakesChosen` 覆盖整个流程含备份内容）与 `autosync`（launchd 每 2 分钟 `relay --quiet`，令牌存系统钥匙串，plist 里没有令牌）。A 机已真实启用，登录项无需环境变量即能完成一次完整同步（退出码 0）。未验证：长时间运行、休眠唤醒、Codex 运行中的后台安装是否总被正确挡住；B 机尚未启用。原先写的"未验证：定时/后台自动运行（目前需手动或由 agent 执行 `relay`）；真实 Codex 客户端里 A→B→A 的整圈。

## 1. 已实现并有测试

| 里程碑 | 内容 | 位置 |
|---|---|---|
| M0 | 命令行骨架（`version`、`config`、`agent probe`）、`errcode`/`logx`（含 lint 测试）/`failpoint`、CI 的 failpoint 任务 | `cmd/session-memory`、`internal/{errcode,logx,failpoint}` |
| M2 | zstd/plain `jsonl.Source`、合成夹具生成器、Codex 发现/探测/采集（孪生、分段、归档、压缩、半行、损坏行、同长度改写）、多 `CODEX_HOME` 配置、流式采集内存断言 | `internal/{jsonl,testutil/codexgen,source/codex,streamcap,config}` |
| M3 | 对象库、journal 与恢复、binding、`sessions.db`（含重建）、采集事务（崩溃矩阵）、原生恢复事务（备份/回滚，崩溃矩阵）、双设备回环 | `internal/{store,engine,sync}` |
| M4 | vault 加解密与加入包、凭据存储接口（系统钥匙串）、FolderTransport、发布/接收算法、隔离与 `device_id` 克隆检测、发布崩溃矩阵 | `internal/{vault,transport,sync,device}` |
| M5 | `init`、`transport attach-folder/attach-weiyun`、`device invite/join/regenerate-id`、`discover`、`track`、`list`、`status`、`checkpoint`、`sync [--watch] [--auto-install]`、`revisions`、`choose`、`project map/track/list/restore`、`restore [--plan]`、`open`、`verify [--deep]`、`auto-install`；冲突检测与落地 | `cmd/session-memory`、`internal/session` |
| M6（读取侧与规则） | WorkBuddy 适配器（发现含软删除、采集含旁路文件与 blob、探测）；Canonical 提取（Codex 主流/链接、WorkBuddy 活动路径）；`convert.Convert` 全部规则与保真度报告；`convert --plan`、`fidelity`、`continue` | `internal/{source/workbuddy,convert,canonical}` |
| M6（跨 Agent 转换写入） | `convert.Serializer`、`agent.MemSnapshot`、`engine.ConvertRestore`（与 `Restore` 共用 `runInstall`）、binding supersede、Codex/WorkBuddy 序列化、`convert` 命令；Codex CLI / app-server（本地桩模型）及 WorkBuddy GUI（真实模型）往返、重开验证 | `internal/{convert,engine,agent}`、`internal/source/{codex,workbuddy}/convert*.go` |
| M6（WorkBuddy 原生写入） | 手动路线 3、版本/schema 白名单、目标本地账号、文件及行级回滚、路径映射与续写往返；默认 opt-out | `internal/source/workbuddy/{writer,registry,runtime}.go` |
| M7（兜底） | Resume Pack（确定性，不依赖 LLM）、`resume-pack` | `internal/bridge` |
| P0 工具 | `dev sandbox/inventory/scrub/codex write/codex append/codex locks/report`（`-tags dev`，发布构建没有），沙箱安全守卫 | `internal/p0`、`cmd/session-memory/dev_*.go` |

端到端测试覆盖：两台“电脑”（独立状态目录、独立凭据存储、共享目录）上的 init → 纳管 → 同步 → 加入 → 接收 → 原生恢复 → 继续写 → 回传 → 快进，含路径映射、篡改/错误 vault/缺对象/序号缺口、克隆检测、conflict/需要处理、运行状态未知时拒绝安装。

## 2. 与规格的偏差

已回写 spec 的：

- `device.json` 增加 `vault_id`；新增 `device/outbox.json`（等待发布的 receipt/choice/convert-request）；新增 `store/revisions/<revision_id>.json`（本机已提交 revision 的事实源，`sessions.db` 重建需要它）。
- 安装事务的 `verified` 状态含义收紧为“文件已验证**且**待提交的 binding 已准备好”（崩溃矩阵测试发现旧定义无法前滚）。

仅记录（行为细节，不改变格式）：

- 对象先写成最终名（临时文件 + rename，内容寻址、幂等），采集事务的 `staged` 与 `stored` 两个状态因此实际上紧挨着；崩溃矩阵对两个状态点都验证。
- 原有故障注入主要用 error；新增 WorkBuddy 矩阵用 panic 保留中断现场、关闭并重开 store 后恢复。二者都不是 OS 断电实验；点位与 spec §13.2 对齐，另加 install.after_registry。
- Codex Writer 按 spec §8.3 的默认行为实现（只写会话文件、不碰数据库、仅未压缩 `.jsonl`），已通过真实 CLI / app-server 往返验证，Desktop GUI 仍未验收。存在厂商线程锁目录时，按线程锁判定运行状态；旧版缺锁目录时返回 `Unknown`，需显式 `--client-closed`；线程锁被持有时即使声明关闭也拒绝。
- 跨 Agent 转换写入（`convert` 不带 `--plan`）已实现：Serializer + 原生 Writer，见 spec §9.3 与 [p0-convert-report.md](p0-convert-report.md)；WorkBuddy 目标仍是版本受限的手动 opt-in，自动安装关闭。
- `engine.ToolMaps` 对两个目标都为空：所有工具调用/结果渲染为文本，直到外来工具名与参数形状经 E3/E4 验证。

## 3. 未完成及原因

| 项 | 原因 |
|---|---|
| Codex Desktop GUI 上的显示/继续/重开验证 | CLI / app-server 已通过；仍需独立 GUI 配置，不把日常客户端用于实验 |
| WorkBuddy 更广版本/路径/容量；转换的原生工具记录、附件、压缩边界 | 原生 Writer 已限量接通；转换写入已通（文本渲染）；原生工具记录/附件/压缩（E4/E6）仍待验证；WorkBuddy GUI 转换会话显示/续写/重开已通过 |
| 客户端运行检测与无人值守安装 | Codex 线程锁与 macOS WorkBuddy 进程检测已实现；WorkBuddy 没有已证实的厂商协作写锁，不允许无人值守安装；Windows/Linux 仍未实测 |
| 后台常驻（登录项注册、钥匙串解锁策略） | 设计 §6.4 第 6 点，M5 的系统集成部分 |
| `mcp serve`（兜底路径的只读 MCP） | M7，低优先级；Resume Pack 文件已可用 |
| Claude Code 适配 | P3；`internal/source/claude`、`checkpoint`、`conversation` 保留冻结（dev §7） |
| 微云真实行为验证 | E7，需要在真实微云同步目录上跑 |
| 真正双机与跨平台验收 | 当前是同一台 Mac 的隔离配置；清单已产出于 [acceptance.md](acceptance.md) |

## 4. P0 第一轮已完成（命令行级），见 [p0-report.md](p0-report.md)

结论摘要：Codex 路线 2（只写会话文件）在 app-server 级可行，原地追加可行，运行中无需重启即可看见，线程写锁是可靠的运行信号；路径映射还须改 `runtime_workspace_roots`（已实现）。第二轮已补真实 CLI 往返和 WorkBuddy GUI 实验，见本文前部与 [第二轮报告](p0-live-report.md)；Desktop GUI 及剩余 E3–E6 验收见清单。下面保留第一轮实验命令供复现。

### 复现与后续命令

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

最近一次全量运行（2026-10-27）：

- `gofmt -l`：无输出；
- `go vet ./...`，以及 `-tags failpoint`、`-tags dev`，并对 `GOOS=linux`、`GOOS=windows` 交叉 vet：通过（Windows 上的测试本身未在 Windows 实机运行，只在 CI 三平台矩阵里跑）；
- `go test ./...`、`go test -tags failpoint ./...`（含 CLI）、`go test -tags dev ./cmd/... ./internal/p0/...`：全部通过；本轮受影响包 `internal/engine`、`internal/source/codex`、`internal/source/workbuddy` 与 `cmd/session-memory` 的 race 测试全部通过；
- 崩溃矩阵：采集 10 个点、发布 9 个点、安装 11 个点，逐点注入失败 + 恢复后断言；
- WorkBuddy 新增/快进各 14 个中断点，共 28 项；包含 SQLite 登记提交后、binding 提交前的恢复；
- 大会话采集：144 MiB 的合成会话，峰值堆增长 26 MiB。
