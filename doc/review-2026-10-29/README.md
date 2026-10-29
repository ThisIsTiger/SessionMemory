# 项目审查与扩展测试（2026-10-29）

> **已修复 (A01-A04, A11, A12, A14)**：这七项已在业务代码中修复，对应 overlay 用例已移入正式测试集，不再出现在 `run_audit.py` 中；下文缺陷描述保留为原始审查记录。
>
> | 编号 | 修复要点 | 正式测试 |
> | --- | --- | --- |
> | A01 | 安装 journal 记录每个计划写入文件的哈希（`InstallData.Planned`）；`Recover` 发现文件既不是写入内容也不是备份时返回 recovery_needed，保留文件、备份与 journal，错误信息给出 journal 路径。实时 `RollbackTx` 不受影响，旧 journal 行为不变 | `internal/sync/recover_changed_test.go`（崩溃后追加不被抹掉；未改动时仍可回滚） |
> | A02 | 新存入的 segment 在写游标前重新校验来源，不一致返回 `streamcap.ErrChangedDuringCapture`，不保存游标 | `internal/streamcap/streamcap_test.go` |
> | A03 | Codex `Plan` 遇到未发布的额外 rollout 返回 NeedsAction，`Apply` 在厂商锁内复查；仅限 Codex | `internal/sync/restore_guard_test.go` |
> | A04 | 备用账号状态文件改为“出现即已知”；未知账号视为落后，先拉取（失败则本轮跳过）再推送全部历史；`StateFile` 为空时 `CatchUp` 不做事 | `internal/transport/multi_test.go` |
> | A11 | Folder transport 在 `init` 前同样检查“vault 存在但密钥被移除” | `cmd/session-memory/onboard_test.go` |
> | A12 | `--plan` 对任务项目不再创建目录；真实运行仍会创建 | `cmd/session-memory/project_test.go`（固定装置没有 WorkBuddy 注册库，安装本身停在“需先创建本地会话”，故只断言目录行为，未覆盖完整成功安装） |
> | A14 | `verify --deep` 对 blob 重算内容摘要 | `internal/streamcap/streamcap_test.go` |
>
> **后续已修复 (A05, A06, A07, A13)**：A05 由 `WriteInput.BindingState` + WorkBuddy Writer 的 blob 基线判断修复（正式测试 `internal/source/workbuddy/writer_test.go` 的 `TestManagedMeta*`）；A06 由 `canonical.Item.Subagent` + `convert` 的过滤与同流配对修复（`internal/convert/convert_test.go`、`internal/source/workbuddy/convert_test.go`）；A07 由 `SaveBinding` 写文件前检查唯一 active binding 修复（`internal/engine/engine_test.go`）；A13 由 `App.fail` 在已输出 JSON 文档后改写 stderr 修复（`cmd/session-memory/json_output_test.go`）。对应 overlay 已移出 `run_audit.py`。
>
> 仍待修复：A08、A09、A10、A15、A16（5 个失败断言，另有 1 个实机守卫用例默认跳过）。下文统计数字（16 个缺陷、54 个失败事件等）是修复前的审查结果。

## 结论与范围

基线：`65adeb18d3d9c49118751e3a84c7c6a93c72e7a7`，开始时工作区干净。审查覆盖 CLI/setup/安装脚本、采集游标、对象存储与索引、清单发布/接收、冲突选择、原生恢复/转换、双微云账号、额度退避与客户端运行守卫。没有修改业务实现。

**确认 16 个功能缺陷：P1 六个、P2 十个。所有缺陷均有隔离复现，带竞态检测连续执行三次，结果一致。** 其中 8 个是前两次审查中仍存在的问题，8 个是本轮补充发现。常规测试通过不能证明这些边界正确；当前仍有可能覆盖新增对话、永久漏采和缺少备份历史的路径，不宜把“开发完成”视为可靠性验收完成。

本轮只修改测试、增加测试与审查资料，没有修复下列业务缺陷。复现用例放在本目录的 `.go.txt` 文件中，通过 Go overlay 注入，避免让正常测试集因已知问题永久失败。

