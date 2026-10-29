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

已实现默认关闭、限 macOS / WorkBuddy 5.x / 已知 schema 的手动原生 Writer：目标账号与本地私有设置、文件和登记行基线、路径映射、进程检测、备份/journal、行级 CAS 与回滚、adoption 后完整后置条件验证。CLI `restore --agent workbuddy` 已接通，默认 home 改为 WorkBuddy 配置，不再落到 Codex home。

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

## WorkBuddy 库结构指纹改为与列顺序无关（2026-10-28）

**问题**：B 机真实测试（WorkBuddy 5.8.3）写入被拒：`WorkBuddy registry schema is outside the allowlist`。原因：原指纹是隔离夹具里 `PRAGMA table_info(sessions)` 的**有序**哈希，而真实安装经过多次迁移，同样的 40 列、相同类型/非空/默认值/主键，但**列的顺序不同**（A、B 机真实库互相一致，与夹具不同）。写入器按列名读写，顺序无关。

**修复**：指纹改为先对每列 `名称:类型:非空:默认值:主键` 排序再取哈希（新值 `f8b32580…a680`），夹具与 A 机真实库的哈希相同；多一列、少一列、改类型/非空/默认值仍然拒绝。A 机真实库只读验证通过。测试：`registry_test.go`（两种列顺序都通过；额外列、改类型、改非空、改默认值都被拒绝）。

## relay / autosync 支持 WorkBuddy（2026-10-27）

`relay` 不带 `--agent` 时处理 Codex，并在用户开启 `agents.workbuddy.allow_registry_write` 后同时处理 WorkBuddy（`relayAgents`）；`--agent X` 只处理 X。后台自动同步运行的是不带 `--agent` 的 `relay --quiet`，所以开启后 WorkBuddy 也随之自动更新。每个项目按各 Agent 分别安装（会话属于哪个 Agent 就由哪个装入，另一个计为"其他 Agent"跳过），结果合并进同一行。

**安全条件**（没有放宽）：WorkBuddy 写入器本来就要求声明"应用已关闭"；后台没有人声明，relay 只在进程检测明确是"未运行"时才替它声明（`relayClientState`），而写入器在写之前还会再查一次进程、版本（仅 macOS 5.8.3）和库结构。应用在运行或状态未知时，这些会话计为"等待应用关闭"，下一轮应用退出后自动补上。默认关闭，不开启则完全不碰 WorkBuddy。`sync --auto-install` 和 `agents.workbuddy.auto_install` 的语义不变（仍不装 WorkBuddy）。

测试：`relay_agents_test.go`（Agent 选择、各运行状态下的关闭声明）；`TestRelayHandsWorkOverAndBack` 在两台电脑都开启 WorkBuddy 的情况下跑完 A→B→A，确认 Codex 接力不受影响。未覆盖：真实 WorkBuddy 应用上的无人值守安装（写入器依赖固定的 `/Applications/WorkBuddy.app`，不能在 CI 里命令行级验证；写入器本身的往返和 28 个中断点测试仍在）。

## 云端密钥：新电脑不用口令也不用邀请包（2026-10-27）

**取舍**：加密的目的是防"随手看一眼"（网页预览、搜索、分享链接），不防能登录这个微云目录的人，所以不引入口令。密钥文件和数据放在同一个云目录里，防护等同于云盘登录本身。

**做法**：
1. `transport attach-weiyun` / `attach-folder` 把本机的 vault 密钥以 `public/vault-key.json`（格式 `smkey1`）放进云目录（已有就不动；用户删过则不再自动放回）。
2. 新电脑：`session-memory device join --from-cloud`（可加 `--weiyun <目录名>`），只需要微云令牌，读到密钥后用云目录里的 `vault.json` 校验、保存到本机钥匙串并建立设备身份。邀请包加口令的方式仍可用。
3. 加入后的第一次前台 `sync` / `relay` 提示一次：密钥仍在云端，所有电脑都加入后可以删除。有人在终端时询问 y/N；Agent 或脚本只打印，由 Agent 问用户，同意后运行 `session-memory key remove --yes`。后台 `relay --quiet` 不提示。
4. `key remove` 把云端密钥文件移到微云回收站（不是永久删除）；已加入的电脑不受影响（密钥在各自钥匙串里），新电脑要加入需先 `key publish`。`key status` 查看云端有没有。
5. 云端密钥文件不走普通同步：`Mirror` 的 `Pull`/`Push` 都跳过它，只有 `PutKey`/`GetKey`/`DeleteKey` 碰它，所以另一台电脑的本地缓存不会把已删除的密钥再传回去。删除的标记是每台电脑各自记的。

