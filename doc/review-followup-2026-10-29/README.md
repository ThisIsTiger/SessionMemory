# 修复后复审（2026-10-29）

> 后续修复：本报告记录的 F01–F03 已在工作区修复并补充正式回归测试；实现与最新验证见 [FIXES.md](FIXES.md)。下文保留复审时的历史结论与反例。

## 结论

本轮重点审查 A01–A04、A11、A12、A14 的修复，以及仓库地址/许可说明的调整。**确认 3 个 P1 修复缺口：A01 的旧 journal 兼容路径、A02 的增量旧段校验、A04 的接收历史备份。** 三个缺口均在 `-race -tags dev,failpoint -count=3` 下稳定复现。

原 16 项问题的状态不能直接更新成“修复了 7 项、剩余 9 项”：本轮确认 A03、A11、A12、A14 的原问题已修；A01、A02、A04 的原反例通过，但更完整的边界仍失败；其余 9 项提交中明确未处理，复测仍失败。按原问题归类，目前为 **4 项已修、3 项部分修复、9 项未处理**，没有把残余场景额外叠加到原问题总数里。

审查开始时 HEAD 为 `a692ed6`，修复提交为 `8874909`。期间 Git 历史被重写，修复提交对应 `7b00e0d`；后续还加入了 MIT 许可证和 CI 调整。已按 Git blob ID 核对本轮 7 个业务修复文件，与开测时完全一致；这些元数据/CI 提交未改变受测业务代码。最后核对的 HEAD、文件哈希与测试结果见 `results.json`。

本轮没有修改业务代码，只新增本目录的复现用例、脚本和记录。

## F01 / P1：旧版 journal 跳过保护，升级恢复仍可抹掉新增对话

**位置：** `internal/store/tx.go:253–255`。

`checkInstallUntouched` 只遍历新字段 `Planned`。修复前产生的 journal 没有该字段，函数直接返回 nil，之后依然进入原来的无条件删除/备份覆盖逻辑。旧 `written` 状态 journal 已有 `Written` 的目标哈希，代码没有使用它做兼容校验。

**触发流程：**

1. 旧版本安装中断，留下 `written` 状态 journal。
2. 用户继续该会话，新增 `LEGACY_AFTER_CRASH`，随后退出客户端。
3. 安装新版 SessionMemory，启动恢复。

**实际结果：** Recover 返回成功。首次安装分支删除整个会话文件，快进分支以旧备份覆盖文件，新增记录丢失。新建的 `Planned` 保护不适用于这个正常升级路径。

**复现方式与限制：** 用当前 Writer 生成 `written` 状态，再关闭 Store，仅移除 JSON 中的新增 `planned` 字段，保留旧格式的其他字段、`Written`、staging 和备份，构造与旧版本相同的相关持久状态；不是实际运行旧二进制。重新打开 Store 后使用 Codex.GuardRecovery，分别验证 create/append。未改动的旧格式 journal 也有通过的正向回滚对照，排除了“旧格式本身无效”的解释。

**建议：** 为旧 journal 使用 `Written`/可验证 staging 推导预期字节；信息不足时保留文件、备份和 journal，返回 recovery_needed。不能用“缺少新字段”作为允许破坏性回滚的条件。

**用例：** `TestFollowupLegacyRecoveryPreservesPostCrashProgress/{create,append}`；对照 `TestFollowupLegacyUntouchedJournalStillRollsBack`。

## F02 / P1：只校验新段，增量采集仍会认证被改写的旧前缀

**位置：** `internal/streamcap/streamcap.go:141–145`。

当前修复只对 `segs[nOld:]` 重算哈希。已有 segments 的旧前缀只在本次采集开始时检查一次；随后 NewCursor 会重新打开文件，计算并保存“现在”的头尾哈希。因此旧前缀在采集期间改变、但新追加段没改变时，会把错误组合认证为正常快照。

**复现：**

1. 首次采集 `{"x":1}`，保存 segment 和游标。
2. 向源文件追加 `{"tail":1}`。
3. 在 sink 保存追加段时，把已采集的旧前缀等长改成 `{"x":2}`，追加段不变。
4. 使用返回游标再次采集。

**实际结果：** 第二次采集返回 CaptureOK；对该结果做完整 segment 校验为 false；第三次采集 Changed=false。历史仍保存旧的 `x=1`，游标却已经认证了 `x=2`，改写内容被永久漏采。

首次完整采集的原反例现在确实会拒绝，新增的现有测试只覆盖这个场景。增量测试需要同时覆盖“新段变化”和“旧段变化”，不能用新段哈希相等推断整个已消费前缀未变。

**建议：** 将游标和全部已消费字节绑定到一致的读取视图；至少不要在旧前缀未经重新确认时用新文件的哈希替换既有认证信息。根据性能约束设计旧游标验证/一致性检查，而不是仅验证 fresh 段。

**用例：** `TestFollowupCaptureRejectsRewriteInCommittedPrefix`。

## F03 / P1：备用账号登记完成后，不再补齐从其他设备接收的新历史