旧问题复核：R01 的 adopt 预览、R05 的十进制清单序号、R06 的未完成索引重建、R07 的采集失败上报、R09 的非法 KDF、R10 的项目 Watch 保留均已有相应修复，相关现有回归测试本轮通过。R11 的普通文本混入 JSON 路径已收紧，但失败后追加第二份 JSON 仍有 A13 的问题。本文不会把这些已修复分支重复计为未修复缺陷。

## 已复现的问题

### A01 / P1：Codex 安装崩溃后的恢复会抹掉随后新增的对话

- 位置：`internal/store/tx.go:252–267`；`internal/source/codex/writer.go:397` 附近的 GuardRecovery。
- 触发：首次安装或快进安装到 `install.after_written` 时 panic；向落地文件追加合法 `AFTER_CRASH` 记录；关闭 Store、重新打开，再使用真实 Codex 恢复守卫执行 Recover。
- 实测：Recover 成功返回。首次安装分支删除整个文件；快进分支用旧备份覆盖文件，新增记录均消失。
- 原因：守卫只检测运行状态和锁；回滚没有确认当前文件仍是本次安装写入的版本。
- 建议：对完整文件集合保存可恢复的前后哈希；检测到新增、改写或额外文件时，保留文件与 journal，返回 recovery_needed。
- 用例：`TestAuditRecoveryPreservesPostCrashContinuation/{create,append}`。对应旧报告 R02，仍未修复。

### A02 / P1：采集时等长改写会被新游标错误认证，造成永久漏采

- 位置：`internal/streamcap/streamcap.go:135–137`。
- 触发：sink 保存 `{"x":1}` 的 segment 后、游标计算前，把源文件等长改成 `{"x":2}`。
- 实测：已保存的是旧字节，游标却认证新文件；下一次采集 Changed=false，`x=2` 未进入对象历史。
- 原因：游标重新打开路径读取哈希，未绑定本次实际读取的文件身份和字节。
- 建议：在同一文件描述符/快照上读取与校验，在提交前验证来源；检测到并发改写则拒绝或重新采集。
- 用例：`TestAuditReplacementDuringCaptureIsNotLost`。对应旧报告 R03，仍未修复；影响两个使用 streamcap 的适配器。

### A03 / P1：恢复把未发布的额外 Codex rollout 纳入游标，却不生成 revision

- 位置：`internal/engine/restore.go:230–269`；`internal/source/codex/writer.go:289–329`。
- 触发：B 已安装旧版本；B 新增同 thread 的第二个 rollout，包含 `LOCAL_ONLY`，不 checkpoint；A 发布主 rollout 更新；B 执行恢复。
- 实测：恢复成功，adoption 收入额外 rollout，随后 checkpoint 报无变化。文件还在，但没有任何 revision 包含它，无法正常回传。
- 原因：Plan/Verify 只覆盖来源列出的路径，adoption 却采集目标线程的全部路径。
- 建议：安装前、锁内及 adoption 时核对完整 artifact 集合；有未纳管的本地进度则拒绝并提示先 checkpoint。
- 用例：`TestAuditRestoreDoesNotAdoptUnpublishedExtraRollout`。对应旧报告 R04，仍未修复。

### A04 / P1：新增的微云备用账号不会自动补齐已有历史

- 位置：`internal/transport/multi.go:120–126`。
- 触发：已有对象只在主账号和本地缓存；随后新增/切换第二账号 token，再走已有设备的 setup/relay（底层 Pull 与 CatchUp），没有重新 attach-weiyun。
- 实测：两步均成功，但只访问备用账号的新 reader 找不到历史对象。
- 原因：CatchUp 只处理 behind 状态中记录过写失败的账号。新账号没有失败记录，因此从不执行补传；从主账号读取历史也不会设置该状态。
- 影响：与“每个账号保留完整副本”的约定不符，主账号失效时旧历史不在备用账号中。
- 建议：账号首次加入/更换时建立待补齐状态，并比较或补传完整本地不可变集合；不能只补传新增写入。
- 边界：显式重新执行 attach-weiyun 会调用完整 Push，可补齐历史；缺陷是新增账号后通常的 setup/relay 不会自动做到这一点。
- 用例：`TestAuditMultiMirrorCatchesUpNewBackup`。

