# SessionMemory 开发文档

> 对应设计：[unified-agent-session-sync-design.md](unified-agent-session-sync-design.md)（v0.4）  
> 状态：Draft / 待实施  
> 日期：2026-10-26

## 0. 文档定位

- 设计文档回答“做什么、为什么”，本文回答“怎么做、按什么顺序做、怎么验证”。两者冲突时以设计文档为准；P0 实验结论会同时回写两份文档。
- 本文中的 Go 接口、磁盘布局、字节格式都是**草案**。其中依赖 Codex / WorkBuddy 实际行为的部分（Writer、转换规则、客户端运行检测）在 P0 结束前不冻结，文中以“待 E#”标注对应的实验。
- 本文只写工程决策，不重复设计文档里的产品规则。

## 1. 开发原则

1. **P0 先行。** 协议里最不确定的是“能否写进客户端、客户端是否认”。与此无关的底座（JSONL 读取、Canonical 类型、journal、对象存储、加密）可以并行开发；Writer 和转换规则必须等 P0 结论。
2. **永远不在日常使用的客户端数据上做实验。** 所有写入类实验使用隔离环境（见 §9.1），备份先于任何写入。
3. **默认只读，写入必须经过 plan → 备份 → journal → 验证 → 可回滚。** 任何向厂商目录写入的代码路径都必须有对应的崩溃恢复测试。
4. **日志和测试数据不含对话正文。** 日志只记 ID、计数、偏移、哈希；fixture 必须脱敏。
5. **宁可拒绝，不要猜。** 未知格式、未知版本、无法确认的边界一律拒绝并给出原因，不静默降级。
6. **一个提交只做一件事**，每个里程碑有可运行、可测试的产出。

## 2. 技术选型

| 领域 | 选择 | 说明 |
|---|---|---|
| 语言 | Go（与 `go.mod` 一致，目前为 1.25；CI 改为 `go-version-file: go.mod`） | 单二进制，跨平台 |
| CLI | `github.com/spf13/cobra` | 命令数量多，需子命令与补全；若想零依赖可换标准库 `flag`，不影响其他模块 |
| 本地索引 | `modernc.org/sqlite`（纯 Go） | 避免 Windows 上的 CGO；只存**可重建**的视图（见 §5） |
| 对象加密 | 标准库 `crypto/aes` + `crypto/cipher`（AES-256-GCM）、`crypto/rand` | 每对象随机 data key，由 vault key 包装 |
| 设备签名 | 标准库 `crypto/ed25519` | 若设计文档“待决策”中的简化方案被采纳，则此项可去掉 |
| 口令派生 | `golang.org/x/crypto/argon2`（argon2id） | 保护设备加入包 |
| 凭据存储 | `github.com/zalando/go-keyring` | macOS Keychain / Windows 凭据管理器 / Linux Secret Service；不可用时拒绝落盘私钥，不回退到明文文件 |
| 文件监听 | `github.com/fsnotify/fsnotify` | 只用于降低延迟；正确性靠定期 reconcile |
| 日志 | 标准库 `log/slog` | 统一经 `internal/logx` 做脱敏封装 |
| 测试 | 标准库 `testing`，不引入断言库 | 保持依赖少 |

依赖原则：新增第三方依赖需在 PR 描述里说明理由；加密、签名一律用标准库或 `x/crypto`，不自己实现算法。

## 3. 目录结构与依赖方向

```text
cmd/session-memory/        # CLI 入口，只做参数解析与输出，不含业务逻辑
internal/
  jsonl/                   # 追加式 JSONL 增量读取、前缀校验、文件身份
  canonical/               # Canonical Transcript 类型与校验（配对、保序）
  agent/                   # Adapter / Writer / RuntimeDetector 接口与公共类型
  source/codex/            # Codex 适配器：发现、采集、Writer
  source/workbuddy/        # WorkBuddy 适配器：发现、采集、Writer
  source/claude/           # 保留现有代码，P3 前不扩展
  convert/                 # Canonical → 目标原生条目的规则、保真度报告
  session/                 # SyncSession / Revision / Binding / 分歧检测（纯逻辑，无 IO）
  store/                   # 本地对象库、journal、sessions.db、绑定文件
  vault/                   # 密封（Seal/Open）、设备身份、加入包
  transport/               # Transport 接口与 FolderTransport
  sync/                    # manifest、receipt、reconcile、push/pull
  bridge/                  # Resume Pack（兜底）、注入边界
  mcp/                     # 兜底路径的本地读取接口（可选）
  project/                 # 目录映射、Git 状态
  platform/                # 原子写、锁、进程检测等跨平台原语（保留现有）
  config/                  # 配置加载与校验（重写，见 §7）
  failpoint/               # 测试用崩溃注入
  logx/                    # 脱敏日志封装
```