测试：`weiyun_test.go`（密钥文件独立于普通同步、删除后不被 Push 还原）、`key_test.go`（B 无邀请包加入并读到 A 的会话、提示只说一次、无人确认时拒绝删除、删除后已加入的电脑照常同步、新电脑加入要先 `key publish`）。

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

## WorkBuddy：版本放宽与「任务」会话

**版本**：写入器不再要求 WorkBuddy 恰好是 5.8.3，改为 5.x 系列都可以（A 机已升到 5.9.3，sessions 表指纹与 5.8.3 完全相同）。每次写入前仍检查：表结构指纹、无触发器、会话文件格式、应用未运行。大版本变化（6.x）或读不到版本时拒绝。

**任务与空间**：WorkBuddy 把「不属于任何项目」的会话（任务）放在自己建的 `~/WorkBuddy/<年-月-日-时-分-秒>` 目录里，登记行 `is_playground=1`，界面归在「任务」；其余会话是「空间」里的项目。此前同步到另一台电脑的任务会话用了表默认值（`is_playground=0`），所以被当成项目放进「空间」。现在：
- 来源目录是这种任务目录、目标目录也是时，新建的登记行写 `is_playground=1`、`mode=craft`、`source_mode=craft`（和这类会话在原机上的取值一致）；真正的项目目录不受影响。
- 目录自动对应：任务目录在另一台电脑的用户目录下时，自动对应到本机 `~/WorkBuddy/<同名>`，安装时建立（预览不建），不再需要 `--cwd`。
- relay 会为收到的、尚无本机目录的任务项目自动建立映射，接收端不用任何命令。

**已装好的会话不会自动改**：之前以 `is_playground=0` 装进去的行需要手动改一次（见本次说明）。

## 微云额度与后台同步的调用量

微云每天有调用额度，两台电脑共用同一个账号。后台同步每 2 分钟一轮，原先每轮都要列出整个云端目录（含最大的 `objects`），额度很快耗尽，之后两台电脑的后台同步都会报 `code:117401 … daily quota used up`。

- **额度用完后暂停**：识别到该错误（`transport.ErrQuota`）后，记到 `<状态目录>/weiyun-quota.json`：第一次拒绝，后台运行（`relay --quiet`）停到次日 00:05；到点再试一次，还被拒就再多等 1 小时（每次递增，最多 6 小时）；任何一次调用成功就清除。你手动运行的 `relay` 不受暂停影响，仍会尝试。`autosync status` 显示暂停到几点。
- **按目录列出**：轮询新 manifest 只列云端根目录和 `manifests`，不再列 `objects`；`objects` 只在确实缺对象、要下载时才列。Push/Pull 的全量路径不变。
- **安静时自动放慢**：后台连续 3 轮没有任何变化（没发布、没收到、没采集、没安装）后，间隔依次放到 4、8、10 分钟；有任何变化立即回到 2 分钟。`autosync status` 会写出当前间隔。

## 两个微云账号互为备份

一个微云账号有每日额度，用完就整体停摆。现在可以配两个账号，各存一份完整的库，其中一个出问题，另一个继续同步。

- **配置**：`WEIYUN_MCP_TOKEN` 是主账号（primary），`WEIYUN_MCP_TOKEN_2` 是备份账号（backup）；只配一个就和以前一样。`transport attach-weiyun` 和 `autosync enable` 会把环境里的令牌存进系统凭据库，后台运行读它们。两个账号里的目录名相同（默认 `SessionMemory`），共用本机同一份缓存。
- **写**：每个新文件写到所有能写的账号；某个账号这次没写成（额度用尽、网络错），记到 `<状态目录>/weiyun-behind.json`，之后它能用时由下一轮同步（`CatchUp`）按本地缓存补齐。至少一个账号成功就算成功。
- **读**：从所有能读的账号取并集（所以另一台电脑只写进了备份账号的东西，这台也能看到）。轮询只列根目录和 `manifests`。全部账号都失败才报错。
- **额度**：暂停状态按账号分别记（`weiyun-quota.json`）。某个账号用尽只绕开它，另一个照常；两个都用尽，后台运行才整体暂停到次日 00:05，之后每次递增 1 小时（最多 6 小时）再试。`autosync status` 会逐个账号说明。
- **云端钥匙**：`key remove` 会从所有账号里移除，任一账号没清掉就报错，不会假装完成。
- **两个账号各自的调用量**：每个账号的轮询量和单账号时相同（各一次根目录加 `manifests`）；安静时同样放慢到 10 分钟一次。