### A05 / P1：变化的 WorkBuddy 元数据阻断合法快进

- 位置：`internal/source/workbuddy/writer.go:325–327`。
- 触发：A/B 首次完整同步成功；A 仅更新 `.meta.json`，checkpoint 并传输 revision；B 没有任何本地修改，再次 Restore。
- 实测：返回 `needs_action: WorkBuddy target has local or unmanaged content`。
- 原因：Writer 对已存在 blob 要求来源与目标完全相等，而 CaptureBlob 支持采集变化内容。
- 建议：按绑定基线判断本地有无修改，允许合法 blob 备份和替换，同时保留并发修改保护。
- 用例：`TestAuditManagedMetaUpdate`。对应 WorkBuddy 旧报告 WB01，仍未修复。

### A06 / P1：子代理对话混入跨 Agent 转换的主会话

- 位置：`internal/source/workbuddy/extract.go:99–117`；`internal/convert/convert.go:60–61`。
- 触发：主流和子代理各有用户/助手记录，以 IncludeSubagents=false 执行转换。
- 实测：输出仍含 `SUBAGENT_PRIVATE_TASK`，以正常主会话用户轮次呈现。
- 原因：各流的活动路径均带空 Branch；没有保留可用于过滤的子代理归属，IncludeSubagents 选项从未被使用。
- 建议：保留流与父会话归属，在转换器按选项过滤并计入保真度报告；工具 callId 的配对应限制在同一来源流。
- 用例：`TestAuditSubagentsExcludedFromConversion`。对应旧报告 WB02，仍未修复。标记文本为合成数据，不是真实对话。

### A07 / P2：SaveBinding 返回冲突后，仍留下第二个 active binding 文件

- 位置：`internal/store/db.go:443–446`。
- 触发：已有 active binding，再保存同 session/agent、不同 BindingID 的 active binding。
- 实测：SQLite 唯一约束返回冲突，但 Bindings 从文件读取到两个 active binding。
- 原因：文件是事实源，却先写文件、后尝试可能失败的数据库镜像；失败时没有回滚文件。
- 建议：在持有 Store 锁的前提下先校验唯一性，或把事实源和视图更新纳入可恢复事务，失败时保持原状态。
- 用例：`TestAuditRejectedBindingLeavesNoFile`。现有 TestUniqueActiveBinding 只断言错误，没有断言失败后的文件状态。

### A08 / P2：索引重建静默丢掉损坏 revision，仍标记重建成功

- 位置：`internal/store/db.go:94–124`。
- 触发：正常采集一个 revision，然后破坏其事实源 JSON，再 Rebuild。
- 实测：返回 nil，Revisions 返回空列表，但原 binding 仍指向该 revision。
- 原因：解析失败被跳过，insertRevision/ApplyManifest/mirrorBinding 的错误也被忽略，refill 最后恒返回 nil。
- 建议：保留损坏事实源并汇总诊断，不能把有缺口的索引当成完整成功；对写入视图失败必须传播错误。
- 用例：`TestAuditRebuildReportsDamagedRevision`。旧 R06 的“未完成重建版本号”已修，但不覆盖此错误吞没路径。

### A09 / P2：微云 Put 把等长不同内容当作幂等成功

- 位置：`internal/transport/weiyun.go:734–738`。
- 触发：远端同一路径已有 `aaaa`；另一独立缓存的 Mirror 对该路径 Put `bbbb`。
- 实测：返回 nil，远端仍是 `aaaa`，本地却是 `bbbb`。
- 原因：只比较 file_size，违反 Transport 的“同路径不同内容必须返回 ErrExists”约定。
- 建议：利用内容哈希或下载比较验证远端相等；不得把文件大小当作内容相等证明。公共 vault 文件和不可变对象都应遵循一致契约。
- 用例：`TestAuditMirrorRejectsEqualLengthRemoteCollision`。用本地 HTTP 微云替身验证，未请求真实微云。