依赖方向（只允许向下）：

```text
cmd → sync, bridge, mcp, convert, project
sync → store, transport, vault, session
convert, bridge → canonical, session
source/* → agent, canonical, jsonl, platform
store → session, vault(仅 Sealer 接口), platform
session, canonical, jsonl → （无内部依赖）
```

- `session` 和 `canonical` 是纯数据与纯函数，不做 IO，便于大量单元测试。
- `source/*` 之间互不依赖；跨 Agent 的转换只通过 `canonical` 与 `convert`。
- `cmd` 之外不得 `os.Exit`，不得直接读环境变量（统一经 `config`）。

## 4. 核心接口草案

以下仅表达职责边界，具体签名以 M2 评审为准。

### 4.1 Agent 适配器

```go
package agent

type Profile struct {
    Agent, Surface, Version, Format string // 如 codex / desktop / 0.x / jsonl-v?
    AdapterVersion                  string
    OS                              string
}

// NativeRef 是设备本地数据，路径永远不同步。
type NativeRef struct {
    NativeID string
    Path     string
}

type Adapter interface {
    Name() string
    Probe(ctx context.Context) (ProbeResult, error)          // 只读；输出安全，不含标题/正文/cwd
    Discover(ctx context.Context, o DiscoverOptions) ([]NativeSession, error)
    Capture(ctx context.Context, ref NativeRef, cur Cursor) (CaptureResult, error)
    Runtime() RuntimeDetector
    Writer() Writer
}

type CaptureResult struct {
    Segments  []Segment         // 原始字节段 (generation, start, end, raw_hash)
    Canonical []canonical.Item  // 本次增量
    Cursor    Cursor            // 仅在 State==CaptureOK 时可提交
    State     CaptureState      // OK | Unsafe | Replaced | Truncated
    Diags     []Diag            // 类型、偏移、计数，无正文
}
```

### 4.2 Writer（安装与转换写入）

```go
type Mode int // ModeNative | ModeConvert

type WriteInput struct {
    SessionID  string
    Revision   string
    Mode       Mode
    Source     ObjectReader     // 读取 native snapshot / canonical
    PathMap    PathMapper       // 结构化路径变换
    Target     Profile
}

type Writer interface {
    // Plan 无副作用：校验兼容矩阵、基线、依赖，产出可展示的计划与保真度报告。
    Plan(ctx context.Context, in WriteInput) (Plan, error)
    // Apply 在 journal 事务中执行：staging → 备份 → 写入。
    Apply(ctx context.Context, p Plan, tx *store.Tx) (Receipt, error)
    Verify(ctx context.Context, r Receipt) error
    Rollback(ctx context.Context, tx *store.Tx) error
}

type RuntimeDetector interface {
    Running(ctx context.Context) (RunState, error) // Running | NotRunning | Unknown
}
```

- `Unknown` 一律按“可能在运行”处理，`Apply` 不执行（设计 §9.3）。
- `Plan` 必须是纯函数：同样的输入产出同样的计划，便于测试和 `--plan` 展示。
- `Apply` 之前由调用方重新校验基线（`baseline_changed` 错误）。

### 4.3 Canonical Item

```go
package canonical

type Kind string // message | reasoning | tool_call | tool_result | compaction |
                 // attachment_ref | session_meta | unknown

type Item struct {
    ID       string            // 稳定记录键，来自原生记录 ID 或 (generation, offset, hash)
    Kind     Kind
    Role     string            // message 专用：user | assistant | system | developer
    ParentID string
    CallID   string            // tool_call 与 tool_result 配对
    Time     time.Time
    Norm     map[string]any    // 归一化字段，不含 Agent 私有结构
    Raw      RawRef            // 指回原始条目（native snapshot 中的位置与哈希）
}

// Validate 检查：保序、CallID 配对、必填字段；不通过的条目标记而不是丢弃。
func Validate(items []Item) []Issue
```

### 4.4 Store、Transport、Sealer