## git 调用有超时

后台同步曾被一个不返回的 `git` 卡死 10 分钟以上：采集时检查代码状态要调 `git`，而它在持有存储锁的情况下一直不返回，之后所有同步和命令都报 `another session-memory is running`。现在每次 `git` 调用最多 60 秒，超时即杀掉，该项代码状态记为 `unavailable`（与 git 不可用时的既有降级相同）。

## 轮询只剩一次调用

之前每个后台进程都要先列账号根目录、再列 vault 目录，才知道 `manifests` 的目录键，一轮轮询至少 3 次调用。现在目录键记在 `<状态目录>/weiyun-keys-<账号>.json`：轮询新 manifest 只剩列 `manifests` 目录这一次（manifest 超过 50 个时每多 50 个再加一次）。记下的键失效时（目录被换掉），第一次出错就丢掉重新学一遍。账号根目录里每多一个文件（比如放二进制包）也不再拖慢轮询。

## 一步接入（setup）与安装页（2026-10-29）

目标：普通用户只把一个链接发给 AI 助手，之后什么都不用想；每个对用户可见的步骤都必须是必要的。

- `session-memory setup`：无提问、可重复运行（可能要几分钟；被打断后重跑即可）。依次：检查微云（环境变量 → 已保存 → 微云 skill 的 `~/.mcporter/mcporter.json`，三处都没有则输出 `NEEDS_WEIYUN` 并给出 skill 安装指引）→ 检测 Codex / WorkBuddy（一个都没有就报错，此时云端什么都没创建）→ 微云里已有同步库就用云端密钥加入（`device join --from-cloud`），没有就 `init` + `attach-weiyun`；云端有库但密钥被移除、或读云端失败时明确报错，绝不新建第二个库 → WorkBuddy 自动打开 `allow_registry_write` → 把最近 `--days`（默认 60）天、工作目录存在且不是 home 的会话按目录建项目（只在本地，不上传；输出“N covered, M skipped (no project folder)”）→ 写下标记 `track-new-folders` → 开后台同步（沙盒里 launchd 加载失败时在桌面留 `开启同步.command`，输出 `NEEDS_USER`）→ `relay`（由它把所有未上传的内容一次发布）。
- 最后一行说明结果：`done: FIRST_COMPUTER`（微云里第一台；若用户其实配过别的电脑，多半是登录了另一个微云账号）/ `done: JOINED` / `done: ALREADY_CONNECTED`。relay 因微云额度或网络失败时，输出 `PENDING: connected; the first upload will finish by itself` 并以 0 退出，由后台同步接着完成；因 `recovery_needed` / 会话需要处理（冲突、分叉、被中断的安装）失败时，输出单独的 `NEEDS_ACTION: N sessions did not sync and will NOT fix themselves …`（含受阻会话数和下一步），同样以 0 退出（已连接、其他会话照常同步），但不承诺自动恢复；其他错误照常报错。`NEEDS_*` 里的命令都用程序的完整路径。
- 标记 `track-new-folders`（状态目录）存在时，每次 sync 还会在本地把新文件夹里的新会话纳入（`trackFolders(..., onlyNew)`：已在某个项目目录内的子文件夹不新建项目，home、`/`、已不存在的目录跳过），随同一轮上传，不增加微云调用。没跑过 setup 的用户行为不变。
- 云端密钥放不进去时（`attach-*` 的警告分支）写 `key-publish-pending`；setup 和手动 relay 看到它就补放一次，成功或已存在则清掉。用户主动移除过密钥（`key-removed-from-cloud`）时不会放回。
- 令牌优先级不变：环境变量 > 已保存 > skill 配置（只在前两者都没有时用，并会保存下来供后台使用），避免已切换的账号被旧 skill 配置覆盖。
- relay 现在也会把“恰好只有一个同名文件夹”的项目自动映射，不再只限 WorkBuddy 任务目录；还没有文件夹的项目只说一句“有 N 个来自其他电脑的项目暂时没有对应文件夹（名字…），有了同名文件夹会自动加入”，后台静默运行不再因此每次输出。
- `project restore` 不写 `--agent` 时按项目会话的来源自动选；`--plan` 不再写入映射，输出 `would install`。
- 分发：`install.sh`（下载 → sha256 校验 → `~/.local/bin` → `setup`），`.github/workflows/release.yml` 在打 `v*` 标签时构建 darwin/linux × arm64/amd64 并附 `checksums.txt`，并在仍有 `OWNER/` 占位时让发布失败（占位已替换为 `ThisIsTiger/SessionMemory`，该检查保留作保险）；`INSTALL.md` 是给 AI 助手的执行说明（列出程序输出的每个标记及处理办法），README 顶部是给用户复制的一句话。只支持 macOS（Linux 没有后台同步）。
- 对话保存在用户自己的微云里，保护程度等同于其微云账号；云端密钥与数据同处一个文件夹，不是对账号持有人的强加密。
- 待办：仓库已公开（https://github.com/ThisIsTiger/SessionMemory），但尚未打 `v*` 标签发布，install.sh 要等第一个 release 才能下载到程序；许可证为 MIT；没有同名文件夹的空间项目（如 GEO）仍需指定目录；未在真实第二台电脑上端到端验证（单元测试用文件夹传输和假微云模拟）。