### A10 / P2：密钥文件部分发布成功后，缺失的备用副本永不重试

- 位置：`internal/transport/multi.go:261–265`；`internal/transport/weiyun.go` 的 Push 跳过 KeyPath 分支。
- 触发：主账号可写、备用账号额度耗尽；PutKey 返回成功；备用恢复后 CatchUp。
- 实测：备用账号仍没有密钥文件。
- 原因：PutKey 不记录部分失败，而普通 Push/CatchUp 又故意跳过密钥文件。
- 建议：单独记录每个账号的密钥发布状态和删除意图，对未完成的主动发布重试；不能依靠普通对象同步，避免恢复用户有意删除的密钥。
- 用例：`TestAuditMultiMirrorRetriesMissingKey`。

### A11 / P2：Folder transport 的 setup 失败后残留无关 vault 身份

- 位置：`cmd/session-memory/cmd_onboard.go:187–214`。
- 触发：共享目录已存在 vault，用户已移除云端密钥；新设备执行 setup。
- 实测：setup 失败，但新设备先执行了 init，写入新 VaultID 的 device.json；重新 setup 被当作已初始化设备，不能直接加入原 vault。
- 原因：创建新 vault 前，只有微云分支检查公共 vault 文件；Folder 分支跳过检查，先 init，后 attach 才发现 vault 不匹配。
- 建议：对两种传输一致地先确认 vault 是否存在，缺密钥时直接提示原设备 key publish，不创建新本地身份。
- 用例：`TestAuditSetupMissingFolderKeyDoesNotCreateForeignIdentity`。没有修改原共享目录的 vault 内容。

### A12 / P2：WorkBuddy 任务项目的 --plan 仍创建真实目录

- 位置：`cmd/session-memory/cmd_project.go:281–290`。
- 触发：新设备收到 WorkBuddy 任务项目，无本地映射，执行 project restore --plan。
- 实测：本机 `~/WorkBuddy/<时间戳>` 被创建；即使随后因 Writer 未启用而失败，目录也留下。
- 原因：autoTask 的 MkdirAll 发生在 planOnly 判断之前。
- 建议：计划阶段仅返回待创建目录和告警，在真正执行并通过安装前置条件后才创建目录。
- 用例：`TestAuditTaskProjectPlanDoesNotCreateFolder`。最新的普通项目预览修复未覆盖任务目录分支。

### A13 / P2：失败的 sync --json 输出两个 JSON 文档

- 位置：`cmd/session-memory/cmd_sessions.go:286–295`；错误最终由 App.fail 再写一次 JSON。
- 触发：已跟踪会话读取失败，执行 sync --json。
- 实测：非零退出码正确，但 stdout 先写统计对象，再写 error 对象，json.Valid=false。
- 建议：完成错误汇总后只输出一次统一结果/错误文档；部分统计放入结构化错误 details。project restore/relay 也有同类先输出再返回错误结构，需要一起检查。
- 用例：`TestAuditSyncFailureJSONIsOneDocument`。实测针对 sync；另外两个命令在此列为代码审查发现的同类风险，未单独计数。

### A14 / P2：verify --deep 不检查等长 blob 的内容

- 位置：`internal/streamcap/streamcap.go:270–275`。
- 触发：采集 `aaaa` 后，把 blob 改成同长度 `bbbb`，执行深度校验。
- 实测：异常列表为空。
- 建议：deep 模式对所有 blob segment 重新计算 RawHash，并验证区间和长度；不能只查大小。
- 用例：`TestAuditDeepVerifyDetectsEqualLengthBlobChange`。对应旧报告 R08，仍未修复。

### A15 / P2：WorkBuddy 旁路文件删除不被识别为变化

- 位置：`internal/source/workbuddy/workbuddy.go:257–293`。
- 触发：采集主流及旁路文件后，单独删除 `.meta.json`。
- 实测：新 artifact 集合从 4 个变成 3 个，Changed 却为 false；Checkpoint 因无变化保留旧快照。
- 建议：比较新旧 artifact 集合，明确删除的版本化、目标安装与回滚语义，不能仅汇总新增字节。
- 用例：`TestAuditRemovedArtifactChange`。对应旧报告 WB03，仍未修复。