```go
package store
type Store interface {
    PutObject(ctx context.Context, ref ObjectRef, r io.Reader) error  // 先 staging 再校验再发布
    GetObject(ctx context.Context, ref ObjectRef) (io.ReadCloser, error)
    CommitRevision(tx *Tx, rev session.Revision) error                // 对象齐全才提交
    Begin(ctx context.Context, kind string) (*Tx, error)              // 写 journal
}

package transport
type Transport interface {
    Put(ctx context.Context, path string, r io.Reader) error // 写 .partial-<uuid>，fsync，最终发布；写一次不可改
    List(ctx context.Context, prefix string) ([]Entry, error)
    Get(ctx context.Context, path string) (io.ReadCloser, error)
    Stable(ctx context.Context, e Entry) (bool, error)       // 文件是否可安全读取（占用/按需下载）
}

package vault
type Sealer interface {
    Seal(t ObjectType, aad AAD, plain []byte) ([]byte, error)
    Open(t ObjectType, aad AAD, sealed []byte) ([]byte, error)
}
```

`Sealer` 是接口，使 M1–M3 可以在没有 vault 的情况下开发和测试（见 §8 M3）。测试专用的透传实现用 build tag 隔离，**不得进入发布构建**。

## 5. 本地数据布局

状态目录默认 `~/.session-memory`，可由 `--home` 或 `SM_HOME` 覆盖（测试和双设备模拟必须使用）。全局配置沿用 `~/.config/session-memory/config.json`。

```text
$SM_HOME/
├── device/                  # device_id、公钥；私钥在系统凭据存储，不落盘
├── bindings/<binding_id>.json   # 绑定、cursor、receipt：设备状态的事实源
├── journal/<tx_id>.json     # 事务意图记录（见 §6.2），启动时扫描恢复
├── store/
│   ├── objects/ab/<sha256>.obj  # 已验证的密文对象
│   ├── staging/                 # 0700，唯一允许出现明文的临时区
│   └── sessions.db              # SQLite 视图：可由 objects + bindings 重建
├── backups/<tx_id>/         # 写入厂商目录前的备份，0700，有保留期限
├── quarantine/              # 校验失败的对象与 manifest
├── locks/                   # 每设备 sequence、每 session 采集的互斥锁
└── logs/
```

- **事实源**：`bindings/`、`journal/`、`objects/`。`sessions.db` 丢了可以重建，不能当事实源。
- 所有 JSON 文件带 `schema_version`；读到更高版本拒绝并提示升级，读到更低版本按迁移函数升级。
- 所有写入使用 `platform.AtomicWrite`（temp → fsync → rename → fsync 父目录），目录权限 0700、文件权限 0600。
- 同步目录布局沿用设计 §11.1，由 `transport` 与 `sync` 负责，本地不直接拼路径。

### 5.1 ID 约定

| 类型 | 形式 | 说明 |
|---|---|---|
| session | `ss_<128-bit 随机，base32>` | 首次纳管时生成 |
| revision | `rev_<payload-sha256>` | 按规范化 payload 计算 |
| binding | `bind_<随机>` | 设备本地 |
| device | `dev_<随机>` | 每个活跃设备唯一；备份恢复后必须重新生成 |
| object | `obj_<密文 sha256>` | 密文内容寻址；同内容重新加密会得到不同 ID，不期望跨设备去重 |
| tx | `tx_<随机>` | journal 事务 |

### 5.2 加密对象信封（草案，M4 冻结）

```text
magic "SMOB" | version u8 | type u8 | wrapped_key_len u16 | wrapped_key | nonce[12] | ciphertext+tag
```

- AAD = vault_id ‖ schema_version ‖ object_type；被篡改或换类型时 `Open` 失败。
- data key 随机生成，用 vault key 以 AEAD 包装；vault key 与设备私钥只在内存和系统凭据存储中。

## 6. 关键机制实现要点

### 6.1 JSONL 增量读取与前缀校验（`internal/jsonl`）

沿用现有“只消费以 `\n` 结尾的完整行”的做法，并补足设计 §8.2 要求的校验：

