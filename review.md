# SessionMemory 代码审查报告

- 审查日期：2026-10-27
- 审查范围：当前工作区，包括未提交改动；重点检查 CLI、同步、采集、原生安装、崩溃恢复、存储及加密边界。
- 结论：确认并通过隔离测试复现 11 个问题，其中 P1 级 6 个、P2 级 5 个。
- 本轮仅审查和记录，未修复以下问题；复现测试通过临时 Go overlay 注入，没有写入仓库测试文件。

## 严重程度

- **P1**：可能丢失会话内容、漏同步数据，或阻断恢复，需要优先修复。
- **P2**：错误提示、校验、配置更新或输出协议存在缺陷，应补充回归测试后修复。

## P1：优先修复

### R01：`adopt --plan` 会移走真实会话文件

**位置**：[cmd/session-memory/cmd_adopt.go:102–137](cmd/session-memory/cmd_adopt.go#L102)

**原因与影响**：命令在读取和处理 `PlanOnly` 前，已经执行 `os.Rename`，将本地会话移入备份目录。随后 `Restore` 只生成计划并成功返回，命令没有撤销移动，反而输出已经切换成功。原生会话文件消失，旧内容虽然仍在备份中，但预览操作已经改变了实际状态。

**复现**：

1. 两台模拟设备从同一版本分别继续会话，形成冲突。
2. 在 A 设备选择 A 的版本。
3. 在 B 设备执行 `adopt <session_id> --client-closed --plan`。
4. 命令返回成功，但 B 原来的 rollout 路径已不存在，新版本也没有安装。

**建议**：在任何移动、采集发布或安装副作用之前处理预览模式。为 `adopt --plan` 增加文件内容、路径及绑定不变的回归断言。

### R02：安装崩溃后的恢复会覆盖新增对话

**位置**：

- [internal/source/codex/writer.go:356–366](internal/source/codex/writer.go#L356)
- [internal/store/tx.go:252–268](internal/store/tx.go#L252)

**原因与影响**：恢复保护只检查客户端运行状态并获取锁，没有检查目标文件在安装中断后是否被继续修改。回滚会直接恢复旧备份，或删除首次安装创建的文件，导致崩溃后新增的本地对话丢失。

**复现**：

1. 已安装会话收到远端更新。
2. 使用 `install.after_written=panic` 中断安装，停在 `written`、尚未 `verified` 的状态。
3. 在目标 rollout 中追加合法 JSONL，模拟用户继续会话后关闭客户端。
4. 清除故障注入并执行带 `codex.GuardRecovery` 的恢复。
5. 恢复返回成功，但追加内容被旧备份覆盖。

**建议**：持锁后核对待回滚文件与安装记录中的内容。发现安装后新增或改写时，保留原文件及 journal，返回需要人工处理的错误，不得直接覆盖或删除。

### R03：采集期间源文件替换会造成永久漏采

**位置**：[internal/streamcap/streamcap.go:125–138](internal/streamcap/streamcap.go#L125)

**原因与影响**：segment 保存完成后，`NewCursor` 重新打开源路径计算游标哈希。如果此时文件发生等长改写或原子替换，segment 保存的是旧内容，游标却认证新内容。下一轮采集误判内容未变，新内容被跳过；后续追加还可能与旧 segment 拼接。

**复现**：

1. 初始文件为 `{"x":1}\n`。
2. 测试 sink 保存 segment 后，将源文件改为 `{"x":2}\n`。
3. 使用返回的游标再次采集。
4. 第二次报告未变化，但已保存的内容仍是 `x=1`。

**建议**：游标必须对应实际采集的字节，不能直接认证重新打开路径后读到的内容。统一读取和验证所使用的文件身份，并在提交采集状态前检测换代或改写。

### R04：恢复会将未同步的本地分段误标为已采集

**位置**：

- [internal/source/codex/writer.go:242–253](internal/source/codex/writer.go#L242)
- [internal/engine/restore.go:227–267](internal/engine/restore.go#L227)

**原因与影响**：安装计划只检查源快照列出的路径，而安装后的 adoption 会采集整个线程。如果本地存在远端快照之外的新分段，其游标也会被提交到远端 revision 对应的绑定。下一次 checkpoint 认为没有变化，但该分段从未进入任何 revision，无法正常同步。

**复现**：

1. A、B 安装同一版本。
2. B 为同一个 thread 新增分段 rollout，包含 `LOCAL_ONLY`，不执行 checkpoint。
3. A 在主 rollout 中追加内容并发布新 revision。
4. B 接收后直接执行 Restore，再执行 Checkpoint。
5. Restore 成功，Checkpoint 报告未变化；绑定包含本地额外分段，但远端 revision 的快照不包含它。

**建议**：安装前及 adoption 时检查完整目标 artifact 集合。存在源快照之外且尚未采集的本地文件时，应拒绝安装并要求先保存本地进度。

### R05：恢复同步清单时将补零序号按八进制解析

**位置**：[internal/sync/pull.go:229–257](internal/sync/pull.go#L229)

**原因与影响**：`fmt.Sscan(match[1], &seq)` 使用带前导零的数字解析规则，并忽略解析错误。例如 `000000000010` 被解析为 8，而不是 10，导致恢复查询错误前驱并构造错误的认证附加数据，解密失败。残留 journal 会继续阻断后续恢复和同步。

**复现**：

1. 正常发布并接收前 9 个 manifest。
2. 第 10 个 manifest 接收时注入 `pull.after_verified=error`。
3. 清除故障注入，执行 Recover。
4. 返回 `recovery_needed`，无法完成该清单的恢复。

**建议**：使用显式十进制解析，例如 `strconv.ParseInt(value, 10, 64)`，并处理错误。补充序号 8、9、10 及更大序号的恢复用例。

### R06：数据库重建中断后不会继续回填历史

**位置**：[internal/store/db.go:40–59](internal/store/db.go#L40)

**原因与影响**：创建 schema 后，在回填历史前就写入当前 `user_version`。如果此时中断，下一次打开数据库会因版本匹配直接返回，残缺索引被当作正常视图。源数据仍保留，但历史查询缺失，依赖这些查询的恢复和同步功能受到影响。

**复现方式：模拟中断后的持久状态**：

1. 在临时设备中正常采集一个 revision，保留对象、revision 和 binding 文件。
2. 将索引替换为仅包含空 schema、但已设置当前 `user_version` 的数据库，模拟回填前崩溃。
3. 重新调用 `store.Open`。
4. 原始 revision 文件仍在，但查询得到 0 个 revision，回填没有重新执行。

**建议**：只有完整回填成功后才标记重建完成；失败或中断应保留可识别的未完成状态。同时传播回填错误，避免把失败回填标记为正常。

## P2：其他已复现问题

### R07：`sync` 吞掉采集错误并返回成功

**位置**：[cmd/session-memory/cmd_sessions.go:320–322](cmd/session-memory/cmd_sessions.go#L320)

**原因与影响**：仅在 `Checkpoint` 成功且有变化时更新计数，错误被直接忽略。用户可能误以为本地会话已成功同步。

**复现**：将已跟踪的临时会话文件设为不可读，执行 `sync --json`。命令退出码为 0，stderr 为空，也没有采集失败信息。

**建议**：保留按会话继续处理的能力，但汇总并输出失败信息；存在未完成采集时返回明确的非成功状态。

### R08：`verify --deep` 不校验 blob 内容

**位置**：[internal/streamcap/streamcap.go:270–275](internal/streamcap/streamcap.go#L270)

**原因与影响**：blob 分支只检查长度，随后直接继续，忽略 `deep` 和已记录的内容哈希。等长内容改写无法被深度校验发现。

**复现**：已记录内容为 `aaaa` 的 blob，将文件改为 `bbbb`，执行深度验证，异常列表仍为空。

**建议**：深度验证时按已保存的 segment 范围重新计算哈希，与 `RawHash` 比较，并保留长度检查。

### R09：加入包的非法 KDF 参数会触发 panic

**位置**：[internal/vault/vault.go:289–295](internal/vault/vault.go#L289)

**原因与影响**：外部 bundle 的 KDF 参数在调用 Argon2 前未校验。损坏或不可信的加入包会使导入命令崩溃，而不是返回可处理的格式错误。

**复现**：提供格式可解析、nonce 长度正确，但 `kdf.time=0` 的 bundle，调用 `OpenBundle`，触发 `argon2: number of rounds too small` panic。

**建议**：在执行 KDF 前验证轮数、线程数和内存参数，并设定明确的资源上限。非法参数直接返回错误。

### R10：更新项目映射会丢失自动跟踪设置

**位置**：[cmd/session-memory/cmd_restore.go:74–76](cmd/session-memory/cmd_restore.go#L74)

**原因与影响**：`project map` 使用新建的 `ProjectMap` 覆盖已有配置，没有保留 `Watch`。仅修改标签或路径也会使后续新会话不再自动加入同步。

**复现**：对已经 `project track` 的项目执行 `project map <ref> --path <原路径> --label renamed`，读取配置，`Watch` 从 `[codex]` 变为空。

**建议**：更新已有映射时先读取原配置，仅修改命令明确负责的字段，保留自动跟踪设置。

### R11：`project restore --json` 输出不是合法 JSON

**位置**：[cmd/session-memory/cmd_project.go:312–319](cmd/session-memory/cmd_project.go#L312)

**原因与影响**：命令在判断 JSON 模式前输出 `installed <session_id>`，污染 stdout，导致脚本无法将输出解析为单个 JSON 文档。

**复现**：在目标设备首次执行 `project restore <ref> --cwd <目录> --client-closed --allow-code-mismatch --json`。输出以 `installed …` 开头，后面才是 JSON；JSON 解析失败。

**建议**：JSON 模式只输出一个结构化文档。普通进度文本应受模式控制，或发送到 stderr。

## 验证结果与限制

现有验证均通过：

```bash
gofmt -l cmd internal
go vet ./...
git diff --check
go test ./...
go test -race ./...
go test -tags failpoint ./...
```

- 格式检查、静态检查及 diff 空白检查没有输出问题。
- 上述 11 个问题均通过临时 Go overlay 测试复现；用于保护预期行为的断言失败，说明现有测试尚未覆盖这些边界。
- R06 使用模拟崩溃后数据库状态的方式验证，未实际终止重建进程。
- 审查收尾发现归档相关文件出现并行改动，已保留，并补跑对应竞态测试通过：

```bash
go test -race ./cmd/session-memory ./internal/source/codex -run 'Test.*Assign' -count=1
```

- 全量测试结果对应执行时的工作区；收尾后的并行改动仅补测了对应归档用例。
- 未验证真实微云服务、Codex 桌面客户端或 WorkBuddy 图形界面行为。
- 本报告不是完整安全认证，也不表示未列出的路径不存在缺陷。

## 建议修复顺序

1. 优先处理 R01–R06，防止预览产生副作用、覆盖新增对话、漏采数据及恢复失败。
2. 修复 R07–R11，补齐失败可见性、深度校验、输入验证及 CLI 协议。
3. 将隔离复现用例转为正式回归测试，再重跑全量、竞态及 failpoint 测试。