**位置：** `internal/transport/multi.go:133–135`；相关 `Pull` 在 `237–244`。

新账号首次 CatchUp 能补齐历史，已通过复测。但两账号一旦记为 `false`（已追平），后续 Pull/Get 收到其他设备只发布到其中一个账号的内容，不会把缺少副本的账号重新标记为 behind。CatchUp 每轮都跳过它们。

**两种稳定复现的场景：**

- 发布设备只配置主账号，接收设备配置双账号。
- 两设备都配置双账号，发布时备用账号暂时额度耗尽，发布通过主账号成功；随后发布设备离线，备用账号恢复，接收设备现在可访问两个账号。

接收设备已在此前完成两账号登记。它成功收到新历史，再执行三轮 CatchUp/Pull，备用账号仍为空，状态依旧是 `backup:false primary:false`。

**影响：** 新增账号的一次性补齐不能保证持续互备。正常交接后，如果原发布设备不再上线补传，备用账号可能长期缺少已接收的历史；主账号失效时，备用副本不完整。

**建议：** 把“从远端收到本地原来没有的文件”也纳入副本修复，记录缺失副本或安排后续补传；不能把上次登记的 level 状态永久视为完整。保留密钥文件独立发布/删除的约束，不要用普通对象同步恢复用户已删除的密钥。

**用例：** `TestFollowupKnownBackupCopiesHistoryReceivedFromOtherDevice/{single_account_publisher,dual_account_publisher_backup_unavailable}`。

## 修复复核表

| 原编号 | 本轮结果 | 证据 |
| --- | --- | --- |
| A01 | 部分修复 | 新格式中断后追加、未追加的现有测试通过；旧格式追加仍被 F01 覆盖/删除 |
| A02 | 部分修复 | 首次采集改写被拒绝；增量旧段改写仍触发 F02 |
| A03 | 原问题已修 | 原额外 rollout 拒绝测试通过；额外文件在 Plan 后、Apply staging 时出现，也正确返回 baseline_changed，主文件与额外文件均保留 |
| A04 | 部分修复 | 新账号与空缓存的初始补齐测试通过；后续接收历史仍触发 F03 |
| A11 | 原问题已修 | Folder 缺云端密钥时不创建无关身份的回归通过 |
| A12 | 原问题已修 | 任务项目预览不创建目录的回归通过；该夹具的安装仍因缺少厂商 registry 而拒绝，不代表 GUI/真实安装验收 |
| A14 | 原问题已修 | 等长 blob 改写被深度校验发现；额外使用真实 Store sink 对空、1 字节、单分块上限、跨分块文件验证，未改写均通过，改写后均拒绝 |

其余 **A05、A06、A07、A08、A09、A10、A13、A15、A16** 用原 `run_audit.py --race` 复测，9 个顶层断言继续失败，未出现构建错误或 DATA RACE。这些是提交中明确保留的问题，本轮没有冒充新发现重新计数。

## 测试与限制

| 检查 | 结果 |
| --- | --- |
| `go test -count=1 -timeout=12m -json ./...` | 297 个测试/子测试通过，3 个 live 用例跳过 |
| `go test -race -tags dev,failpoint -count=1 -timeout=12m -json ./...` | 403 个测试/子测试通过，3 个 live 用例跳过，无竞态报告 |
| 本目录 overlay，race 模式连续 3 轮 | 3 个不同顶层缺陷稳定复现；包含崩溃与发布模式子例，共 21 个失败事件；21 个正向/对照测试事件通过 |
| 原剩余问题脚本 `run_audit.py --race` | 9 项按预期失败，1 个 live 守卫跳过；没有构建错误或竞态报告 |
| native / Linux amd64 / Windows amd64 `go vet -tags dev,failpoint ./...` | 通过；交叉 vet 不代表在非本机平台运行 |
| gofmt / git diff --check | 通过 |

默认与 race 矩阵中的计数包含子测试，且两个矩阵覆盖大量相同用例，不能相加当作独立功能数。本轮没有复跑无关的长时间 JSONL/加密 fuzz，也没有复用上轮的“360 万次”作为本轮成绩。

所有新增反例使用临时 Store、合成对话和本地 HTTP 微云替身；未访问真实云端账号，未写入日常客户端会话目录，未控制 launchd。真实双机、WorkBuddy GUI/模型续写与物理断电仍不在本轮验证范围。panic 后关闭再打开 Store 是持久状态恢复测试，不等同于 OS 断电。

## 复现

```bash
# 正常矩阵，应通过
go test -count=1 ./...
go test -race -tags dev,failpoint -count=1 ./...

# 本次发现的三个修复缺口，当前应非零退出
python3 doc/review-followup-2026-10-29/run.py --race --count 3

# 上轮明确留待后续处理的九项，当前仍应非零退出
python3 doc/review-2026-10-29/run_audit.py --race
```

脚本自动生成 overlay、原始 JSONL 日志和 summary.json，不向正常测试包加入预期失败测试。请先检查 summary.build_errors 为空；构建失败不能算作复现。修复后应让相应断言通过，再将其纳入正式回归测试。