- **文件身份**：Unix 用 `(dev, inode)`，Windows 用文件 ID；与大小、mtime 一起记录在 cursor 中，仅作为快速判定。
- **前缀校验**：cursor 保存已采集区间末尾的 `(offset, hash_of_last_4KiB, hash_of_first_4KiB)`；每次采集先校验这两段哈希，不符则 `Replaced`（同长度重写也能发现）。
- **generation**：检测到替换/截断时 `generation++`，保留旧 revision，新 segment 从 0 开始，并记录“替代关系”。
- **损坏行**：完整但无法解析的行不静默跳过，也不让整个会话卡死——返回 `Unsafe` 与偏移，cursor 停在其前，会话标记 `unsafe`，由适配器兼容矩阵决定是否继续后面的记录（设计 §8.2）。
- **读取上限**：沿用 `MaxRecordBytes`（默认 16 MiB），超限按 `Unsafe` 处理并报告偏移。
- segment 按安全边界（完整记录、且由适配器判定为稳定边界）切分，不按日期切。

### 6.2 事务与 journal（`internal/store`）

跨“本地对象、revision、cursor、SQLite、厂商目录”的操作都走同一套事务：

```text
采集：  planned → objects_staged → objects_published → revision_committed → cursor_advanced → done
发布：  planned → objects_uploaded → manifest_published → marked_published → done
安装：  planned → staged → backed_up → written → verified → binding_committed → done
                                  └─ 任一步失败 → rolling_back → rolled_back
```

- 每次状态迁移先写 journal（原子写）再执行动作；每个动作必须**幂等**。
- 进程启动时扫描 `journal/`：处于中间状态的事务按状态继续或回滚，不重复产生对象或 revision。
- **cursor 只在 revision 提交之后推进**；崩溃后重新采集同一区间，靠 `(generation, start, end, raw_hash)` 去重。
- 安装事务写厂商目录之前必须已有可验证的备份；`verified` 失败自动回滚，并留下诊断（无正文）。

崩溃点测试矩阵（M3 建立，之后每个会写磁盘的功能都要加入）：

| 事务 | 必测崩溃点 |
|---|---|
| 采集 | staged 后、published 后、revision 提交后、cursor 推进前 |
| 发布 | 对象上传一半、对象齐全但 manifest 未发布、manifest 发布后未标记 |
| 安装 | 备份后、写入一半、写完未验证、验证后未提交 binding |

用 `internal/failpoint` 注入：`SM_FAILPOINTS=store.after_stage=crash`（仅测试构建可用）。

### 6.3 Canonical 采集与转换（`canonical`、`convert`）

- 采集时同时产出 Canonical 增量；原始条目留在 native snapshot，`Raw` 指回其位置和哈希。
- `Validate` 在采集和转换前后各跑一次：保序、`tool_call`/`tool_result` 按 `CallID` 配对、必填字段齐全。
- 转换规则表（设计 §7.2）以**数据+代码**形式放在 `convert/<目标 Agent>/`，每条规则带单元测试和 golden 样例；规则随 Writer 版本发布。字段映射细节**待 E3/E4**。
- 必须落实的硬规则（不依赖实验结论）：
  1. `system`/`developer` 指令类消息不写入目标；
  2. 孤立的 `tool_call`/`tool_result` 不写成原生条目，渲染为文本；
  3. 文本渲染的工具输出有大小上限，超出截断并标注来源；
  4. `reasoning` 默认不写入；
  5. 转换结果写入前后各做一次自检：目标格式校验 + 条目数与计划一致。
- 保真度报告是结构化 JSON（`schema_version`、各类条目的 kept/rendered/dropped 计数、上下文容量估算、环境缺口、写入路线），再由 CLI 渲染成文本；报告不含正文。

### 6.4 导入边界（设计 §8.3）

Writer 在 `Apply` 时产出 `import_receipt`：

```json
{
  "writer_version": "…",
  "source_revision": "rev_…",
  "written_count": 1234,
  "last_native_marker": "…",
  "boundary_hash": "sha256 of written prefix"
}
```

采集端据此：校验边界之前内容的哈希；只把边界之后的记录当新记录；边界被改写或无法确认时标记异常，不凭文本相似去重。能写入来源字段的目标（如 WorkBuddy meta 中的来源类字段，待 E3/E8 确认）作为辅助证据，不是唯一依据。

### 6.5 客户端运行检测（待 E2）

`RuntimeDetector` 的候选信号，P0 逐个验证后写入各适配器的支持矩阵：

| 信号 | 说明 |
|---|---|
| 进程存在 | 按平台枚举进程名/路径；注意多个客户端形态（Desktop/CLI） |
| 打开的文件句柄 | 目标会话文件或客户端数据库是否被持有（`lsof` / Windows Handle API）；跨平台成本高 |
| 官方接口 | 如 Codex App Server 的线程状态；若存在优先使用 |
| 客户端自身的锁文件 | 本机 `~/.codex` 下有 `*-locks` 类目录，用途待确认 |