### A16 / P2：WAL 中已提交的软删除仍被 Discover 列出

- 位置：`internal/source/workbuddy/workbuddy.go:113–119`。
- 触发：打开 WAL，禁用自动 checkpoint；建立会话后 checkpoint；提交 deleted_at 更新，保持连接打开。
- 实测：普通查询确认软删除 1 条，Discover 仍返回该条。
- 原因：immutable 读取活动库看不到这个已提交 WAL 状态；当前规格要求 immutable，也需要一起修订。
- 建议：选择能读取已提交 WAL 的只读方案，并验证不改写厂商文件的约束。
- 用例：`TestAuditSoftDeleteInWAL`。对应旧报告 WB04，仍未修复。

## 正常测试、故障注入、模糊测试与实机检查

最终执行结果和覆盖率见同目录 `results.json`。本轮使用 `-count=1`，没有把缓存结果当作重新测试。16 个缺陷使用 `-race -tags dev,failpoint -count=3` 复现，没有出现 DATA RACE。

| 执行矩阵 | 结果（计入子测试事件，不是互不重复的功能数） |
| --- | --- |
| 全项目默认配置 | 282 通过，3 个 live 用例跳过 |
| 全项目 `-race -tags dev,failpoint` | 382 通过，3 个 live 用例跳过，无竞态报告 |
| 最后一次全项目 `-tags dev,failpoint -coverpkg=./internal/...,./cmd/session-memory` | 388 通过，3 个 live 用例跳过；包含后来新增的安装器用例 |
| 缺陷 overlay，race 模式重复 3 轮 | 16 个不同顶层断言稳定失败；连同两个崩溃子例，累计 54 个失败事件 |

跨包统计合并各测试进程的相同覆盖块，语句覆盖率为 **78.0%（7,283 / 9,335）**；其中 engine 81.4%、streamcap 86.2%、store 74.5%、Codex 80.0%、WorkBuddy 80.4%、sync 78.7%。普通不跨包统计的默认覆盖率为 65.8%，会漏算由别的包测试间接调用的代码，因此不能把它显示的 streamcap 0% 当作没有执行过采集代码。覆盖率说明执行范围，不证明未测错误分支正确，也不包含另列的预期失败 overlay。

本轮正式增加的正常测试：

| 文件 | 内容 |
| --- | --- |
| `internal/sync/handoff_test.go` | 3 个固定随机种子，每个 12 次三设备接力；逐轮比较本地字节与独立模型、快照、父 revision、幂等发布/接收、无伪 checkpoint；每 4 步重建索引 |
| `internal/session/property_test.go` | 40 组随机森林，各 75 个 revision；随机到达顺序和缺失祖先，对照独立模型验证 heads、pending、全部节点对祖先关系与选择结果 |
| `internal/jsonl/fuzz_test.go` | 完整行、部分尾行、JSON 损坏、超长行和游标边界；返回字节必须与原文完全一致，错误不能推进游标 |
| `internal/vault/fuzz_test.go` | 密文畸形输入、对象类型/AAD 隔离、逐位篡改拒绝、合法密文往返；非法 KDF 参数必须先拒绝，避免 fuzz 本身分配昂贵 Argon2 内存 |
| `cmd/session-memory/install_test.go` | 本地 HTTP 下载、正确校验、错误校验、缺少校验项、HTTP 404、含空格安装路径与启动 setup；错误时必须保留旧安装 |
| `cmd/session-memory/main_test.go` | 修复测试隔离：使用每个测试独立的内存凭据，清除微云/Agent 相关环境变量 |

初始全量测试确实失败于 TestSetupNeedsAConnection：它读取了宿主机的备用微云凭据，错误地认为已有连接。修复的是测试隔离，没有改动产品的凭据发现规则；修复后该用例和完整测试均通过。安装器的新增测试在完整矩阵之后加入并单独通过 race 验证，后续跨包覆盖率全量执行也包含它。

验证分层：

