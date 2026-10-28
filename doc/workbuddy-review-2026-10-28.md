# WorkBuddy 功能审查与验证

日期：2026-10-28。范围：当前工作区（含已有未提交改动），重点为 WorkBuddy 发现、采集、原生恢复、跨 Agent 转换、relay 和崩溃恢复。本轮保留业务实现与已有改动，只新增报告及隔离复现用例。

结论：现有测试全部通过，但新增边界断言稳定失败 4 项：2 个 P1、2 个 P2。不能据现有测试通过认定 WorkBuddy 的所有接续场景可用。

## 已确认的问题

### WB01 / P1：合法的元数据更新无法快进恢复

位置：`internal/source/workbuddy/writer.go:325–327`。

Writer 要求已存在的 blob 与来源逐字节相等，否则直接返回 `needs_action: WorkBuddy target has local or unmanaged content`。但读取侧明确支持重新采集变化的 `.meta.json` / tool-result blob。因此，即使目标没有任何本地修改，来源更新元数据后，合法的新版本也无法安装。

复现：两个临时 agent home、独立账号与 Store；A 采集主会话和 `{"value":"old"}` 元数据；B 首次恢复成功；A 仅将元数据改成 `{"value":"new"}` 并 checkpoint，复制加密对象与 revision 到 B；B 再次恢复失败。目标文件未被修改，绑定与旧版本均由正常流程建立。

用例：`TestReviewManagedMetaUpdate`。建议对可替换 blob 校验目标是否仍等于绑定基线，再允许备份及替换；不能简单移除比较而失去本地修改保护。

### WB02 / P1：子代理记录被当作主会话用户/助手轮次导入

位置：`internal/source/workbuddy/extract.go:96–117`；相关选项 `internal/convert/convert.go:60–61`。

Extract 遍历主流和所有 `subagents/*.jsonl`，分别求出各流的活动路径后直接 emit。子代理的活动消息同样带空 Branch，且转换器从未使用 `IncludeSubagents`。因此，即使显式设置 `IncludeSubagents=false`，子代理的任务提示和回答仍会变成目标主历史中的正常用户/助手轮次，没有来源标识。

复现：构造主对话与一个子代理对话；调用 Extract 后以默认排除子代理的配置转换，输出仍出现 `SUBAGENT_PRIVATE_TASK`。这里的标记是合成文本，不涉及真实用户内容。实际影响是跨 Agent 接续时混淆用户意图与子代理上下文。

用例：`TestReviewSubagentsExcludedFromConversion`。建议显式保留子代理归属，并在转换阶段按选项过滤、计入保真度报告；工具调用配对也应限制在同一流内。

### WB03 / P2：删除旁路文件未被报告为变化

位置：`internal/source/workbuddy/workbuddy.go:257–293`；下游 `internal/engine/capture.go:144–145`。

Capture 只遍历现存文件，Changed 仅来自各文件内容采集结果，没有比较新旧 artifact 集合。删除 `.meta.json`、子代理流或工具结果后，新 Cursor 的集合已缩小，但若其他文件没有变化，仍返回 Changed=false。Checkpoint 的无变化分支会保留旧快照，单独发生的文件删除不能形成新版本；深度验证却会报告旧绑定文件缺失。

用例：`TestReviewRemovedArtifactChange`。先采集含 4 个 artifact 的会话，删除元数据再采集，得到 3 个 artifact，但 Changed=false。建议比较集合差异并明确删除的同步语义，必要时同时调整 Writer 的删除计划与回滚处理。

### WB04 / P2：WAL 中已提交的软删除被漏读

位置：`internal/source/workbuddy/workbuddy.go:113–119`。

softDeleted 使用 `mode=ro&immutable=1` 读取活动数据库。隔离实验中，删除事务已提交但仍在 WAL、尚未 checkpoint 时，这种读取方式看不到最新 deleted_at，Discover 继续返回已经删除的会话。自动跟踪可能将该会话重新纳入同步。

用例：`TestReviewSoftDeleteInWAL`。启用 WAL、禁用自动 checkpoint；建立会话后 checkpoint；再提交 deleted_at 更新并保持数据库连接打开。普通查询确认已软删除 1 条，Discover 仍返回 1 条。