任何信号不确定或互相矛盾，返回 `Unknown`。

### 6.6 分歧检测（`internal/session`，纯逻辑）

- 输入：本地已验证的所有 revision（含 parent）与用户的 choice 记录。
- 计算 head 集合（没有后代的 revision）。head 数 > 1 且没有一条 choice 覆盖**全部当前 head** → `conflict`。
- choice 记录引用选择时看到的 head 集合；之后出现该集合之外的新 head，重新进入 `conflict`（设计 §9.2 第 5 条）。
- 分歧下禁止：自动安装、自动转换、自动选择“最新”。
- 这部分是纯函数，用表驱动测试覆盖设计 A5、A6，包括跨 Agent 的分歧。

### 6.7 Sync 引擎

- 每设备 sequence 由本机锁保护，分配后先写 journal 再发布；manifest 外层字段与设计 §11.1 一致。
- Pull：扫描 → 验证成员与签名/哈希 → 依赖未齐则 `pending-objects` → 解密校验 → 原子提交 → 分歧检测。
- watcher 只触发一次提前 reconcile；定时完整 reconcile（默认 60 秒）是正确性的来源。
- 乱序、重复、冲突副本按内容校验去重；校验失败进入 `quarantine/`，不删除。
- `Transport.Stable` 判定“文件仍被占用/尚在下载”，对失败做指数退避重试。

### 6.8 项目与代码状态（`internal/project`）

- 首次选目录生成 `project_ref`；其他设备显式映射（设计 §10）。
- 采集时记录 branch、HEAD、dirty/untracked 摘要（只摘要，不含内容）。
- 转换到按目录组织存储的 Agent（如 WorkBuddy）之前**必须已有本地目录绑定**，否则只能出计划。

## 7. 现有代码处置

对照 README 中已复制的代码，逐文件处置。“保留”指可直接作为基础，“改”指保留思路但需修改，“删”指新协议里不再需要。

| 路径 | 处置 | 说明 |
|---|---|---|
| `internal/platform/platform.go` | 改 | `AtomicWrite` 补 fsync 父目录（Unix）；目录创建改用 0700，并要求调用方先 `EnsurePrivateDir` |
| `internal/platform/lock_unix.go`、`lock_windows.go` | 改 | Windows 非阻塞锁只把“已被占用”映射成 `ErrLockHeld`，其他错误原样返回 |
| `internal/checkpoint/` | 删（M3 后） | 新的 cursor/receipt 放在 `bindings/` + journal；迁移完成后移除，避免两套状态 |
| `internal/conversation/` | 删（M2 后） | `Batch`、`OperationID`、`ConversationText` 是日报协议；`CleanText` 如仍需要，迁入 `canonical` 工具函数 |
| `internal/config/` | 重写（M0） | 去掉 Summary/Report/Blog/Schedule/Provider（含 `api_key`）等字段与校验；保留路径展开、全局+项目配置合并的思路；新增 agents、transport、auto_install 等 |
| `internal/source/codex/parser.go` | 拆 | 完整行读取与偏移推进抽入 `internal/jsonl`；按日期生成 Batch 和丢弃工具调用的逻辑不保留，由新的 Canonical 采集取代 |
| `internal/source/codex/discover.go` | 改 | 去掉“最近 N 小时”窗口；以用户选中的会话为准；按原生会话 ID 定位而非路径，处理 `archived_sessions/` 迁移；修正 UTC 截断日期的问题 |
| `internal/source/codex/classify.go` | 改 | 只记录 source/originator 信息，不再作为产品边界；默认不排除 `cli`/`exec`，改为用户选择与支持矩阵决定；子代理（`<object>` 来源）是否需要作为依赖待 E1 |
| `internal/source/codex/probe.go` | 改 | 扩展为 `agent probe codex`：版本、存储形式、登记/投影文件存在性、兼容性结论；保持“输出不含标题/正文/cwd” |
| `internal/source/claude/*` | 保留不动 | P3 前不扩展；其中 `install.go` 的手写字符串查找可改用 `strings`，Hook 路径应尊重 `CLAUDE_CONFIG_DIR` |
| `testdata/` | 保留 | 继续作为 fixture；新增脱敏的真实版本样本，并加“必须原样保留”的反向断言 |
| `.github/workflows/test.yml` | 改 | `go-version-file: go.mod`；保留 3 平台矩阵和 `-race` |