- 全量默认测试、带 dev/failpoint 的全量 race 测试均通过；覆盖原有采集、发布、接收、安装、WorkBuddy 转换的中断矩阵。panic/error 注入不等于真实断电。本轮 Codex 崩溃保护反例额外关闭并重新打开 Store 验证。
- 3 个 fuzz 目标各运行 25 秒，合计 3,602,126 次执行，没有发现崩溃或断言失败。它们验证具体性质，不代表遍历了所有协议输入。
- go vet（含 dev/failpoint）、gofmt、git diff --check、sh -n install.sh 通过。
- Linux amd64、Windows amd64 的交叉 vet 和构建通过；Darwin amd64 也构建通过。它们是编译/静态验证，没有在这些平台实机执行。
- macOS arm64 的 release/dev 构建通过，release 帮助无 dev 命令，dev 构建包含 dev 工具。
- 真实 Codex CLI 0.162.1 的 app-server 对临时 home 中的合成 rollout 完成 thread 登记（TestRegisterLive），不调用模型。
- WorkBuddy 5.9.3 的只读结构探测 compatibility=ok，发现 98 个主会话、抽样 20 个；真实版本/进程检测能拒绝向临时目标安装，即使 ClientClosed=true。
- 本轮未请求真实微云账号、没有注册/卸载真实 launchd 作业、没有退出用户客户端；WorkBuddy GUI 导入显示、真实模型续写及真实两台电脑/休眠唤醒不在本轮已验证范围。

## 性能观察（不计入 16 个功能缺陷）

`BenchmarkGraphLinearHistory` 使用每种长度 3 次迭代，Apple M1 Pro / macOS arm64：

| 线性 revision 数 | 每次构图耗时 | 每次构图累计分配 |
| --- | --- | --- |
| 100 | 约 0.58 ms | 约 0.50 MB |
| 1,000 | 约 59.5 ms | 约 44.7 MB |
| 4,000 | 约 1.05 s | 约 705.7 MB |

这不是峰值常驻内存，也不是统计充分的容量上限测定。NewGraph 为每个节点重复递归遍历整条父链、分配 seen map，在线性历史上呈二次增长。建议在数据安全缺陷之后，使用记忆化/迭代状态标记优化，增加更长历史与多 session 的性能回归。

## 复现、维护与建议顺序

从仓库根目录运行：

```bash
# 正常测试：应通过
go test -count=1 ./...
go test -race -tags dev,failpoint -count=1 ./...

# 已知缺陷：当前应非零退出；失败不是测试安装错误
python3 doc/review-2026-10-29/run_audit.py --race --count 3

# 定向随机/性质测试
go test ./internal/sync -run TestRandomizedThreeDeviceHandoffs -count=1
go test ./internal/session -run TestGraphAgainstRandomForests -count=1

# 每次只运行一个 fuzz 目标
go test ./internal/jsonl -run '^$' -fuzz '^FuzzReaderBoundaries$' -fuzztime=25s -parallel=2
go test ./internal/vault -run '^$' -fuzz '^FuzzObjectAuthentication$' -fuzztime=25s -parallel=2
go test ./internal/vault -run '^$' -fuzz '^FuzzBundleRejectsInvalidKDF$' -fuzztime=25s -parallel=2
```

run_audit.py 自动生成 overlay 与日志，打印结构化汇总和日志位置。修复 A01–A04、A11、A12、A14 之后，当前预期 5 个不同顶层失败测试（A08、A09、A10、A15、A16），每轮 5 个失败事件、1 个跳过（实机守卫）；修复前为 16 个失败测试、18 个失败事件、三轮 54 个。真实 WorkBuddy 守卫默认跳过，可用已记录的单独实机结果判断，不要把跳过当通过。若 summary 的 build_errors 非空，先处理构建错误，不能把它算成缺陷复现。

建议先修 A01–A03，避免覆盖和漏采；随后修 A04–A06，保证备份和跨 Agent 接续语义；再处理失败操作的一致性、JSON 协议、只读预览与完整校验。将相应 overlay 断言移入正式测试集后，再执行同一套矩阵。已有失败反例不能因其他正常测试通过而视为解决。
