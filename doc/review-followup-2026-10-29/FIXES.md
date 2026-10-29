# F01–F03 修复与验证

本次处理复审发现的三个 P1 修复缺口，基于 `b219d94` 的工作区。保留原审查报告和 `results.json` 作为历史证据；本次结果另存 `fix-results.json`。

## F01：兼容旧 journal，并拒绝无法证明安全的回滚

`store.checkInstallUntouched` 合并 `Written` / `Planned` 的哈希，并覆盖 `Backed` / `Created` 的完整路径集合。旧 journal 没有 Planned 也必须检查：当前内容等于预期安装字节或备份时才能撤销；缺少预期哈希的现存文件必须等于备份，否则保留现场并返回 recovery_needed。WorkBuddy 继续通过自己的 RegistryUndo 校验文件与登记行。

正式回归：`internal/sync/legacy_recovery_test.go` 重新打开 Store，验证旧格式 create/append 中断后新增内容被保留，未改动的旧格式可以回滚；`internal/store/legacy_guard_test.go` 验证 Written 尚未落盘、备份未改/被改/被删、新文件缺少哈希等场景。

## F02：从完整已校验前缀生成游标

`jsonl.CursorBuilder` 是一个只保留头尾窗口的流式指纹构造器，生成的头尾哈希与原 Cursor 格式一致。`CaptureStream` 有新段时重新校验完整已消费前缀，同时把校验字节流送给构造器；旧段被改写也会拒绝。无新字节时复用旧游标，避免用后来打开的文件给未捕获字节重新背书。

成本：有变化的 plain 流增加一次完整已消费前缀读取；验证仍按流处理，不缓存整份历史。无新增字节的快速前缀检查保持原行为。FileID 是可选诊断信息，流式游标不填写；身份判断仍依赖既定的头尾内容校验。

正式回归：`internal/streamcap/regression_test.go` 的增量旧前缀改写拒绝；`internal/jsonl/cursor_builder_test.go` 在空、窗口边界、跨窗口与不同写入块大小下对照原文件 Cursor，并验证后来改写不能被已有指纹认证。既有首次采集改写、追加、半行、zstd、144 MiB 流式内存测试一并执行。

## F03：收到的远端历史也纳入持久副本修复

MultiMirror 的 Pull/List/Get 在普通文件进入共享缓存前，先原子保存已知账号的待补传状态。中途退出后，即使文件已在缓存、下轮不会再次下载，也会继续 CatchUp。账号暂停时保留该状态，恢复后补传；无新下载的空闲轮询不触发额外补传。无法持久记录修复工作时，停止相应接收，避免留下不受追踪的缓存文件。

对旧版可能错误记录的 level 状态，首次升级重新读取远端并集，再补齐本地缓存。并集读取完成才写 `<StateFile>.received-v1`；否则下一轮继续重新核对，期间本地失败写入产生的 behind 标志不能替代完整基线。复核后补充：每个账号完整读取过一次就记入 `<StateFile>.read`，永久不可达的账号不会让健康账号每轮被强制全量拉取，也不会每轮重新清除状态。密钥文件不进入普通下载/补传路径，删除意图保持独立。

正式回归：`internal/transport/receive_test.go` 覆盖单账号发布设备、双账号发布但备用额度耗尽、发布设备离线后接收设备补传、Get/List 接收、跨进程重启、跳过/恢复备用、状态目录不可写、空闲不增加远端调用，以及已有缓存升级和升级期间交错失败写入。

## 验证结果

| 检查 | 结果 |
| --- | --- |
| 默认全量测试 | 326 个测试/子测试通过，3 个 live 用例跳过 |
| `-race -tags dev,failpoint` 全量测试 | 437 个测试/子测试通过，3 个 live 用例跳过，无数据竞争报告 |
| 原复现脚本，race 模式连续 3 轮 | 42 个测试/子测试事件全部通过，退出码 0 |
| 本机、Linux amd64、Windows amd64 `go vet -tags dev,failpoint ./...` | 全部通过；交叉 vet 不代表在对应平台运行 |
| gofmt / git diff --check | 通过 |
| 延期问题的原审查脚本 | 9 个顶层断言仍失败，1 个 live 守卫跳过；无构建错误或数据竞争报告 |

两组全量测试的计数包含子测试，覆盖大量相同用例，不能相加当作独立功能数。每组覆盖 26 个有测试的包，另有 3 个包无测试；WorkBuddy、同步、Store 与 CLI 测试均通过。跳过的 live 用例为 `TestRegisterLive`、`TestLiveConvertToCodex`、`TestLiveWeiyunTwoMachines`。

## 验证命令

```bash
go test -count=1 ./...
go test -race -tags dev,failpoint -count=1 ./...
go vet -tags dev,failpoint ./...
python3 doc/review-followup-2026-10-29/run.py --race --count 3
```

原复现脚本已以 0 退出。完整计数、工作区源文件哈希与日志位置见 [fix-results.json](fix-results.json)；旧 `results.json` 中的失败计数属于修复前结果。测试使用临时目录、内存凭据与本地 HTTP 微云替身，没有访问用户的真实云端账号或改写日常原生会话。

其余延期的 9 项仍由 `doc/review-2026-10-29/run_audit.py` 单独追踪；本次没有将它们标记为已修复。