## 8. 里程碑与任务

每个里程碑都要满足通用完成标准（§8.1）。里程碑之间的依赖：

```text
M0 ──► M1 (P0 实验) ──────────────┐
  └──► M2 ──► M3 ──► M4 ──► M5 ◄──┘ (Writer 依赖 M1 结论)
                          └──► M6 ──► M7
```

M2–M4 中与 P0 结论无关的部分可以在 M1 进行期间并行；所有 Writer 实现等 M1 的决策点之后。

### 8.1 通用完成标准

- `gofmt`、`go vet ./...`、`go test ./...`、`go test -race ./internal/...` 在三个平台通过；
- 新增的写磁盘逻辑有崩溃恢复测试（§6.2 矩阵）；
- 日志和测试输出不含对话正文；
- 相关设计/开发文档同步更新。

### M0：仓库整理与骨架

- 重写 `internal/config`，清除日报遗留字段；
- 建立 `internal/jsonl`，从现有 parser 抽出行读取并补前缀校验；
- 修正 `platform` 的原子写与锁问题；
- CI 改为 `go-version-file`；
- `cmd/session-memory`：`version`、`agent probe codex`；
- 搭建 `failpoint` 与 `logx`。

**Done**：代码里无 Summary/Blog/Provider 引用；`agent probe codex` 在本机输出安全的探测结果；`jsonl` 有同长度重写、截断、半行、损坏行的测试。

### M1：P0 实验（设计 §15 的 E1–E9，外加本文补充的隔离环境 E0）

- 实验工具放在 `internal/p0/`，通过隐藏子命令 `session-memory dev …` 调用，**不进入正式命令集**；
- 工具：只读结构清单、隔离沙箱准备、最小 Codex 会话写入、朴素的 Codex↔WorkBuddy 转换、结果差异比对；
- 每个实验按 §9 的模板产出报告 `doc/p0/E#.md`，汇总为 `doc/p0-report.md`；
- 结束时召开决策点：确定写入路线、第二优先需求的交付等级（设计 §15）。

**Done**：E0–E9 均有结论（通过/失败/部分通过）与证据；设计文档 §3.3、§6.2、§7.2 按结论回写；脱敏 fixture 入库。

### M2：Canonical 与 Codex 采集

- `canonical` 类型与 `Validate`；
- Codex 适配器 `Capture`：产出 segments 与 canonical，覆盖 `response_item`、`event_msg`、`compacted` 等记录，未知记录保留为 `unknown`；
- 文件替换、截断、半行、损坏行、归档迁移的处理；
- 测试：golden fixture，原始字节段拼接后与源文件逐字节一致；Canonical 保序、配对。

**Done**：对 fixture 采集后，segments 重新拼接等于原文件；`unknown` 不丢；A7 场景通过。

### M3：本地 Store、journal 与回环恢复

- `store`：对象库、journal、绑定文件、`sessions.db` 重建；
- 两个 `SM_HOME` + 一个普通目录作为传输（`Sealer` 测试实现，build tag 隔离）模拟两台电脑；
- Codex Writer（按 M1 选定的路线）：`Plan/Apply/Verify/Rollback`，在**隔离的 Codex 环境**上跑通；
- 崩溃矩阵测试。

**Done**：A9 通过；在隔离环境中，一个会话从设备 A 采集、经目录传到设备 B 并安装，Codex 能显示并继续；回滚后与安装前逐字节一致。

### M4：Vault、传输与同步

- `vault`：密封/打开、设备身份、加入包、系统凭据存储；
- `transport.FolderTransport`、`sync`：manifest、receipt、reconcile、quarantine、重试；
- 去掉测试 `Sealer`，所有跨设备对象加密；
- 在真实微云同步目录上做手动验证（E7 的延续）。

**Done**：A8、C1、C2 通过；篡改一个字节的对象被拒绝并进入 quarantine；没有加入包的电脑无法解密。

### M5：P1 CLI 与体验

- 完整命令：`track`、`list`、`checkpoint`、`sync [--watch] [--auto-install]`、`status`、`project map`、`restore`、`open`、`revisions`、`choose`、`verify`；
- 后台自动安装（按 Agent 启用）与 `open`；
- 分歧保护与目录/代码状态检查；
- 支持矩阵与版本拒绝提示。

**Done**：满足设计 P1 门槛——两台真实电脑交替继续同一 Codex 会话，含往返、重启、断网恢复、Desktop 入口验证；A1–A12 中适用于 Codex 的场景通过。