该实现与当前规格的 immutable 读取要求一致，但实验表明这项规格不适用于正在变化的数据库。建议同步修订规格，采用能看到已提交 WAL 的只读方案，并验证探测不会改写厂商数据。

## 通过的验证

| 验证 | 本轮结果 |
| --- | --- |
| `go test ./...` | 全部通过；CLI 包约 225 秒 |
| `go test -race ./internal/... ./cmd/session-memory` | 全部通过；CLI 包约 241 秒 |
| `go test -tags failpoint ./...` | 全部通过；CLI 包约 224 秒 |
| `go vet ./...` | 通过 |
| `git diff --check` | 通过 |
| WorkBuddy 单包重新执行 `-count=1 -v` | 24 个顶层用例通过；真实 Codex 转换测试因未设置 `SM_LIVE_OUT` 跳过 |
| WorkBuddy failpoint 单包重新执行 | 27 个顶层用例通过；含原生新建/快进共 28 个、转换写入 14 个、转换返回 13 个中断恢复场景 |
| relay 的 Agent 选择、运行检测、任务项目自动映射 | 4 项通过；映射测试中实际安装被前置条件拒绝，不能作为真实 relay 安装验收 |
| dev 沙箱工作流与 P0 包 | 定向测试通过；未完成完整 dev 全量，另一次完整 dev 运行被主动终止，不能计为通过 |

覆盖了正常采集、旁路文件、包装用户输入、分支历史、路径映射、登记 schema、账号隔离、opt-in、符号链接保护、原生往返、跨 Agent 转换和安装崩溃恢复。新增复现用例的 4 次失败是预期的缺陷证据，与已有测试通过并不矛盾。

## 本机 WorkBuddy 5.9.3 验证

- 从已安装应用的 Info.plist 读取版本为 5.9.3。
- 构建当前源码后运行 `agent probe workbuddy --json`：compatibility=ok；发现 93 个主流、14 个元数据文件、48 个子代理流、37 个工具结果文件；抽样 20 个会话做结构探测。未把真实会话正文或标题写入本报告。
- 对本机数据库作 immutable 只读 schema 检查：sessions 共 40 列，指纹为 `f8b325800c48b7c558caff1330e2c4a15aaa6419777a5fc691b8074250b3a680`，sessions 触发器数为 0，与当前白名单一致。此检查仅证明表结构兼容。
- `TestReviewLiveRuntimeGuard` 使用真实版本与进程检测，数据写入目标为临时目录：WorkBuddy 正在运行时，即使声明 ClientClosed=true，Apply 仍返回 client_running，且未创建目标 projects 文件。
- 本轮没有退出用户正在运行的 WorkBuddy，也没有向日常会话目录安装测试记录；没有重新执行 GUI 显示、真实模型续写、重开、真实双机/微云接续验收。仓库旧报告中的 GUI 验收是历史证据。

## 复现方法

源码保存在 [`workbuddy-review-2026-10-28/repro_test.go.txt`](workbuddy-review-2026-10-28/repro_test.go.txt)，使用 Go overlay 注入测试包，避免把预期失败的测试加入正常测试集。从仓库根目录执行：

```bash
python3 - <<'PY'
import json
from pathlib import Path
root = Path.cwd()
overlay = {'Replace': {
    str(root / 'internal/source/workbuddy/review_overlay_test.go'):
    str(root / 'doc/workbuddy-review-2026-10-28/repro_test.go.txt')
}}
Path('/tmp/sm-workbuddy-review-overlay.json').write_text(json.dumps(overlay))
PY
go test -overlay /tmp/sm-workbuddy-review-overlay.json \
  ./internal/source/workbuddy -run '^TestReview' -count=1 -v
```

预期：4 个边界用例失败；本机进程守卫用例默认跳过。若在安装了 5.x 的 macOS 上、WorkBuddy 正在运行，可在最后的 go test 命令前设置 `SM_REVIEW_LIVE=1`，额外验证真实运行守卫；其写入目标仍全部为测试临时目录。

建议先修 WB01 / WB02，再补齐删除与 WAL 场景，并把修复后的断言纳入正式回归测试。