## 复审修复缺口补齐（2026-10-29）

- **A01 / F01**：旧 journal 缺少 `Planned` 时，用 `Written`、`Backed`、`Created` 的完整集合核对恢复安全性；无法确认的现存文件保持原样并返回 recovery_needed。已修改的旧会话不再被自动覆盖/删除，未改动的旧 journal 仍可安全回滚。
- **A02 / F02**：plain 流有新数据时校验完整已消费前缀，用同一条已校验流生成游标；没有新字节时沿用旧游标。CursorBuilder 只缓存头尾窗口；有变化的捕获增加一次完整前缀读取，不增加整会话内存缓冲。
- **A04 / F03**：Pull/List/Get 下载普通文件前持久化待补传状态；下轮 CatchUp 补齐副本，支持重启、额度暂停恢复及空闲无额外远端请求。首次升级以 `<StateFile>.received-v1` 标记完整远端并集已核对，重新核对旧 level 状态并补齐修复前已缓存的历史；并集未读全时不记录升级完成。密钥仍走独立的发布/删除流程。
- 补充正式回归覆盖旧 journal、增量旧段改写、游标分块边界、跨设备接收、重启补传、状态落盘失败与升级补齐。验证记录见 [复审修复说明](review-followup-2026-10-29/FIXES.md)。其余明确延期的 A05–A10、A13、A15、A16 不在这次修复范围（其中 A05–A07、A13 已在下一节关闭）。

## 发布前审查修复（2026-10-29）

- **A05**：`WriteInput.BindingState`（引擎在快进时传入活动 binding 的采集状态）。WorkBuddy Writer 对已存在的 blob（`.meta.json` 等）只在本机文件仍等于 binding 记录的内容（`streamcap.BlobDigest`）或已等于目标内容时才替换，计划里标为 `replace`；本机改过则仍返回 NeedsAction，保持并发修改保护。
- **A06**：`canonical.Item.Subagent` 记录来源子代理流；`convert.Convert` 在 `IncludeSubagents=false` 时丢弃并计入报告（`dropped_types.subagent`），工具 callId 只在同一来源流内配对。
- **S02**：带 `WyHeader` 的 MCP 请求只允许同 scheme、同 host:port 的重定向，其余拒绝；下载链接（不带令牌）仍可跟随 CDN 重定向。
- **S03**：传输层错误边界去掉 URL 的路径、查询和用户信息（只留 scheme://host，保留错误类别），服务端错误文本里的已知令牌替换为 `[redacted]` 并限长；额度（`ErrQuota`）和重试判断不受影响。
- **U01**：保存的（Keychain）主令牌被微云拒绝（`ErrAuth`，HTTP 401/403 或明确的令牌失效消息）时，客户端只尝试一次 `Reauth`：skill 的 mcporter 令牌与之不同、且用它能读到本机同一个 vault（`public/vault.json` 的 vault_id 相同，只读、不创建目录）才改用并存入 Keychain；环境变量令牌和备用账号不被替换，不同账号不会被静默切换。没有新增命令。
- **U02**：见上面的 `NEEDS_ACTION`；`INSTALL.md` 与标记检查测试同步。
- **S01**：`go.mod` 升到 `go 1.26.10`（1.26 分支当前最新安全补丁，setup-go 可下载）；govulncheck v1.9.0 对 `./...` 报告 0 个符号可达漏洞。release 工作流在构建前运行 `go vet ./...` 和 `go test ./...`。
- **F03 复核**：MultiMirror.CatchUp 记录每个账号是否已完整读取（`<StateFile>.read`），升级期间无法联系的账号不再导致每轮都清除状态并对健康账号强制全量拉取；`.received-v1` 仍表示完整并集已核对、旧标记已清除。
- **A07**：`SaveBinding` 在写文件前检查唯一的 active binding，冲突时不留下文件。**A13**：`--json` 下命令已输出结果文档后再失败，错误只写 stderr，stdout 保持一个文档。
- 仍延期：A08、A09、A10、A15、A16。