### M6：P2 WorkBuddy 与转换

- WorkBuddy 适配器：发现、采集（含导入边界）、Writer；
- `convert`：`--plan`、保真度报告、转换写入；
- Codex → WorkBuddy → Codex 往返；
- 转换规则的 golden 测试与保真度报告 schema。

**Done**：B1、B2、B4、B5 通过；达到设计定义的 `roundtrip-supported`（若 M1 决策为较低等级，则按决策等级验收并如实标注）。

### M7：兜底与 MCP

- Resume Pack 生成（确定性，不依赖 LLM）；
- 可选的本地 stdio MCP；
- B3：转换不可用时的降级与标注。

**Done**：转换被拒绝时能生成 `context-only` 并清楚标注原因，不被误报为完成。

## 9. P0 实验手册

### 9.1 隔离环境（E0）

写入类实验不得使用日常数据。可选方式，按可靠性从高到低：

1. 独立的系统用户或虚拟机；
2. 通过环境变量/启动参数让客户端使用独立数据目录（据已知用法 Codex CLI 支持 `CODEX_HOME`；Desktop 与 WorkBuddy 是否遵循待实测）；
3. 对日常目录做完整备份后在原位实验——仅在前两种不可行时使用，且实验前必须验证备份可还原。

每个实验开始前记录：客户端版本、操作系统、数据目录位置；结束后保存环境清单（无正文）。

### 9.2 实验报告模板

```markdown
# E# 标题
- 环境：客户端与版本 / OS / 隔离方式
- 假设：
- 步骤：（可复现，命令或操作序列）
- 观察：（结构、现象；不含正文；必要截图需脱敏）
- 结论：通过 / 失败 / 部分通过
- 对设计的影响：（需要修改的章节与建议）
- 遗留问题：
```

### 9.3 各实验的执行要点

| 实验 | 要点 |
|---|---|
| E1 Codex 最小恢复材料 | 先只放会话文件，看客户端是否回填登记与投影；失败再试补 `session_index.jsonl`；仍失败才评估最小行级登记写入（须备份、客户端关闭、事务内） |
| E2 运行检测与是否需重启 | 分别测：客户端运行中放入新会话是否出现；关闭再开是否出现；有无官方刷新/导入接口；§6.5 各信号的可靠性 |
| E3 Codex → WorkBuddy | 用朴素转换器写一个小会话（纯文本）→ 加入工具调用 → 加入长历史，逐级验证“显示/打开/继续/重开”；记录是否需要写索引、`.meta.json` 的必要字段 |
| E4 WorkBuddy → Codex | 同 E3 反向；重点看 Codex 对缺失字段的容忍度 |
| E5 回采与边界 | 在转换会话里继续对话，验证采集器能划出边界、不重复收录；再转换回另一端 |
| E6 模型接受度 | 对含工具调用/推理被丢弃/文本渲染的会话继续提问，观察是否报错、是否“知道”前情；用超长会话观察上下文窗口的处理 |
| E7 微云行为 | 在目标系统上测乱序到达、冲突副本命名、按需下载、占用与 rename 行为 |
| E8 导入记录位置 | 设备重装/迁移后用同步数据重建 binding；验证不重复纳管 |
| E9 fork/resume | 在 Codex 里 fork 或 resume 一个会话，观察新原生 ID 与文件内容 |

**所有写入类实验先做“只写会话文件”，再逐步增加写入范围；每升一级都先在备份上验证回滚。**

## 10. 测试策略

| 层 | 内容 |
|---|---|
| 单元 | `jsonl`、`canonical`、`session`（分歧）、`convert` 规则、`vault`、`transport`；表驱动，覆盖设计验收场景 |
| Golden | 采集 fixture → segments/canonical；转换 fixture → 目标条目与保真度报告；变更需人工确认 |
| 往返自洽 | Writer 写出的内容经同一适配器的 `Capture` 读回，Canonical 与源一致（排除按规则丢弃的条目） |
| 故障注入 | `failpoint` 覆盖 §6.2 的崩溃矩阵；传输层模拟乱序、重复、占用、半文件 |
| 双设备模拟 | 两个 `SM_HOME` + 一个共享目录，在单进程测试里跑 A→B→A |
| 手动验收 | 真实客户端的“显示/继续/重开”无法进 CI，用 `doc/acceptance.md` 清单逐项勾选并记录版本 |
| 跨平台 | CI 三平台；Windows 额外关注路径、锁、rename 语义、文件占用 |