## 认领云端已有的会话（2026-10-29，reset / 重装之后不再生出重复会话）

问题：本机状态丢失（`reset`、重装）后，`setup` 把本机原生会话都当新会话追踪；其中许多已在云端（本机自己发布过，或从另一台安装来，两边原生 id 相同），结果云端出现重复会话，另一台收到自己的会话又装不上，relay 永远 `recovery_needed`。

做法（`internal/engine/claim.go`，`Engine.Claim`；`cmd_claim.go` 的 `trackOne` 在 setup / `project track` / 同步里代替直接 `Track`）：
- **只读元数据**：`Sync.PeekRevisions` 读其他设备尚未导入的 manifest（不导入、不写 store、不下载任何 segment），`FetchSnapshot` 只取被比较的 revision 的 snapshot 小对象（链接成功后再取该 revision 的 repo-state）；微云上用新增的 `transport.Fetcher.Fetch` 按单文件下载（`Get` 会把 `objects/` 里缺的全拉下来）。setup 第一个原生会话需要时才读一次；读不到云端清单时 setup 中止而不追踪，宁可重跑也不制造副本。
- **候选**：同 agent、首个 revision 的 `origin_native_id` 等于该原生 id、且本机没有它的 active binding 的 session；多个时取最老的（早先 setup 造成的副本更年轻）。
- **精确校验**：原生文件的 artifact 集合、各 artifact 大小与某个 revision R 的 snapshot 完全一致，并用 R 自己的分段边界重新哈希原生字节（`VerifyBinding(deep)`，流用 segment `raw_hash`，blob 含 0 字节文件用 segments digest）。优先最新的匹配 R（可以是祖先，之后 relay 快进）。每个 session 最多比较最新 12 个 revision；若原生文件已超出最新 revision（历史单头），只取这一个 snapshot 就停。
- **绑定**：匹配后按安装后的形态建 binding（`ad.Capture` 取游标，`BaseRevisionID = InstalledRevisionID = R`，`InstallNative`），不产生 revision、不上传任何对象，只往 outbox 放一条 `installed` 回执；R（及根 revision）预先记入本机 store（`Store.AdoptRevisions`），之后的正常拉取会再写同样的行。一次原子的 binding 文件写入，崩溃矩阵测试覆盖。
- **不匹配则不变**：有未发布进度、内容不同、被篡改/半行/截断、无候选，一律回到原来的 `Track`（仍会新建一个 session，是已知的剩余情形）。仅凭原生 id 从不认领；不改原生文件。
- 输出：`N session(s) were already in your Weiyun and are now linked (nothing was uploaded)`（不是标记，INSTALL.md 不变）。被认领会话的项目 ref 来自另一台，setup 会把它们也映射到该文件夹，以便之后 relay 更新。

未覆盖：从另一台**安装**来、且安装时改写过内容（cwd 路径映射、Codex 的 model_provider、WorkBuddy 的 `projects/<目录>` 路径）的原生会话，字节与云端 revision 不同，仍会被当作新会话（需要「按 Writer 计划比对」的第二层，要下载该会话全部 segment）；已经存在于云端的重复会话不会被合并或清理。