fixture 规范：脱敏后入库；含一条“应被保留的敏感样式字符串”的反向用例，验证日志与报告不输出正文。

## 11. 安全与隐私开发规范

- `logx` 是唯一的日志入口：只接受 ID、计数、偏移、哈希、错误码；传入正文类字段会被拒绝或替换。
- 保真度报告、probe 输出、诊断信息不得含标题、cwd、正文、secret。
- 明文只允许出现在 `store/staging/` 与内存；staging 用完即清，崩溃遗留由启动恢复清理。
- 备份（`backups/`）含客户端原始数据，0700，设保留期限，`verify` 可列出并清理。
- 密钥与设备私钥只在系统凭据存储；凭据存储不可用时拒绝，不落明文。
- secret 风险扫描（设计 §12）放在首次发布之前；命中时暂停、提示，不上传。
- 任何新增的“写厂商目录”代码路径，PR 必须附：写入范围说明、备份与回滚说明、崩溃测试。

## 12. 错误码与状态

CLI 对外使用稳定的错误码（`--json` 时输出），便于脚本与测试：

| 错误码 | 含义 |
|---|---|
| `unsafe_record` | 遇到完整但无法解析的记录，会话标记 unsafe |
| `file_replaced` / `prefix_mismatch` | 文件被替换/截断/同长度重写 |
| `unknown_format` | 客户端形态、版本或格式不在兼容矩阵内 |
| `client_running` | 目标客户端运行中或无法确认（`Unknown`） |
| `baseline_changed` | 安装前基线与计划不一致 |
| `deps_missing` | 缺少 revision 依赖对象或会话附件 |
| `conflict` | 存在未被选择覆盖的多个 head |
| `secret_detected` | 风险扫描命中，发布被暂停 |
| `fidelity_blocked` | 转换保真度不足，未写入 |
| `target_rejected` | 目标客户端拒绝转换产物（已回滚） |
| `recovery_needed` | 启动发现未完成的 journal，需要先恢复 |

状态枚举沿用设计 §11.3（`captured`、`published`、`received`、`native-ready`/`converted-ready`/`context-ready`、`installed`、`pending-objects`、`waiting-client-close`、`conflict`）。

## 13. 版本与兼容

- 每个持久化 JSON 与加密对象信封都带版本号；读到更高版本拒绝；迁移函数单向、可测试。
- Writer 与适配器各有 `AdapterVersion`，写入 binding 与 receipt；转换产物不同步，可按新 Writer 版本重新生成。
- 支持矩阵以数据文件形式随发布（`internal/agent/matrix/*.json`），未列入的客户端版本一律 `unknown_format`。

## 14. 风险与开放问题

| 风险 | 缓解 |
|---|---|
| Codex/WorkBuddy 不接受外部写入，或必须改私有数据库 | P0 先验证；路线分级；第三级默认关闭；无路线则如实记录阻塞，不降级宣称完成 |
| 客户端版本升级破坏格式 | 兼容矩阵 + 未知版本拒绝写入；采集只读仍可继续 |
| 转换后模型行为异常或上下文超限 | E6 验证；保真度报告展示容量与丢弃项；不静默截断 |
| 微云行为与预期不符 | `Transport` 接口收窄，行为差异收在实现里；以完整 reconcile 为准 |
| P1 范围大（CLI + 加密 + 同步 + 安装） | M3 先用测试 Sealer 打通本地回环，M4 再叠加加密；测试 Sealer 用 build tag 隔离并在发布构建中禁用 |
| 复制来的代码里仍有日报假设 | §7 逐文件处置；M0 清除配置遗留；删除 `Batch`/`OperationID` |

待设计层决策（见设计文档 §17）：第三级登记写入是否启用、加密协议是否简化、协议试点是否先用 Claude Code、原生 ID 保留策略。这些在 M1 决策点一并确认。

## 15. 工作方式

- 分支：每个里程碑一个长期分支，功能分支合入；合入前满足 §8.1。
- 提交：小步、单一目的；P0 实验代码与正式代码分目录（`internal/p0/`），实验结束后只保留结论与 fixture，实验代码不进入发布构建。
- 评审：涉及厂商目录写入、加密、journal 状态机的变更需要单独评审。
- 文档：设计与开发文档变更与代码同一个 PR；实验结论回写设计文档后再继续实现依赖它的部分。
