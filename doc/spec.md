# SessionMemory 实现规格

> 对应设计：[unified-agent-session-sync-design.md](unified-agent-session-sync-design.md)（v0.6）  
> 对应开发计划：[dev.md](dev.md)  
> 状态：Draft / 可直接编码（P0 依赖的部分已逐项标出，见 §17）  
> 日期：2026-10-26

## 0. 如何使用本文

三份文档的分工：

| 文档 | 回答 | 冲突时 |
|---|---|---|
| 设计（design） | 做什么、为什么、验收标准 | 产品规则以它为准 |
| 开发计划（dev） | 按什么顺序做、怎么测、现有代码怎么处置 | 顺序与分工以它为准 |
| **本文（spec）** | **每个类型、字段、文件格式、算法、命令的精确定义** | **字节级与字段级细节以本文为准，dev §4–§5 的草案已被本文取代** |

本文是**规范性**的。约定：**必须**＝不满足即视为缺陷；**应当**＝有正当理由才可偏离；**可以**＝可选。

### 0.1 已验证的参考代码

本文中**整文件嵌入**的 Go 代码块（`session`、`agent`、`canonical`、`convert`、`store/journal`、`transport`、`errcode`、`logx`、`p0` 的类型）和全部测试向量**不是手写的**：它们取自一个独立编译、有测试的参考模块，已通过 `go vet`、`go test`（Go 1.25 语言版本，`golang.org/x/crypto v0.46.0`）。§11 的配置字段片段和 §14.2 的 `codexgen.Options` 是接口草案，未经编译，以其文字说明为准。参考模块随文档一起放在 [`spec/_reference/`](spec/_reference/)（以下划线开头，`go ./...` 会忽略它），目录结构与将来的 `internal/` 一致：

```text
spec/_reference/internal/
  canonjson/   规范化 JSON（实现 + 测试）
  session/     ID、Revision、Snapshot、Binding、Manifest、冲突与落地逻辑（实现 + 测试 + 向量）
  vault/       对象封装、vault.json、加入包（实现 + 测试 + 向量）
  agent/       适配器与 Writer 合约（仅类型）
  canonical/   Canonical 条目与 Validate（实现）
  store/       journal 状态流与恢复表、sessions.db DDL、Store 接口（仅类型与表）
  transport/   Transport 接口（仅类型）
  errcode/     错误码与退出码（实现）
  logx/        脱敏日志属性（实现）
  failpoint/   故障注入（实现，带构建标签）
  jsonl/       仅 Source 接口（其余已在仓库 internal/jsonl）
spec/_reference/cmd/vectors/   用参考实现重新生成本文所有向量
```

**采用方式**：编码时把对应包原样复制到 `internal/` 下，运行其中的测试，之后在此基础上扩展；复制后删除 `spec/_reference/` 里对应文件，避免两份并存。本文给出的向量（§1.4、§2.5、§6.4）必须在你的实现里逐字节复现——这是两台电脑互相兼容的唯一保证。

### 0.2 仍待 P0 决定的部分

凡依赖 Codex / WorkBuddy 实际行为的地方（Writer 的写入细节、路径变换的字段清单、运行检测的最终信号、转换规则的字段映射），本文给出**接口与默认行为**，行为本身标注“待 E#”。这些接口在 P0 之后不变。完整清单见 §17。

## 1. 通用约定

### 1.1 工具链与依赖

- Go 语言版本 `1.25.0`（与 `go.mod` 一致）。标准库已含 `crypto/hkdf`、`crypto/pbkdf2`，不需要为 HKDF 引第三方库。
- 允许的第三方依赖（引入时在 PR 说明理由）：

| 依赖 | 版本 | 用途 |
|---|---|---|
| `golang.org/x/crypto` | `v0.46.0`（已验证与 `go 1.25.0` 兼容；更新的版本先确认其 `go` 指令不超过 1.25） | `argon2` |
| `modernc.org/sqlite` | M3 引入时取当时最新稳定版 | 纯 Go SQLite，Windows 无需 CGO |
| `github.com/klauspost/compress` | M2 引入时取当时最新稳定版 | `zstd` 只读解压 |
| `github.com/spf13/cobra` | M0 引入时取当时最新稳定版 | CLI |
| `github.com/zalando/go-keyring` | M4 引入 | 系统凭据存储（存 vault key） |
| `github.com/fsnotify/fsnotify` | M5 引入 | 降低同步延迟（不依赖它保证正确性） |

### 1.2 标识符

所有标识符是 ASCII 小写字符串，形式 `<前缀>_<主体>`，必须匹配下表的正则；**任何从文件、网络或用户输入读到的标识符都必须先校验再使用**（它们会出现在路径和日志里）。

| 类型 | 形式 | 正则 | 生成 |
|---|---|---|---|
| session | `ss_` + 26 字符 base32 | `^ss_[a-z2-7]{26}$` | 16 字节随机 |
| device | `dev_` + 16 | `^dev_[a-z2-7]{16}$` | 10 字节随机 |
| binding | `bind_` + 16 | `^bind_[a-z2-7]{16}$` | 10 字节随机 |
| tx | `tx_` + 16 | `^tx_[a-z2-7]{16}$` | 10 字节随机 |
| request | `req_` + 16 | `^req_[a-z2-7]{16}$` | 10 字节随机 |
| project | `prj_` + 16 | `^prj_[a-z2-7]{16}$` | 10 字节随机 |
| vault | `vlt_` + 26 | `^vlt_[a-z2-7]{26}$` | 16 字节随机 |
| revision | `rev_` + 64 十六进制 | `^rev_[0-9a-f]{64}$` | 规范化 JSON 的 SHA-256（§2.1） |
| object | `obj_` + 64 十六进制 | `^obj_[0-9a-f]{64}$` | **密文文件**的 SHA-256 |

base32 指 RFC 4648 标准字母表（`A-Z2-7`）、无填充、转小写；随机数一律用 `crypto/rand`。`device_id` 在每台设备上唯一；**从备份恢复或克隆系统时必须重新生成**（§7.4 有检测）。

### 1.3 时间与哈希

- 同步文档里的时间只有一种格式：UTC、毫秒精度，`2006-01-02T15:04:05.000Z`（`session.TimeLayout`）。设备本地文档（binding、journal 等）也用同一格式。时间戳只用于显示和排序提示，**不参与任何冲突或顺序判定**。
- 哈希一律是 SHA-256，输出为小写十六进制（64 字符）。

### 1.4 规范化 JSON

凡是要在多台设备上得到相同字节的地方（revision ID、plan 摘要）都必须用规范化 JSON；参考实现是 `internal/canonjson`（`Marshal`、`Canonicalize`、`Hash`）。规则（RFC 8785 的子集，因为所有键都是 ASCII、所有数字都是整数）：

1. 对象键按其 UTF-8 字节序升序，无重复键；
2. 无多余空白；
3. 字符串只转义 `"`、`\` 和小于 0x20 的控制字符（有短形式的用 `\b \f \n \r \t`，其余用 `\u00xx`，十六进制小写）；`/`、`<`、`>`、`&`、U+2028、U+2029 **原样输出**；
4. 数字必须是 int64 范围内的整数，无前导 0、无小数、无指数，拒绝 `-0`；
5. **不允许 `null`**：可选字段用 `omitempty`（或指针保持 nil）；
6. 数组保持顺序。

输入输出示例（必须逐字节一致）：

| 输入 | 输出 |
|---|---|
| `{"b":1,"a":"x"}` | `{"a":"x","b":1}` |
| 字符串 `a"b\c<换行>\x01</>&/é<U+2028>` | `"a\"b\\c\n\u0001</>&/é<U+2028>"`（`é` 与 U+2028 为原始 UTF-8） |
| `{"z":[{"b":true,"a":false},3]}` | `{"z":[{"a":false,"b":true},3]}` |
| `{"a":1.5}`、`{"a":1e3}`、`{"a":null}`、`{"a":-0}`、`{"a":01}`、`{"a":9223372036854775808}` | **报错** |

结构体经 `encoding/json`（尊重 struct tag）先编码、再按上面规则重编码。字符串内容必须是合法 UTF-8：标识符、路径等在**进入结构体之前**就要校验（`encoding/json` 会把非法字节悄悄换成 U+FFFD）。

## 2. 同步数据类型

下面是 `internal/session` 的完整类型定义（原样取自参考代码）。包内除 `canonjson` 外不 import 任何内部包，也不做 I/O。

### 2.1 Revision 与它的 ID

`revision_id` = `"rev_"` + 下列 JSON 的规范化哈希：把 `Revision` 的 `revision_id` 字段**置空（omitempty 省略）**后做 `canonjson.Hash`。因此 ID 覆盖全部其他字段（包括时间戳、快照引用、父版本）。`Validate()` 重新计算并比对，校验不通过的 revision 一律拒收并隔离。

字段说明：

| 字段 | 说明 |
|---|---|
| `parent_revision_id` | 单个父版本；根版本为空。**不存在“合并版本”**：冲突后用户的选择记录在 `choice` 条目里（§7.3），不产生新的 revision |
| `replaces_revision_id` | 采集发现来源被替换/重写、开启新 generation 时，指向被替代的上一个版本（设计 §8.2）；其余情况为空 |
| `project_ref` | 仅作标签，不参与任何身份判定 |
| `origin_native_id` | **仅根版本**记录来源原生会话 ID（Codex 线程 ID 等），用于避免重复纳管 |
| `native_snapshot_ref` | `Snapshot` 对象（§2.2）的 ObjectRef |
| `repo_state_ref` | 代码状态对象（§2.4）的 ObjectRef，可空 |
| `capture_state` | `checkpoint`（用户执行了 `checkpoint`）或 `auto`（后台在稳定边界采集） |

### 2.2 快照：Artifact 与 Segment

一个原生会话的快照是**文件集合**（设计 §6.3）。`Snapshot` 对象列出全部 `Artifact`（按 `path` 升序）；每个 Artifact 由若干**连续的、不可变的** `SegmentRef` 组成，每个 Segment 是一个密封对象。

关键规则：

1. `path` 是相对于 agent home 的、以 `/` 分隔的逻辑路径；不得以 `/` 开头，不得含 `..`、盘符、反斜杠、NUL；每个分量非空。Codex 的 rollout 无论磁盘上是 `.jsonl` 还是 `.jsonl.zst`，`path` 都写**逻辑名 `.jsonl`**，真实存储形式记在 `form`（`plain`/`zstd`，仅供参考）。
2. `kind = stream`：追加式 JSONL，`segments[i].start/end` 是**解压后字节流**的偏移，`segments[0].start = 0`，`segments[i].end = segments[i+1].start`。`kind = blob`：不可变文件，同样用连续区间表示分块（`start/end` 为文件内偏移）。
3. `raw_hash` = 该区间明文字节的 SHA-256；`object` = 该 Segment 密封后文件的 ObjectRef。**同一设备对同一区间只加密一次**，后续 revision 直接复用同一个 `object`（跨设备不期望去重，因为密文含随机 salt）。
4. `segments_digest` = 对 `raw_hash` 的十六进制字符串按顺序**直接拼接**后的 SHA-256（`SegmentsDigest`）。它不需要保存哈希中间状态：给 Artifact 追加 Segment 只需追加；**深度校验**＝对源文件每个 Segment 的区间重新哈希并与 `raw_hash` 比对（`.zst` 必须从头解压，边解压边校验）。
5. `size` = 所有 Segment 明文长度之和；`mode` = 权限位的**十进制整数**（如 `0o600` 记作 `384`），仅供重建时参考，目标端以自己的 umask/策略为准。
6. 单个 Segment 明文不超过 `MaxSegmentBytes`（8 MiB）；**单条记录大于上限时独占一个 Segment**（该 Segment 可超过上限，但不得超过 `vault.MaxPlainObject`＝16 MiB，超过则该记录视为 `unsafe_record`）。
7. generation：当来源被替换/重写（`jsonl.StatusReplaced`），Artifact 的 `generation` 加 1，Segment 从 0 重新开始，新 revision 带 `replaces_revision_id`；旧 revision 及其 Segment 保持不变。

```go
// internal/session/types.go
// Package session holds the protocol's data types and its pure logic
// (revision identity, head/conflict resolution, install landing). It does no
// I/O and imports no other internal package except canonjson.
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"session-memory/internal/canonjson"
)

// SchemaVersion is the version of every persisted or synced JSON document in
// this package.
const SchemaVersion = 1

// ---- identifiers ----

var (
	reSession  = regexp.MustCompile(`^ss_[a-z2-7]{26}$`)
	reDevice   = regexp.MustCompile(`^dev_[a-z2-7]{16}$`)
	reBinding  = regexp.MustCompile(`^bind_[a-z2-7]{16}$`)
	reTx       = regexp.MustCompile(`^tx_[a-z2-7]{16}$`)
	reRevision = regexp.MustCompile(`^rev_[0-9a-f]{64}$`)
	reObject   = regexp.MustCompile(`^obj_[0-9a-f]{64}$`)
	reRequest  = regexp.MustCompile(`^req_[a-z2-7]{16}$`)
	reVault    = regexp.MustCompile(`^vlt_[a-z2-7]{26}$`)
	reProject  = regexp.MustCompile(`^prj_[a-z2-7]{16}$`)
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func randomID(prefix string, nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system RNG failing is not recoverable
	}
	return prefix + strings.ToLower(b32.EncodeToString(b))
}

func NewSessionID() string  { return randomID("ss_", 16) }
func NewDeviceID() string   { return randomID("dev_", 10) }
func NewBindingID() string  { return randomID("bind_", 10) }
func NewTxID() string       { return randomID("tx_", 10) }
func NewRequestID() string  { return randomID("req_", 10) }
func NewVaultID() string    { return randomID("vlt_", 16) }
func NewProjectRef() string { return randomID("prj_", 10) }

func ValidSessionID(s string) bool  { return reSession.MatchString(s) }
func ValidDeviceID(s string) bool   { return reDevice.MatchString(s) }
func ValidBindingID(s string) bool  { return reBinding.MatchString(s) }
func ValidTxID(s string) bool       { return reTx.MatchString(s) }
func ValidRequestID(s string) bool  { return reRequest.MatchString(s) }
func ValidVaultID(s string) bool    { return reVault.MatchString(s) }
func ValidProjectRef(s string) bool { return reProject.MatchString(s) }
func ValidRevisionID(s string) bool { return reRevision.MatchString(s) }
func ValidObjectRef(s string) bool  { return reObject.MatchString(s) }

// ObjectRefOf builds an object reference from the hex SHA-256 of the sealed
// (ciphertext) object file.
func ObjectRefOf(sealedSHA256Hex string) string { return "obj_" + sealedSHA256Hex }

// ---- time ----

// TimeLayout is the only timestamp format in synced documents: UTC with
// millisecond precision.
const TimeLayout = "2006-01-02T15:04:05.000Z"

func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

func ParseTime(s string) (time.Time, error) { return time.Parse(TimeLayout, s) }

// ---- agent profile ----

// AgentProfile identifies the producer or target client precisely enough to
// choose a compatibility-matrix entry.
type AgentProfile struct {
	Agent          string `json:"agent"`   // codex | workbuddy | claude | ...
	Surface        string `json:"surface"` // desktop | cli | vscode | ...
	Version        string `json:"version,omitempty"`
	Format         string `json:"format,omitempty"` // adapter-defined storage format tag
	AdapterVersion string `json:"adapter_version"`
	OS             string `json:"os"` // darwin | linux | windows
}

// ---- snapshot ----

// Artifact kinds.
const (
	KindStream = "stream" // append-only JSONL, addressed by decompressed byte offsets
	KindBlob   = "blob"   // immutable file
)

// Source forms of a stream artifact (informational).
const (
	FormPlain = "plain"
	FormZstd  = "zstd"
)

// SegmentRef is one immutable chunk of an artifact: bytes [Start, End) of its
// (decompressed) content, stored as one sealed object.
type SegmentRef struct {
	Start   int64  `json:"start"`
	End     int64  `json:"end"`
	RawHash string `json:"raw_hash"` // hex SHA-256 of the plaintext bytes
	Object  string `json:"object"`   // ObjectRef of the sealed segment
}

// Artifact is one file of a native snapshot.
type Artifact struct {
	// Path is relative to the agent home, slash-separated, never starts with
	// '/' or contains ".." or a drive letter. For a Codex rollout it is the
	// logical .jsonl name even when the file on disk is .jsonl.zst.
	Path       string       `json:"path"`
	Kind       string       `json:"kind"`
	Form       string       `json:"form,omitempty"` // streams only
	Generation int          `json:"generation"`
	Mode       uint32       `json:"mode"` // permission bits to recreate, informational
	Size       int64        `json:"size"` // total plaintext bytes
	Digest     string       `json:"segments_digest"`
	Segments   []SegmentRef `json:"segments"` // contiguous from 0, ordered
}

// SegmentsDigest is the hex SHA-256 over the concatenated raw_hash hex strings
// of segs, in order. It identifies the whole plaintext without a running
// hash state: extending an artifact only appends segments. Deep verification
// re-hashes each segment's byte range of the source and compares raw_hash.
func SegmentsDigest(segs []SegmentRef) string {
	h := sha256.New()
	for _, s := range segs {
		h.Write([]byte(s.RawHash))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Snapshot is the sealed object a revision points to: the complete file set
// of the native session at capture time.
type Snapshot struct {
	SchemaVersion int        `json:"schema_version"`
	Artifacts     []Artifact `json:"artifacts"` // sorted by Path
}

// ---- revision ----

// Capture states of a revision.
const (
	CaptureCheckpoint = "checkpoint" // user ran checkpoint
	CaptureAuto       = "auto"       // background capture at a stable boundary
)

// Revision is an immutable version of one synced session. Its ID is the
// SHA-256 of its canonical JSON without the revision_id field.
type Revision struct {
	SchemaVersion      int          `json:"schema_version"`
	RevisionID         string       `json:"revision_id,omitempty"`
	SessionID          string       `json:"session_id"`
	ProjectRef         string       `json:"project_ref,omitempty"`      // label only (design §10)
	OriginNativeID     string       `json:"origin_native_id,omitempty"` // root revision only
	ParentRevisionID   string       `json:"parent_revision_id,omitempty"`
	ReplacesRevisionID string       `json:"replaces_revision_id,omitempty"` // set when a generation bump replaced an earlier snapshot
	ProducerDeviceID   string       `json:"producer_device_id"`
	ProducerBindingID  string       `json:"producer_binding_id"`
	AgentProfile       AgentProfile `json:"agent_profile"`
	NativeSnapshotRef  string       `json:"native_snapshot_ref"`
	RepoStateRef       string       `json:"repo_state_ref,omitempty"`
	CaptureState       string       `json:"capture_state"`
	CreatedAt          string       `json:"created_at"`
}

// ComputeID returns the revision ID for r, ignoring r.RevisionID.
func (r Revision) ComputeID() (string, error) {
	r.RevisionID = ""
	h, err := canonjson.Hash(r)
	if err != nil {
		return "", err
	}
	return "rev_" + h, nil
}

// Seal fills in RevisionID.
func (r *Revision) Seal() error {
	id, err := r.ComputeID()
	if err != nil {
		return err
	}
	r.RevisionID = id
	return nil
}

// Validate checks structure and that RevisionID matches the content.
func (r Revision) Validate() error {
	switch {
	case r.SchemaVersion != SchemaVersion:
		return fmt.Errorf("revision: unsupported schema_version %d", r.SchemaVersion)
	case !ValidSessionID(r.SessionID):
		return fmt.Errorf("revision: bad session_id %q", r.SessionID)
	case r.ProjectRef != "" && !ValidProjectRef(r.ProjectRef):
		return fmt.Errorf("revision: bad project_ref")
	case r.ParentRevisionID != "" && !ValidRevisionID(r.ParentRevisionID):
		return fmt.Errorf("revision: bad parent_revision_id")
	case r.ReplacesRevisionID != "" && !ValidRevisionID(r.ReplacesRevisionID):
		return fmt.Errorf("revision: bad replaces_revision_id")
	case !ValidDeviceID(r.ProducerDeviceID) || !ValidBindingID(r.ProducerBindingID):
		return fmt.Errorf("revision: bad producer ids")
	case !ValidObjectRef(r.NativeSnapshotRef):
		return fmt.Errorf("revision: bad native_snapshot_ref")
	case r.RepoStateRef != "" && !ValidObjectRef(r.RepoStateRef):
		return fmt.Errorf("revision: bad repo_state_ref")
	case r.CaptureState != CaptureCheckpoint && r.CaptureState != CaptureAuto:
		return fmt.Errorf("revision: bad capture_state %q", r.CaptureState)
	}
	if _, err := ParseTime(r.CreatedAt); err != nil {
		return fmt.Errorf("revision: bad created_at: %w", err)
	}
	want, err := r.ComputeID()
	if err != nil {
		return err
	}
	if r.RevisionID != want {
		return fmt.Errorf("revision: revision_id does not match content")
	}
	return nil
}

// ---- device-local binding ----

// Install modes of a binding.
const (
	InstallNative    = "native"
	InstallConverted = "converted"
	InstallContext   = "context"
)

// Binding states.
const (
	StateActive     = "active"
	StateSuperseded = "superseded"
	StateDetached   = "detached"
)

// StreamCursor mirrors jsonl.Cursor field for field (same JSON), so this
// package stays free of other internal imports.
type StreamCursor struct {
	Generation int    `json:"generation"`
	Offset     int64  `json:"offset"`
	FileID     string `json:"file_id,omitempty"`
	HeadHash   string `json:"head_hash,omitempty"`
	TailHash   string `json:"tail_hash,omitempty"`
}

// ArtifactState is what a binding remembers about one captured artifact.
type ArtifactState struct {
	Kind     string        `json:"kind"`
	Form     string        `json:"form,omitempty"`
	Mode     uint32        `json:"mode"`
	Cursor   *StreamCursor `json:"cursor,omitempty"` // streams only
	Segments []SegmentRef  `json:"segments"`         // current generation only
	Size     int64         `json:"size"`
	Digest   string        `json:"segments_digest"` // SegmentsDigest(Segments)
}

// ImportReceipt records what a Writer put into a target so the capture side
// can tell the imported prefix from the user's later records (design §8.3).
type ImportReceipt struct {
	WriterVersion  string `json:"writer_version"`
	SourceRevision string `json:"source_revision"`
	WrittenCount   int64  `json:"written_count"`   // records written
	LastMarker     string `json:"last_marker"`     // id or ordinal of the last written record
	BoundaryOffset int64  `json:"boundary_offset"` // byte offset (decompressed) just after the last written record
	BoundaryHash   string `json:"boundary_hash"`   // hex SHA-256 of bytes [0, BoundaryOffset)
	At             string `json:"at"`
}

// UnsafeMark records where capture stopped at a corrupt record. Capture of the
// binding is paused until the user runs `verify`; revisions before the mark
// stay valid.
type UnsafeMark struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	At     string `json:"at"`
}

// ProjectMap is projects/<project_ref>.json: device-local, never synced.
type ProjectMap struct {
	SchemaVersion int    `json:"schema_version"`
	ProjectRef    string `json:"project_ref"` // prj_<base32>
	Label         string `json:"label,omitempty"`
	LocalPath     string `json:"local_path"`
	// Rules rewrite recorded cwd prefixes to this device's paths (origin
	// /work/app -> local D:\code\app).
	Rules         []PathRuleJSON `json:"rules"`
	GitRemoteHash string         `json:"git_remote_hash,omitempty"` // sha256 of the normalised remote URL
}

// PathRuleJSON is the persisted form of agent.PathRule.
type PathRuleJSON struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Binding maps a synced session to one native session on this device. It is
// the device's source of truth for cursors and receipts.
type Binding struct {
	SchemaVersion       int                      `json:"schema_version"`
	BindingID           string                   `json:"binding_id"`
	SessionID           string                   `json:"session_id"`
	DeviceID            string                   `json:"device_id"`
	AgentProfile        AgentProfile             `json:"agent_profile"`
	NativeSessionID     string                   `json:"native_session_id"`
	NativeHome          string                   `json:"native_home"` // device-local, never synced
	InstallMode         string                   `json:"install_mode"`
	State               string                   `json:"state"`
	BaseRevisionID      string                   `json:"base_revision_id,omitempty"`
	InstalledRevisionID string                   `json:"installed_revision_id,omitempty"`
	Artifacts           map[string]ArtifactState `json:"artifacts"` // key = Artifact.Path
	ImportReceipt       *ImportReceipt           `json:"import_receipt,omitempty"`
	FidelityReportRef   string                   `json:"fidelity_report_ref,omitempty"`
	Unsafe              *UnsafeMark              `json:"unsafe,omitempty"`
	CreatedAt           string                   `json:"created_at"`
	UpdatedAt           string                   `json:"updated_at"`
}
```

### 2.3 示例与向量

下面的值由参考实现生成，**必须原样复现**。

根 revision（`session.Revision`，规范化 JSON；`revision_id` 在哈希时省略，下面是填入后的结果）：

```json
{"agent_profile":{"adapter_version":"0.1.0","agent":"codex","format":"rollout-jsonl","os":"darwin","surface":"desktop","version":"0.150.0"},"capture_state":"checkpoint","created_at":"2026-10-26T10:00:00.000Z","native_snapshot_ref":"obj_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","origin_native_id":"0192f3a4-0000-7000-8000-000000000001","producer_binding_id":"bind_cccccccccccccccc","producer_device_id":"dev_bbbbbbbbbbbbbbbb","project_ref":"prj_dddddddddddddddd","revision_id":"rev_bf5fc65f428edc2e61f0ac445f942553580481f1c0702f63906382b6d571b932","schema_version":1,"session_id":"ss_aaaaaaaaaaaaaaaaaaaaaaaaaa"}
```

`revision_id` = `rev_bf5fc65f428edc2e61f0ac445f942553580481f1c0702f63906382b6d571b932`

以它为父的第二个 revision（`capture_state=auto`，`created_at=2026-10-26T10:05:00.000Z`，无 `origin_native_id`，其余字段同上）：`rev_aa573d28dff0d2a6e8204a85e59033d4e8028c4b3cfb5f579a79c377c57d79a5`

快照对象（`session.Snapshot`，一个 `.zst` 形式的 Codex rollout，两个 Segment）：

```json
{"artifacts":[{"form":"zstd","generation":0,"kind":"stream","mode":384,"path":"sessions/2026/10/26/rollout-2026-10-26T10-00-00-0192f3a4-0000-7000-8000-000000000001.jsonl","segments":[{"end":100,"object":"obj_2222222222222222222222222222222222222222222222222222222222222222","raw_hash":"1111111111111111111111111111111111111111111111111111111111111111","start":0},{"end":250,"object":"obj_4444444444444444444444444444444444444444444444444444444444444444","raw_hash":"3333333333333333333333333333333333333333333333333333333333333333","start":100}],"segments_digest":"709d39a7ffa1ba4ab5e4176518c8b809d838815b55e5305b6fb58f4e2a37fb19","size":250}],"schema_version":1}
```

两个 Segment 的 `segments_digest`（`raw_hash` 分别为 64 个 `1` 和 64 个 `3`）：`709d39a7ffa1ba4ab5e4176518c8b809d838815b55e5305b6fb58f4e2a37fb19`

这些向量在参考模块里有对应的测试（`session_test.go` 的 `TestSpecExampleVectors`），改动字段名、省略规则或规范化规则会让测试失败。

### 2.4 代码状态对象

`repo_state_ref` 指向一个类型为 `TypeRepoState` 的密封对象，明文是规范化 JSON：

```json
{"branch":"main","dirty":{"modified":3,"untracked":1},"head":"<commit sha>","remote_hash":"<sha256 of normalised remote url>","schema_version":1}
```

只记摘要（计数、分支、HEAD、远端哈希），**不含文件内容、不含 diff**（设计 §10）。远端 URL 规范化：小写主机名，去掉 `.git` 后缀、凭据和末尾斜杠，`ssh://git@host/x` 与 `https://host/x` 视为同一远端；再取 SHA-256。

### 2.5 Manifest

Manifest 是设备发布的“日志条目批次”，加密后是同步目录里的一个文件（§7）。下面是 `internal/session/manifest.go` 的完整定义。

规则：

1. 每设备 `sequence` 从 1 开始，**严格逐个加 1**；`prev_manifest_hash` 是该设备上一个 manifest **文件（密文字节）**的 SHA-256，`sequence = 1` 时为空。
2. 一个 manifest 最多 `MaxManifestEntries`（200）个条目；更多则拆成多个，按序发布。
3. `revision` 条目的 `objects` 是**该 revision 新引入**的对象（不在其父版本快照里出现过的），升序；接收方必须先持有该 revision 快照引用的**全部**对象（包括来自旧 revision 的）才能导入，缺失时该 manifest 保持 `pending_objects`（§7.4）。
4. `receipt` 只能由**目标设备自己**发布（设计 §11.3）；`status = installed` 时带 `install_mode`。
5. `choice` 的 `seen_heads` 升序且包含 `chosen`。
6. 幂等键 `Entry.Key()`：重复到达的同一事实只保留一份。

```go
// internal/session/manifest.go
package session

import (
	"fmt"
	"sort"
)

// Entry types of a manifest (design §11.4).
const (
	EntryRevision       = "revision"
	EntryReceipt        = "receipt"
	EntryChoice         = "choice"
	EntryConvertRequest = "convert-request"
	EntryBinding        = "binding"
)

// Receipt statuses.
const (
	ReceiptReceived  = "received"
	ReceiptInstalled = "installed"
)

// Convert-request modes.
const (
	ModeConvert = "convert"
	ModeContext = "context"
)

// Manifest is the plaintext payload of one published manifest object. Its
// sealed form is the file manifests/<device_id>/<seq>-<hash12>.manifest.
type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	DeviceID      string `json:"device_id"`
	// Sequence starts at 1 and grows by exactly 1 per published manifest of
	// this device.
	Sequence int64 `json:"sequence"`
	// PrevManifestHash is the hex SHA-256 of the previous manifest FILE of this
	// device (sealed bytes); empty for sequence 1.
	PrevManifestHash string  `json:"prev_manifest_hash,omitempty"`
	CreatedAt        string  `json:"created_at"`
	Entries          []Entry `json:"entries"`
}

// MaxManifestEntries bounds one manifest; larger batches are split.
const MaxManifestEntries = 200

// Entry is a tagged union: Type selects which pointer is set.
type Entry struct {
	Type           string               `json:"type"`
	Revision       *RevisionEntry       `json:"revision,omitempty"`
	Receipt        *ReceiptEntry        `json:"receipt,omitempty"`
	Choice         *Choice              `json:"choice,omitempty"`
	ConvertRequest *ConvertRequestEntry `json:"convert_request,omitempty"`
	Binding        *BindingEntry        `json:"binding,omitempty"`
}

// RevisionEntry publishes a revision and the objects it introduced (those not
// already referenced by its parent's snapshot). A receiver must hold every
// object of the snapshot before importing the revision (design §11.2).
type RevisionEntry struct {
	Revision Revision `json:"revision"`
	Objects  []string `json:"objects"` // ObjectRefs, sorted
}

// ReceiptEntry is published by the TARGET device only.
type ReceiptEntry struct {
	SessionID   string `json:"session_id"`
	RevisionID  string `json:"revision_id"`
	DeviceID    string `json:"device_id"`
	Status      string `json:"status"`
	InstallMode string `json:"install_mode,omitempty"` // for installed
	BindingID   string `json:"binding_id,omitempty"`
	At          string `json:"at"`
}

// ConvertRequestEntry is what `continue --device` publishes.
type ConvertRequestEntry struct {
	RequestID      string `json:"request_id"`
	SessionID      string `json:"session_id"`
	RevisionID     string `json:"revision_id"`
	SourceDeviceID string `json:"source_device_id"`
	TargetAgent    string `json:"target_agent"`
	TargetDeviceID string `json:"target_device_id"`
	Mode           string `json:"mode"`
	At             string `json:"at"`
}

// BindingEntry is the synced summary of a binding: enough to rebuild a
// device's bindings after a reinstall (E8). It carries no local paths or
// cursors.
type BindingEntry struct {
	Version             int            `json:"version"` // grows with every change of this binding
	BindingID           string         `json:"binding_id"`
	SessionID           string         `json:"session_id"`
	DeviceID            string         `json:"device_id"`
	AgentProfile        AgentProfile   `json:"agent_profile"`
	NativeSessionID     string         `json:"native_session_id"`
	InstallMode         string         `json:"install_mode"`
	State               string         `json:"state"`
	BaseRevisionID      string         `json:"base_revision_id,omitempty"`
	InstalledRevisionID string         `json:"installed_revision_id,omitempty"`
	ImportReceipt       *ImportReceipt `json:"import_receipt,omitempty"`
}

// Validate checks the union and the fields every consumer relies on.
func (e Entry) Validate() error {
	set := 0
	for _, p := range []bool{e.Revision != nil, e.Receipt != nil, e.Choice != nil, e.ConvertRequest != nil, e.Binding != nil} {
		if p {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("entry: exactly one payload must be set, got %d", set)
	}
	switch e.Type {
	case EntryRevision:
		if e.Revision == nil {
			break
		}
		if err := e.Revision.Revision.Validate(); err != nil {
			return err
		}
		if !sort.StringsAreSorted(e.Revision.Objects) {
			return fmt.Errorf("entry: objects must be sorted")
		}
		for _, o := range e.Revision.Objects {
			if !ValidObjectRef(o) {
				return fmt.Errorf("entry: bad object ref %q", o)
			}
		}
		return nil
	case EntryReceipt:
		if r := e.Receipt; r != nil {
			if !ValidSessionID(r.SessionID) || !ValidRevisionID(r.RevisionID) || !ValidDeviceID(r.DeviceID) {
				return fmt.Errorf("entry: bad receipt ids")
			}
			if r.Status != ReceiptReceived && r.Status != ReceiptInstalled {
				return fmt.Errorf("entry: bad receipt status %q", r.Status)
			}
			return nil
		}
	case EntryChoice:
		if c := e.Choice; c != nil {
			if !ValidSessionID(c.SessionID) || !ValidRevisionID(c.Chosen) || !ValidDeviceID(c.DeviceID) {
				return fmt.Errorf("entry: bad choice ids")
			}
			if !sort.StringsAreSorted(c.SeenHeads) {
				return fmt.Errorf("entry: seen_heads must be sorted")
			}
			found := false
			for _, h := range c.SeenHeads {
				found = found || h == c.Chosen
			}
			if !found {
				return fmt.Errorf("entry: chosen must be among seen_heads")
			}
			return nil
		}
	case EntryConvertRequest:
		if c := e.ConvertRequest; c != nil {
			if !ValidRequestID(c.RequestID) || !ValidSessionID(c.SessionID) || !ValidRevisionID(c.RevisionID) ||
				!ValidDeviceID(c.SourceDeviceID) || !ValidDeviceID(c.TargetDeviceID) {
				return fmt.Errorf("entry: bad convert-request ids")
			}
			if c.Mode != ModeConvert && c.Mode != ModeContext {
				return fmt.Errorf("entry: bad convert-request mode %q", c.Mode)
			}
			return nil
		}
	case EntryBinding:
		if b := e.Binding; b != nil {
			if !ValidBindingID(b.BindingID) || !ValidSessionID(b.SessionID) || !ValidDeviceID(b.DeviceID) || b.Version < 1 {
				return fmt.Errorf("entry: bad binding summary")
			}
			return nil
		}
	default:
		return fmt.Errorf("entry: unknown type %q", e.Type)
	}
	return fmt.Errorf("entry: type %q does not match its payload", e.Type)
}

// Key is the idempotency key: entries with the same Key are the same fact and
// are stored once (design §11.4).
func (e Entry) Key() string {
	switch {
	case e.Revision != nil:
		return "revision:" + e.Revision.Revision.RevisionID
	case e.Receipt != nil:
		r := e.Receipt
		return "receipt:" + r.DeviceID + ":" + r.RevisionID + ":" + r.Status
	case e.Choice != nil:
		c := e.Choice
		return "choice:" + c.SessionID + ":" + c.Chosen + ":" + fmt.Sprint(c.SeenHeads) + ":" + c.DeviceID + ":" + c.At
	case e.ConvertRequest != nil:
		return "convert-request:" + e.ConvertRequest.RequestID
	case e.Binding != nil:
		return fmt.Sprintf("binding:%s:%d", e.Binding.BindingID, e.Binding.Version)
	}
	return ""
}

// ManifestFileName is the file name under manifests/<device_id>/: a 12-digit
// zero-padded sequence, then the first 12 hex characters of the SHA-256 of the
// sealed file.
func ManifestFileName(sequence int64, fileSHA256Hex string) string {
	return fmt.Sprintf("%012d-%s.manifest", sequence, fileSHA256Hex[:12])
}
```

`manifests/<device_id>/` 下的文件名 = `ManifestFileName(sequence, 密文文件 SHA-256)`，例如 `000000000001-abababababab.manifest`（12 位补零序号 + 密文哈希前 12 位十六进制）。

序号 1 的 manifest 明文示例（规范化 JSON，含一个 revision 条目）：

```json
{"created_at":"2026-10-26T10:00:01.000Z","device_id":"dev_bbbbbbbbbbbbbbbb","entries":[{"revision":{"objects":["obj_1111111111111111111111111111111111111111111111111111111111111111","obj_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"],"revision":{"agent_profile":{"adapter_version":"0.1.0","agent":"codex","format":"rollout-jsonl","os":"darwin","surface":"desktop","version":"0.150.0"},"capture_state":"checkpoint","created_at":"2026-10-26T10:00:00.000Z","native_snapshot_ref":"obj_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","origin_native_id":"0192f3a4-0000-7000-8000-000000000001","producer_binding_id":"bind_cccccccccccccccc","producer_device_id":"dev_bbbbbbbbbbbbbbbb","project_ref":"prj_dddddddddddddddd","revision_id":"rev_bf5fc65f428edc2e61f0ac445f942553580481f1c0702f63906382b6d571b932","schema_version":1,"session_id":"ss_aaaaaaaaaaaaaaaaaaaaaaaaaa"}},"type":"revision"}],"schema_version":1,"sequence":1}
```

## 3. 本地目录与持久化格式

状态目录 `$SM_HOME`（默认 `~/.session-memory`，可由 `--home`、环境变量 `SM_HOME` 覆盖）。**所有权限：目录 0700，文件 0600**；所有写入走 `platform.AtomicWrite`（临时文件 → fsync → rename → fsync 父目录）。

```text
$SM_HOME/
├── device/
│   ├── device.json            {schema_version, device_id, vault_id, created_at}   # vault_id 用于在凭据存储里找钥匙（实现时新增）
│   ├── sequence.json          {schema_version, last_published}   # 本设备已发布的最大 manifest 序号
│   └── outbox.json            {entries}   # 等待下一个 manifest 的 receipt/choice/convert-request 条目（实现时新增）
├── projects/<project_ref>.json        session.ProjectMap（设备本地，不同步）
├── bindings/<binding_id>.json         session.Binding（设备状态的事实源）
├── journal/<tx_id>.json               store.JournalRecord（进行中的事务）
├── journal/done/<tx_id>.json          已完成的事务，保留 7 天后清理
├── store/
│   ├── objects/<ab>/<64hex>.obj       已验证的密文对象；<ab> 是哈希前两个字符
│   ├── manifests/<device_id>/<file>   已验证的 manifest 密文副本（重建 sessions.db 的依据）
│   ├── staging/<tx_id>/               唯一允许出现明文的临时区（0700）
│   ├── cache/                         Canonical 缓存，可随时删除
│   └── sessions.db                    可重建的 SQLite 视图（§5）
├── backups/<tx_id>/                   写入厂商目录前的备份，保留 30 天
├── quarantine/<yyyymmdd>/<name>       校验失败的对象与 manifest
├── locks/                             store.lock、sync.lock、<session_id>.lock
├── dev/sandboxes/                     P0 工具的隔离沙箱（仅 dev 构建）
└── logs/
```

### 3.1 文件格式规则

- 设备本地 JSON（binding、journal、device、projects）：UTF-8，缩进 2 个空格，末尾换行；**不要求规范化**（不参与哈希）。每个文档带 `schema_version`（当前均为 `1`）。
- 读取时：`schema_version` 大于已知版本 → 拒绝（`unknown_format`，提示升级）；小于已知版本 → 经迁移函数升级后再使用（迁移单向、有测试）；无法解析 → 重命名为 `<name>.corrupt-<UTC 时间>`，返回 `recovery_needed`，**不静默重建**。
- `device.json` 与 `sequence.json` 在 `init` 时创建；`device_id` 一经生成不再变化（除非检测到克隆，§7.4）。

### 3.2 锁

| 锁文件 | 持有者 | 方式 |
|---|---|---|
| `locks/store.lock` | 任何会写本地状态的命令 | 非阻塞排他；取不到时报“另一个 session-memory 正在运行”并以退出码 1 结束 |
| `locks/sync.lock` | `sync --watch` 守护进程 | 非阻塞排他；一台设备上只能有一个守护 |
| `locks/<session_id>.lock` | 对该会话做采集或安装的操作 | 阻塞排他，30 秒超时 |

加锁一律用 `platform.LockExclusive*`（进程退出时系统自动释放，不会留下死锁）。

### 3.3 Binding 文件

`bindings/<binding_id>.json` 是 `session.Binding`（§2.2 代码）。要点：

- `native_home` 与 `artifacts[*].cursor` 是设备本地数据，**永不同步**；
- `artifacts` 的键是 `Artifact.Path`；`segments` 只保留**当前 generation** 的；
- 同一 (`session_id`, 设备, Agent) 同时最多一个 `state = active` 的 binding（由 sessions.db 的唯一索引兜底，§5）；
- `unsafe` 非空表示采集在该位置遇到损坏记录而暂停，直到用户执行 `verify` 处理；
- binding 损坏时**不**从 journal 猜；`verify --repair-binding` 的重建流程：取该会话最新 revision 的快照，逐 Artifact 对源文件做深度校验（§2.2 第 4 点），校验通过则以快照的 Segment 列表重建 `artifacts`，游标取最后一个 Segment 的 `end` 并重新计算首尾哈希；校验不通过则该 Artifact 开启新 generation。

### 3.4 项目映射

`projects/<project_ref>.json` 是 `session.ProjectMap`。`rules` 把会话里记录的来源路径前缀改写成本机路径：匹配按**路径分量边界**、取**最长前缀**；Windows 上比较时不区分大小写并统一分隔符；没有规则匹配时返回原路径且 `ok = false`。`local_path` 必须存在且是目录，否则 `restore`/`convert` 只能出计划（设计 §7.2 第 1 步）。

## 4. Store、事务与崩溃恢复

### 4.1 接口与 journal 类型

下面是 `internal/store/journal.go`。`Flow` 是**状态机与恢复动作的唯一来源**：恢复代码和测试都读它，不另写一份。

```go
// internal/store/journal.go
// Package store owns everything durable on this device: the sealed object
// store, the revision/manifest mirror, bindings, the transaction journal and
// the rebuildable SQLite views.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"session-memory/internal/agent"
	"session-memory/internal/session"
	"session-memory/internal/vault"
)

// ---- journal ----

// Kind is the type of a transaction.
type Kind string

const (
	KindCapture Kind = "capture" // adapter bytes -> objects -> revision -> cursor
	KindPublish Kind = "publish" // objects + manifest -> sync folder
	KindPull    Kind = "pull"    // sync folder -> verified local revisions
	KindInstall Kind = "install" // revision -> vendor store (native, converted, or choice landing)
)

// State names a COMPLETED step: the journal file is rewritten with the new
// State only after the step's effects are durable. Recovery reads the last
// State and performs the next step, which every step makes safe to repeat.
type State string

const (
	StPlanned           State = "planned"
	StStaged            State = "staged"
	StStored            State = "stored"
	StRevisionCommitted State = "revision_committed"
	StCursorAdvanced    State = "cursor_advanced"
	StSealed            State = "sealed"
	StUploaded          State = "uploaded"
	StManifestPublished State = "manifest_published"
	StMarked            State = "marked"
	StVerified          State = "verified"
	StCommitted         State = "committed"
	StBackedUp          State = "backed_up"
	StWritten           State = "written"
	StBindingCommitted  State = "binding_committed"
	StRollingBack       State = "rolling_back"
	StRolledBack        State = "rolled_back"
	StDone              State = "done"
)

// Recovery is what startup recovery does for a journal in a given state.
type Recovery int

const (
	// Discard: nothing durable happened that anything depends on; delete the
	// staging directory and archive the journal.
	Discard Recovery = iota
	// Resume: perform the next step(s) from the journal's Data.
	Resume
	// Undo: roll back using the recorded backups and writes.
	Undo
	// Archive: finished; move the journal to journal/done/.
	Archive
)

// Flow lists, in order, the states of each kind and the recovery action when
// the journal's last State is that state. It is the single source of truth for
// both the state machine and its tests.
var Flow = map[Kind][]struct {
	State    State
	Recovery Recovery
}{
	KindCapture: {
		{StPlanned, Discard}, {StStaged, Discard}, {StStored, Resume},
		{StRevisionCommitted, Resume}, {StCursorAdvanced, Resume}, {StDone, Archive},
	},
	KindPublish: {
		{StPlanned, Resume}, {StSealed, Resume}, {StUploaded, Resume},
		{StManifestPublished, Resume}, {StMarked, Resume}, {StDone, Archive},
	},
	KindPull: {
		{StPlanned, Discard}, {StVerified, Resume}, {StCommitted, Resume}, {StDone, Archive},
	},
	KindInstall: {
		{StPlanned, Discard}, {StStaged, Discard}, {StBackedUp, Undo}, {StWritten, Undo},
		{StVerified, Resume}, {StBindingCommitted, Resume}, {StDone, Archive},
	},
}

// Rollback states shared by KindInstall.
var RollbackFlow = []struct {
	State    State
	Recovery Recovery
}{{StRollingBack, Undo}, {StRolledBack, Archive}}

// RecoveryFor returns the action for a journal in state s.
func RecoveryFor(k Kind, s State) (Recovery, error) {
	for _, e := range Flow[k] {
		if e.State == s {
			return e.Recovery, nil
		}
	}
	if k == KindInstall {
		for _, e := range RollbackFlow {
			if e.State == s {
				return e.Recovery, nil
			}
		}
	}
	return 0, fmt.Errorf("store: kind %q has no state %q", k, s)
}

// JournalRecord is journal/<tx_id>.json (completed ones move to journal/done/).
type JournalRecord struct {
	SchemaVersion int             `json:"schema_version"`
	TxID          string          `json:"tx_id"`
	Kind          Kind            `json:"kind"`
	SessionID     string          `json:"session_id,omitempty"`
	BindingID     string          `json:"binding_id,omitempty"`
	State         State           `json:"state"`
	StartedAt     string          `json:"started_at"`
	UpdatedAt     string          `json:"updated_at"`
	Data          json.RawMessage `json:"data"`
	LastError     string          `json:"last_error,omitempty"` // code only
}

// ---- per-kind journal data ----

// CaptureData is JournalRecord.Data for KindCapture. Everything recovery needs
// to commit the revision is here, so it never re-reads the source.
type CaptureData struct {
	Artifacts  map[string]session.ArtifactState `json:"artifacts"` // the state to commit
	Snapshot   string                           `json:"snapshot"`  // ObjectRef
	RepoState  string                           `json:"repo_state,omitempty"`
	Revision   *session.Revision                `json:"revision,omitempty"` // sealed, set at StStored
	Generation bool                             `json:"generation_bump,omitempty"`
}

// PublishData is JournalRecord.Data for KindPublish.
type PublishData struct {
	Sequence     int64    `json:"sequence"`
	Entries      int      `json:"entries"`
	Objects      []string `json:"objects"`                 // ObjectRefs to upload
	ManifestFile string   `json:"manifest_file,omitempty"` // file name, set at StSealed
	ManifestHash string   `json:"manifest_hash,omitempty"` // hex SHA-256 of the sealed bytes
	// the sealed manifest bytes are kept in staging/<tx_id>/manifest.sealed so a
	// resumed publish re-sends the SAME bytes (never two files for one sequence)
}

// PullData is JournalRecord.Data for KindPull.
type PullData struct {
	DeviceID  string   `json:"device_id"`
	Manifests []string `json:"manifests"` // file names
	Revisions []string `json:"revisions"`
}

// InstallData is JournalRecord.Data for KindInstall.
type InstallData struct {
	PlanDigest string               `json:"plan_digest"`
	Mode       agent.Mode           `json:"mode"`
	Route      agent.Route          `json:"route"`
	Home       string               `json:"home"`
	BackupDir  string               `json:"backup_dir"`
	Backed     []string             `json:"backed,omitempty"`  // relative paths copied to BackupDir
	Created    []string             `json:"created,omitempty"` // files that did not exist before
	Written    []agent.PlannedWrite `json:"written,omitempty"`
	Landing    string               `json:"landing,omitempty"` // "choice" for a §9.2 landing
	Receipt    *agent.Receipt       `json:"receipt,omitempty"` // set at StVerified
	NewBinding *session.Binding     `json:"new_binding,omitempty"`
}

// Tx is an open transaction. Every method that changes state writes the
// journal atomically first.
type Tx interface {
	ID() string
	Kind() Kind
	// SetState records a completed step and its data (nil keeps Data).
	SetState(s State, data any) error
	// StagingDir is store/staging/<tx_id>/ (0700). Plaintext may exist only here.
	StagingDir() string
	agent.Tx // BackupDir, Backed, Wrote
	// Finish moves the journal to done/ and removes StagingDir.
	Finish() error
}

// ---- store ----

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Ref         string // session.ObjectRefOf(sha256 of the sealed file)
	PlainSize   int64
	PlainSHA256 string
	SealedSize  int64
}

// Store is the local durable state. Implementations must be safe for use by
// one process at a time per SM_HOME (guarded by locks/store.lock).
type Store interface {
	// Objects
	PutObject(ctx context.Context, t vault.ObjectType, extra []byte, r io.Reader) (ObjectInfo, error)
	PutSealed(ctx context.Context, ref string, sealed io.Reader) error // verifies sha256(sealed) == ref
	OpenObject(ctx context.Context, ref string, t vault.ObjectType, extra []byte) (io.ReadCloser, error)
	SealedBytes(ctx context.Context, ref string) (io.ReadCloser, error) // for upload
	HasObject(ref string) bool

	// Transactions
	Begin(ctx context.Context, k Kind, sessionID, bindingID string) (Tx, error)
	Recover(ctx context.Context) ([]RecoveryReport, error) // run at every start

	// Revisions and views
	CommitRevision(tx Tx, r session.Revision) error // all objects of its snapshot must exist
	Graph(sessionID string) (*session.Graph, error)
	Choices(sessionID string) ([]session.Choice, error)

	// Bindings (JSON files are the source of truth)
	LoadBinding(id string) (session.Binding, error)
	SaveBinding(tx Tx, b session.Binding) error
	Bindings(sessionID string) ([]session.Binding, error)

	// Rebuild drops sessions.db and rebuilds it from store/manifests, bindings and objects.
	Rebuild(ctx context.Context) error
}

// RecoveryReport is one journal's recovery outcome, for logs and `verify`.
type RecoveryReport struct {
	TxID   string
	Kind   Kind
	State  State
	Action Recovery
	Err    string // code only
}
```

### 4.2 journal 写入协议

1. `Begin` 先写 `journal/<tx_id>.json`（`State = planned`），**然后**才做该事务的任何副作用。
2. 每完成一个步骤，用 `Tx.SetState(新状态, data)` 重写 journal（`AtomicWrite`）。**State 表示“该步骤已完成且其效果已持久”**；先改 State 再做动作是错误的。
3. 每个步骤**必须幂等**：重复执行不产生第二份对象、第二个 revision 或第二次写入。
4. `Finish` 把 journal 移到 `journal/done/` 并删除 `staging/<tx_id>/`。
5. 启动时（以及每次 `sync` 开始前）调用 `Store.Recover`：按 `StartedAt` 升序扫描 `journal/*.json`，对每个调用 `RecoveryFor(kind, state)` 并执行下表的动作；恢复失败的事务保留原样，记 `recovery_needed`，不影响其他会话。

### 4.3 各事务的步骤与恢复

**capture**（采集 → 本地对象 → revision → 游标）

| 完成的步骤（State） | 此时持久的内容 | 崩溃后恢复 |
|---|---|---|
| `planned` | 仅 journal | Discard：删除 `staging/<tx>/` |
| `staged` | 部分对象在 staging，尚未进入 `objects/` | Discard（游标未动，下次重新采集同一区间；按 `(generation, start, end, raw_hash)` 去重） |
| `stored` | 全部对象已移入 `objects/`（含 Snapshot、RepoState）；`Data.Revision` 已封好 | Resume：`CommitRevision(Data.Revision)` |
| `revision_committed` | revision 已在 sessions.db 与 manifests 缓冲 | Resume：保存 binding（`Data.Artifacts` 作为新游标/Segment 列表） |
| `cursor_advanced` | binding 已更新 | Resume：入队 publish 事务，`Finish` |
| `done` | — | Archive |

步骤细节：`staged` ＝每个 Segment 明文经 `Store.PutObject` 封装后写入 `staging/<tx>/<随机>.obj`；`stored` ＝对象 `rename` 到 `objects/<ab>/<hash>.obj`（同名已存在则丢弃临时文件）；**游标只在 `revision_committed` 之后推进**。

**publish**（对象与 manifest → 同步目录）

| State | 持久内容 | 恢复 |
|---|---|---|
| `planned` | `Data.Sequence` 已分配（`sequence.json.last_published + 1`），尚未使用 | Resume：封装 manifest |
| `sealed` | 密封后的 manifest 字节已存 `staging/<tx>/manifest.sealed`，`Data.ManifestHash/File` 已记 | Resume：上传对象 |
| `uploaded` | `Data.Objects` 全部在同步目录 | Resume：发布 manifest（**重发同一份字节**，绝不重新封装） |
| `manifest_published` | manifest 文件在同步目录 | Resume：更新 `sequence.json` |
| `marked` | `sequence.json.last_published = Data.Sequence` | Resume：`Finish` |
| `done` | — | Archive |

**同一序号绝不允许出现两份不同字节**：`sealed` 之后重新封装会改变随机 salt/nonce，从而得到不同文件名。恢复时若发现同步目录已有该序号但哈希不同，按 `device_id_conflict` 处理（§7.4）。

**pull**（同步目录 → 本地已验证的 revision）

| State | 恢复 |
|---|---|
| `planned` | Discard |
| `verified`（manifest 已验证、对象齐全，`Data` 列出将导入的 revision） | Resume：写入 sessions.db 与 `store/manifests/` |
| `committed` | Resume：发布 `received` 回执，`Finish` |
| `done` | Archive |

**install**（revision → 厂商存储；也用于原生更新、转换写入、冲突落地）

| State | 持久内容 | 恢复 |
|---|---|---|
| `planned` | plan 摘要已记（`Data.PlanDigest`） | Discard |
| `staged` | 要写的文件已在 `staging/<tx>/` 构建完成 | Discard |
| `backed_up` | 所有将被替换/追加的目标文件已复制到 `backups/<tx>/`（`Data.Backed`），新建文件已列入 `Data.Created` | **Undo** |
| `written` | 目标文件已写入（`Data.Written`），尚未验证 | **Undo** |
| `verified` | 文件已读回并校验通过，**且要提交的 binding 已完整准备好**（`Data.Receipt`、`Data.NewBinding` 已记；由 engine 在采纳已安装文件后写入，Writer 自己不写这个状态）。实现时据崩溃矩阵测试修正：若 `verified` 时 binding 尚未准备，恢复无法前滚 | Resume：保存 binding |
| `binding_committed` | binding 已更新 | Resume：发布 receipt/binding 条目，`Finish` |
| `rolling_back` | 回滚进行中 | Undo（继续） |
| `rolled_back` | 已回滚 | Archive |

Undo 的做法：对 `Data.Backed` 中每个文件，用备份覆盖回去；对 `Data.Created` 中每个文件，删除；恢复完成后写 `rolled_back`。**回滚后目标目录必须与安装前逐字节一致**（测试要求，§14.4）。

### 4.4 安装事务的写入规则

1. 获取 `locks/<session_id>.lock`；检测运行状态（`agent.RuntimeDetector`），`Running`/`Unknown` 一律不写，返回 `client_running`（设计 §9.3）。
2. 重新计算目标的 `BaselineHash` 并与 plan 比对，不一致返回 `baseline_changed`。
3. 在 `staging/<tx>/` 构建全部将写入的内容；`Plan.Digest` 校验通过。
4. 备份：每个将被替换/追加的目标文件**整文件复制**到 `backups/<tx>/<相对路径>`（保持权限）；记入 `Data.Backed`（这一步完成才写 `backed_up`）。
5. 写入：每个文件用“同目录临时文件 → fsync → rename”替换（**同一设备的同一文件系统内**，保证原子）；保持原权限位；内容变化的文件把 mtime 设为当前时间。向已有文件追加按“读旧内容 + 增量 → 临时文件 → rename”实现，不就地 `O_APPEND`。每写完一个文件调用 `Tx.Wrote`。
6. **写完之后再检测一次运行状态**；若期间客户端启动，立即 Undo 并返回 `client_running`（设计 §6.4 第 7 点）。
7. 验证：读回每个写入文件的哈希与 plan 一致，并用适配器的校验器解析（记录可解析、条目数一致）；失败则 Undo 并返回 `target_rejected`。
8. Codex 目标额外规则（来源：设计 §3.3“参考实现”，待 E1b 验证）：步骤 5 的 rename 之前取 `thread-writer-locks/.coordination.lock`，再尝试取该线程的锁，取不到就放弃并返回 `client_running`；没有 `thread-writer-locks` 目录的旧版 Codex 不写。

## 5. sessions.db

`store/sessions.db` 是**可重建的视图**，不是事实源。事实源是：`store/objects/`、`store/manifests/`、`bindings/`、`journal/`。

- 打开时检查 `PRAGMA user_version`：不等于 `DBSchemaVersion` 则删除文件并 `Rebuild`，**不做迁移**；
- `Rebuild`：按 `(device_id, sequence)` 顺序读取 `store/manifests/` 里的每个 manifest（用 vault 解密）→ 重放 `entries` → 写表；再读 `bindings/*.json` 写 `bindings_view`；
- 所有写入在 SQLite 事务内完成；对 `objects` 表的写入以文件系统为准（`objects/` 下有文件才算存在）；
- 连接参数：`journal_mode=WAL`、`foreign_keys=ON`、`busy_timeout=5000`。

DDL（已用 `sqlite3` 执行验证，唯一索引确实拒绝同一 (session, agent) 的第二个 active binding）：

```sql
CREATE TABLE sessions (
  session_id        TEXT PRIMARY KEY,
  origin_agent      TEXT NOT NULL,
  origin_native_id  TEXT NOT NULL,
  project_ref       TEXT,
  created_at        TEXT NOT NULL
);

CREATE TABLE revisions (
  revision_id        TEXT PRIMARY KEY,
  session_id         TEXT NOT NULL REFERENCES sessions(session_id),
  parent_revision_id TEXT,
  producer_device_id TEXT NOT NULL,
  agent              TEXT NOT NULL,
  snapshot_ref       TEXT NOT NULL,
  repo_state_ref     TEXT,
  capture_state      TEXT NOT NULL,
  created_at         TEXT NOT NULL,
  status             TEXT NOT NULL CHECK (status IN ('committed','pending_parent','pending_objects')),
  json               TEXT NOT NULL            -- canonical Revision JSON
);
CREATE INDEX revisions_session ON revisions(session_id, created_at);
CREATE INDEX revisions_parent  ON revisions(parent_revision_id);

CREATE TABLE objects (
  ref          TEXT PRIMARY KEY,
  object_type  INTEGER NOT NULL,
  sealed_size  INTEGER NOT NULL,
  added_at     TEXT NOT NULL
);

CREATE TABLE revision_objects (
  revision_id TEXT NOT NULL REFERENCES revisions(revision_id),
  ref         TEXT NOT NULL,
  PRIMARY KEY (revision_id, ref)
);

CREATE TABLE manifests (
  device_id    TEXT NOT NULL,
  sequence     INTEGER NOT NULL,
  file_name    TEXT NOT NULL,
  file_sha256  TEXT NOT NULL,
  prev_hash    TEXT,
  status       TEXT NOT NULL CHECK (status IN ('applied','pending_prev','pending_objects','quarantined')),
  seen_at      TEXT NOT NULL,
  PRIMARY KEY (device_id, sequence)
);

CREATE TABLE entries (            -- applied manifest entries, deduplicated by key
  entry_key  TEXT PRIMARY KEY,
  type       TEXT NOT NULL,
  device_id  TEXT NOT NULL,
  sequence   INTEGER NOT NULL,
  json       TEXT NOT NULL
);

CREATE TABLE receipts (
  session_id  TEXT NOT NULL,
  revision_id TEXT NOT NULL,
  device_id   TEXT NOT NULL,
  status      TEXT NOT NULL,
  install_mode TEXT,
  at          TEXT NOT NULL,
  PRIMARY KEY (device_id, revision_id, status)
);

CREATE TABLE choices (
  entry_key   TEXT PRIMARY KEY,
  session_id  TEXT NOT NULL,
  chosen      TEXT NOT NULL,
  seen_heads  TEXT NOT NULL,       -- JSON array
  device_id   TEXT NOT NULL,
  at          TEXT NOT NULL
);

CREATE TABLE bindings_view (        -- mirror of bindings/*.json
  binding_id            TEXT PRIMARY KEY,
  session_id            TEXT NOT NULL,
  agent                 TEXT NOT NULL,
  native_session_id     TEXT NOT NULL,
  install_mode          TEXT NOT NULL,
  state                 TEXT NOT NULL,
  installed_revision_id TEXT,
  updated_at            TEXT NOT NULL
);
CREATE UNIQUE INDEX one_active_binding
  ON bindings_view(session_id, agent) WHERE state = 'active';

CREATE TABLE quarantine (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  path      TEXT NOT NULL,
  reason    TEXT NOT NULL,          -- an errcode.Code
  at        TEXT NOT NULL
);

CREATE TABLE canonical_cache (      -- derived; safe to delete at any time
  revision_id     TEXT NOT NULL,
  adapter_version TEXT NOT NULL,
  path            TEXT NOT NULL,    -- cache file under store/cache/
  PRIMARY KEY (revision_id, adapter_version)
);
```

## 6. 加密与 vault

信任模型（设计 §12，已定为简化方案）：所有持有 vault key 的设备**同等受信**；没有每设备密钥、签名或授权记录；同步目录里每个字节的真实性来自 AES-256-GCM（只有持有 vault key 的设备能产生有效密文）。`device_id` 只是标签。

### 6.1 对象信封

参考实现：`internal/vault/vault.go`（`Seal`/`Open`），测试覆盖：往返、类型绑定、vault 绑定、额外 AAD 绑定、逐字节翻转都被检出、截断被拒绝。

```text
magic "SMOB" (4) | version u8 = 1 | type u8 | salt[32] | nonce[12] | ciphertext + tag(16)
```

- **对象密钥** = `HKDF-SHA256(secret = vault key, salt = 信封中的 salt, info = "smv1/object/" + vault_id + "/" + 十进制 type, 长度 32)`（标准库 `crypto/hkdf.Key`）；
- **加密** = AES-256-GCM，nonce 为信封中的 12 字节；每个对象新生成随机 salt 与 nonce；
- **AAD** = `magic ‖ version ‖ type ‖ 0x00 ‖ vault_id ‖ 0x00 ‖ extra`；`extra` 对普通对象为空，对 manifest 见 §6.2；
- `type`：`1` snapshot、`2` segment、`3` blob、`4` repo_state、`5` manifest、`6` report、`7` key_check；
- 明文上限 `MaxPlainObject`（16 MiB）：单次 AEAD，整个对象在内存中；
- `Open` 任何失败（魔数、版本、类型、认证）都返回 `ErrAuth`，调用方隔离该文件（`auth_failed`），**不区分原因**；
- `ObjectRef` = `"obj_"` + 密文文件整体的 SHA-256；ObjectRef 与内容绑定，`PutSealed` 必须先校验哈希再落盘。

### 6.2 Manifest 的 AAD

Manifest 使用 `type = 5`，`extra = ManifestAAD(device_id, sequence, prev_manifest_hash)`：

```text
0x00 ‖ device_id ‖ 0x00 ‖ sequence(uint64 大端, 8 字节) ‖ 0x00 ‖ prev_manifest_hash（十六进制文本，首个 manifest 为空）
```

这样既保证 manifest 不能被换到另一个设备/序号下，也让每个设备的 manifest 链不可重排或截取（链断裂＝认证失败）。

### 6.3 vault.json 与加入包

`public/vault.json`（明文，同步目录里）：

```json
{"schema_version":1,"vault_id":"vlt_...","created_at":"2026-10-26T10:00:00.000Z","key_check":"<base64>"}
```

`key_check` 是 `Seal(TypeKeyCheck, nil, "session-memory vault key check v1")` 的 base64；加入设备在解开加入包后用 `CheckKey` 验证，**验证通过才读取任何数据**。

**加入包**（`device invite` 产出，同时也是离线恢复包；`smbundle1`）：JSON 文件，字段 `format`、`vault_id`、`kdf{alg:"argon2id", time:3, memory_kib:65536, threads:4, salt(b64,16 字节)}`、`nonce(b64,12 字节)`、`ciphertext(b64)`。`ciphertext = AES-256-GCM(key = argon2id(口令, salt, 32 字节), nonce, 明文 = 32 字节 vault key, AAD = "smbundle1" 0x00 vault_id)`。口令至少 12 个字符；口令与加入包**分开传递**，不放同步目录。参数写在包里，将来可以提高。

### 6.4 向量

固定 vault key = 字节 `00 01 02 ... 1f`，vault_id = `vlt_aaaaaaaaaaaaaaaaaaaaaaaaaa`，salt = 32 个 `a0`，nonce = 12 个 `b0`，type = segment（2），extra 为空，明文 `hello`，密文文件（71 字节）十六进制：

```text
534d4f420102a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0b0b0b0b0b0b0b0b0b0b0b0b083e0343efdff3017d57ba5314835b269a73037d494
```

同样的 key/salt/nonce，type = manifest（5），明文 `{}`，extra = `ManifestAAD("dev_bbbbbbbbbbbbbbbb", 7, "00")`，其中 extra 的十六进制为 `006465765f62626262626262626262626262626262000000000000000007003030`，密文文件：

```text
534d4f420105a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0b0b0b0b0b0b0b0b0b0b0b0b04474e5d2bb028b860d7cef85d5a6cb0646f0
```

（对应测试：`vault_test.go` 的 `TestSealVector`，实现必须在固定 salt/nonce 下产出完全相同的字节。）

### 6.5 密钥保管

vault key 只在内存和系统凭据存储（`go-keyring`，服务名 `session-memory`，账户名 = vault_id）；凭据存储不可用时拒绝运行需要密钥的命令，**不得回退到明文文件**（`no_vault`）。后台常驻进程取不到密钥时不安装、不上传，状态显示“等待解锁”，不反复报错（设计 §6.4 第 6 点）。

## 7. 传输与同步

### 7.1 同步目录布局

```text
SessionMemoryVault/v1/
├── public/vault.json
├── objects/<ab>/<64hex>.obj                  # <ab> = 哈希前两个字符；文件名即 ObjectRef 去掉 obj_
└── manifests/<device_id>/<12 位序号>-<12 位哈希>.manifest
```

所有发布文件**写一次、不可变**；没有任何多设备共同改写的文件。

### 7.2 Transport 合约

```go
// internal/transport/transport.go
// Package transport moves immutable files to and from the shared folder. It
// knows nothing about sessions, encryption or manifests.
package transport

import (
	"context"
	"errors"
	"io"
	"time"
)

// Entry is one file in the shared folder.
type Entry struct {
	Path    string // slash-separated, relative to the transport root
	Size    int64
	ModTime time.Time
	// Suspect marks names that look like a sync client's conflict copy
	// ("name (1).ext", "name (conflicted copy).ext", "name-conflict-..."):
	// listed, but the sync engine verifies them by content (E7 refines this).
	Suspect bool
}

// ErrExists is returned by Put when the path exists with different content.
var ErrExists = errors.New("transport: path exists with different content")

// ErrNotStable means the file is still being written or downloaded; retry.
var ErrNotStable = errors.New("transport: file not stable yet")

// Transport is the whole contract.
type Transport interface {
	// Put publishes r at path, write-once: the bytes go to a sibling
	// "<name>.partial-<uuid>" file, are fsynced and closed, then renamed to
	// path. If path already exists with identical content, Put succeeds; with
	// different content it returns ErrExists. Partial files are never listed.
	Put(ctx context.Context, path string, r io.Reader) error
	// List returns files under prefix (recursive), excluding partial files.
	List(ctx context.Context, prefix string) ([]Entry, error)
	// Get opens path. ErrNotStable means a file of that name exists but cannot
	// be trusted yet.
	Get(ctx context.Context, path string) (io.ReadCloser, error)
	// Stable reports whether e is safe to read: size and modification time were
	// unchanged across Settle and the file opens for reading.
	Stable(ctx context.Context, e Entry) (bool, error)
}

// FolderOptions tunes FolderTransport.
type FolderOptions struct {
	Root   string
	Settle time.Duration // default 2s
	// Backoff for ErrNotStable: starts at 1s, doubles, caps at 60s, gives up
	// after 10 minutes (the entry stays pending and is retried by the next
	// reconcile).
}

// PartialSuffix marks unfinished files; importers must ignore any name
// containing it.
const PartialSuffix = ".partial-"
```

`FolderTransport` 的行为：

- **Put**：在目标同目录写 `<name>.partial-<uuid>` → 写完 `fsync` → 关闭 → `rename` 为最终名。最终名已存在时：内容完全相同则成功（幂等），不同则 `ErrExists`。`rename` 失败（文件被同步客户端占用等）按退避重试，最长 10 分钟；
- **List**：递归列出，**忽略任何含 `.partial-` 的名字**；名字像同步客户端冲突副本的（`name (1).ext`、`name (conflicted copy).ext`、`name-conflict-*`）照常列出但 `Suspect = true`（E7 实测后调整模式）；
- **Stable**：同一文件在间隔 `Settle`（默认 2 秒）的两次检查中大小和修改时间不变，并且能以读模式打开（Windows 上没有被独占占用）；
- **Get**：先 `Stable`，不稳定返回 `ErrNotStable`；
- 退避：1 秒起、每次翻倍、上限 60 秒、总计 10 分钟后放弃本轮（条目保持待处理，下一次 reconcile 再试）。

### 7.3 发布算法（publish 事务）

```text
publish(entries []Entry):
  tx = Begin(publish)                       # State planned；Data.Sequence = sequence.json.last_published + 1
  objs = 所有 revision 条目的 Objects 去重排序
  manifest = {schema_version:1, device_id, sequence, prev_manifest_hash, created_at, entries}
  plain   = canonjson.Marshal(manifest)
  sealed  = vault.Seal(TypeManifest, ManifestAAD(device_id, sequence, prev_manifest_hash), plain)
  写 staging/<tx>/manifest.sealed ; Data.ManifestHash = sha256(sealed) ; State = sealed
  for ref in objs 且同步目录没有: transport.Put("objects/<ab>/<hash>.obj", store.SealedBytes(ref))
  State = uploaded
  transport.Put("manifests/<device_id>/<ManifestFileName>", sealed)       # 同一份字节
  State = manifest_published ; 写 sequence.json ; State = marked ; Finish
```

- `prev_manifest_hash` 取本地 `store/manifests/<device_id>/` 中序号 `sequence-1` 的文件哈希；
- 一批条目超过 `MaxManifestEntries` 时拆成连续的多个 publish 事务；
- **先传对象、后传 manifest**：接收方看到 manifest 时对象一定已经在（微云乱序到达时仍可能先看到 manifest，由接收方的 `pending_objects` 兜底）。

### 7.4 接收算法（reconcile / pull）

触发：`sync` 开始时；`--watch` 下 fsnotify 事件只“提前触发一次 reconcile”；定时完整 reconcile 默认每 60 秒（**正确性只靠完整 reconcile**，不依赖 mtime 或事件）。

```text
reconcile():
  1. List("manifests/") ；按设备目录分组，名字不匹配 ^\d{12}-[0-9a-f]{12}\.manifest$ 的忽略（Suspect 的另行按内容判重）
  2. 对每个设备按序号升序处理 sequence > 本地已 applied 的最大序号 的文件：
     a. Stable 否 → 跳过，下轮再来
     b. Get 全部字节；sha256 前 12 位必须等于文件名里的哈希 → 否则 quarantine(hash_mismatch)
     c. 序号必须等于 已处理最大序号 + 1 → 否则状态 pending_prev（等缺的那个）
     d. prev = 本地记录的 (sequence-1) 的 file_sha256（首个为空）；
        vault.Open(TypeManifest, ManifestAAD(device, seq, prev), bytes) 失败 → quarantine(auth_failed)
     e. 反序列化 Manifest；检查 schema_version、device_id==目录名、sequence==文件名序号、
        prev_manifest_hash==prev、entries≤200、每个 Entry.Validate() → 否则 quarantine(entry_invalid)
     f. 对每个 revision 条目：快照对象与 Objects 必须全部在本地；缺的逐个 Get+PutSealed（校验哈希）；
        仍缺 → manifest 状态 pending_objects（保留，下轮重试；此前不导入其中任何 revision）
     g. 对象齐全 → pull 事务：校验每个 Snapshot（路径规则、Segment 连续性、digest）、
        重新计算每个 revision 的 ID，通过后一次性写入 sessions.db 并把 manifest 副本存入 store/manifests/
     h. 按 Entry.Key() 去重后应用 entries（revision→revisions 表；receipt→receipts；choice→choices；
        convert-request→待用户处理；binding→bindings 摘要）
  3. 对每个有变化的会话：重算 heads/冲突（§10）→ 更新状态；满足条件且已启用自动安装的，入队 install
  4. 发布本机对新 revision 的 received 回执（经 publish 事务）
```

隔离原因（`quarantine.reason`，都是 `errcode.Code`）：

| reason | 含义 |
|---|---|
| `auth_failed` | 解密/认证失败（换了 vault、被篡改、被截断、序号/设备/前链不符） |
| `hash_mismatch` | 文件名里的哈希与内容不符（下载不完整或被改名） |
| `entry_invalid` | 结构非法、条目与类型不符、ID 校验失败、revision 的 ID 与内容不符 |
| `unknown_format` | `schema_version` 比本程序高 |

**本机 ID 克隆检测**：`sync` 开始前，List 出 `manifests/<本机 device_id>/`，若存在序号大于本地 `sequence.json.last_published` 的文件（且不是本机 journal 中进行中的 publish），说明 ID 被克隆或本地状态回退：**拒绝一切发布**，返回 `device_id_conflict`，要求执行 `device regenerate-id`（生成新的 `device_id`，旧 ID 的历史保持原样）。

### 7.5 状态到 CLI 的映射

`status` 命令把设计 §11.3 的状态按 (session, 设备) 计算：

| 状态 | 判据 |
|---|---|
| `captured` | 本地有 revision（binding 的最新采集） |
| `published` | 该 revision 所在的 manifest 已 `marked` |
| `received`（对某设备） | 收到该设备发布的 `receipt{status=received}` |
| `installed`（对某设备） | 收到该设备的 `receipt{status=installed}`，并带 `install_mode` |
| `pending-objects` | 存在 `pending_objects` 的 manifest 引用了该会话 |
| `waiting-client-close` | 最近一次 install 因 `client_running` 中止，且仍有可落地的新 revision |
| `conflict` | `Resolve(...)` 返回冲突 |
| `native-ready` / `converted-ready` / `context-ready` | 对象齐全、该 Agent 的 Writer 兼容矩阵通过、等待安装 |

## 8. Agent 适配器

### 8.1 合约（`internal/agent`）

下面是适配器、Writer 和相关类型的完整定义。**接口形状在 P0 前后不变**；P0 决定的是 Writer 的行为（§17）。

```go
// internal/agent/agent.go
// Package agent defines the contracts between the core (store, sync, CLI) and
// each agent adapter (Codex, WorkBuddy, ...): discovery, capture, snapshot
// reading, installation and run-state detection.
//
// Items marked "P0" are fixed in shape here but their behaviour is decided by
// the P0 experiments (E1a-E6); the interfaces do not change when they do.
package agent

import (
	"context"
	"io"

	"session-memory/internal/session"
)

// ---- discovery ----

// NativeRef locates one native session on this device. Nothing in it is ever
// synced.
type NativeRef struct {
	Home     string `json:"home"`      // agent home (one CODEX_HOME, ~/.workbuddy, ...)
	NativeID string `json:"native_id"` // thread id / session id
}

// NativeSession is one discovered session for `discover` / `list`.
type NativeSession struct {
	Ref       NativeRef            `json:"ref"`
	Profile   session.AgentProfile `json:"profile"`
	Cwd       string               `json:"cwd"`
	Title     string               `json:"title,omitempty"` // device-local display text; printed by the CLI to the user's own terminal, never logged, never synced
	UpdatedAt string               `json:"updated_at"`      // session.TimeLayout
	// Parent is the parent native session id for sub-agent threads, else "".
	Parent string `json:"parent,omitempty"`
	// Files are the files on disk, for display.
	Files []FileStat `json:"files"`
}

// FileStat is a file's identity on disk.
type FileStat struct {
	Path    string `json:"path"` // relative to Home, slash-separated, as stored on disk (may end in .zst)
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
}

// DiscoverOptions narrows discovery. The zero value lists everything.
type DiscoverOptions struct {
	NativeIDs []string // only these sessions
	Homes     []string // override the configured homes (tests)
}

// ProbeResult is the safe output of `agent probe`: no titles, no cwd, no text.
type ProbeResult struct {
	SchemaVersion int            `json:"schema_version"`
	Agent         string         `json:"agent"`
	Homes         []HomeProbe    `json:"homes"`
	Compatibility string         `json:"compatibility"` // ok | partial | unsupported
	Notes         []string       `json:"notes,omitempty"`
	Counts        map[string]int `json:"counts"`
}

// HomeProbe describes one agent home.
type HomeProbe struct {
	Exists       bool            `json:"exists"`
	ClientVer    string          `json:"client_version,omitempty"`
	Stores       map[string]bool `json:"stores"`       // named store -> present
	FileForms    map[string]int  `json:"file_forms"`   // "jsonl", "jsonl.zst", ...
	RecordTypes  map[string]int  `json:"record_types"` // histogram of record type names
	Capabilities []string        `json:"capabilities"` // the capability fingerprint (design §6.3)
}

// ---- capture ----

// Cursor is the adapter's resume state for one binding: one entry per stream
// artifact. It is persisted inside session.Binding.Artifacts.
type Cursor struct {
	Artifacts map[string]session.ArtifactState
}

// CaptureState is the outcome of one Capture call.
type CaptureState int

const (
	CaptureOK        CaptureState = iota // cursor may be committed
	CaptureUnsafe                        // corrupt record: cursor stays before it
	CaptureReplaced                      // prefix mismatch: a new generation is needed
	CaptureTruncated                     // file shorter than the cursor
	CaptureDetached                      // native files gone or moved and not found again
)

// ArtifactKey names the artifact a segment belongs to.
type ArtifactKey struct {
	Path       string // logical path (session.Artifact.Path)
	Kind       string // session.KindStream | session.KindBlob
	Form       string // session.FormPlain | session.FormZstd (streams)
	Generation int
	Mode       uint32
}

// SegmentSink receives captured bytes. The store implements it: it hashes the
// plaintext, seals it and returns the reference. Segments of one artifact are
// delivered in order and are contiguous.
type SegmentSink interface {
	// PutSegment stores bytes [start, end) of a stream artifact. The reader
	// yields exactly end-start bytes.
	PutSegment(ctx context.Context, k ArtifactKey, start, end int64, r io.Reader) (session.SegmentRef, error)
	// PutBlob stores an immutable file, chunked by the sink at MaxSegmentBytes.
	PutBlob(ctx context.Context, k ArtifactKey, size int64, r io.Reader) ([]session.SegmentRef, error)
}

// MaxSegmentBytes is the default segment size cap (design §8.2). A single
// record larger than this occupies a segment of its own.
const MaxSegmentBytes = 8 << 20

// CaptureResult is what Capture returns after streaming segments to the sink.
type CaptureResult struct {
	State  CaptureState
	Cursor Cursor // meaningful for CaptureOK only
	// Changed reports whether any new bytes were captured.
	Changed bool
	// Diags are counts and offsets only, never record text.
	Diags []Diag
}

// Diag is one non-sensitive diagnostic.
type Diag struct {
	Code   string // e.g. "unknown_record_type"
	Detail string // type name, artifact path, never content
	Count  int
	Offset int64
}

// SnapshotReader gives an adapter read access to one revision's files.
type SnapshotReader interface {
	Artifacts() []session.Artifact
	// Open returns the artifact's plaintext, segments concatenated in order.
	Open(ctx context.Context, path string) (io.ReadCloser, error)
}

// ---- installation (Writer) ----

// Mode selects what a Writer does with a revision.
type Mode int

const (
	ModeNative  Mode = iota + 1 // install the origin agent's own snapshot
	ModeConvert                 // convert a foreign snapshot into this agent's form
)

// Route is the write route of design §6.2.
type Route int

const (
	RouteOfficial Route = 1 // vendor import/resume interface
	RouteFiles    Route = 2 // session files only; the client registers them itself
	RouteRegistry Route = 3 // minimal row-level registry write (off by default)
)

// RunState is the client run-state; Unknown is treated as running.
type RunState int

const (
	NotRunning RunState = iota
	Running
	Unknown
)

// RunDetail refines RunState for display and for the support matrix.
type RunDetail string

const (
	DetailWindowOpen     RunDetail = "window_open"
	DetailProcessAlive   RunDetail = "process_alive"
	DetailBackgroundOnly RunDetail = "background_only"
	DetailNotRunning     RunDetail = "not_running"
	DetailUnknown        RunDetail = "unknown"
)

// RuntimeDetector reports whether the client could be writing the target.
// When thread-level information exists (Codex thread-writer-locks) the
// adapter passes the native id; otherwise it is "".
type RuntimeDetector interface {
	Running(ctx context.Context, home, nativeID string) (RunState, RunDetail, error)
}

// PathRule rewrites a path prefix (company /work/app -> home D:\code\app).
type PathRule struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// PathMap holds the rules of one project mapping. Apply uses the longest
// matching prefix on path-component boundaries; a path matching no rule is
// returned unchanged with ok=false.
type PathMap struct{ Rules []PathRule }

// TargetState is a read-only snapshot of the target taken by the caller.
type TargetState struct {
	Home          string
	ClientVersion string
	Run           RunState
	RunDetail     RunDetail
	// Existing lists the target files the plan may touch.
	Existing map[string]FileStat
	// BaselineHash is the hex SHA-256 over the canonical JSON of Existing plus
	// the registry rows the plan may touch; Apply re-computes it.
	BaselineHash string
}

// WriteInput is everything a Writer needs; it never reads the sync store.
type WriteInput struct {
	SessionID  string
	RevisionID string
	Mode       Mode
	Source     SnapshotReader
	// SourceProfile is the producer's profile (differs from Target for conversion).
	SourceProfile session.AgentProfile
	PathMap       PathMap
	Target        session.AgentProfile
	TargetState   TargetState
	// BindingID is the active binding being updated, "" for a first install.
	BindingID string
	// Cwd is the local project directory the session is bound to.
	Cwd string
}

// PlannedWrite is one file operation of a plan.
type PlannedWrite struct {
	Op     string `json:"op"`   // create | replace | append | row_insert | row_update
	Path   string `json:"path"` // relative to Home, or "<table>#<key>" for rows
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Plan is the Writer's pure description of what Apply will do.
type Plan struct {
	SchemaVersion int             `json:"schema_version"`
	SessionID     string          `json:"session_id"`
	RevisionID    string          `json:"revision_id"`
	Mode          Mode            `json:"mode"`
	Route         Route           `json:"route"`
	Writes        []PlannedWrite  `json:"writes"`
	BaselineHash  string          `json:"baseline_hash"`
	Warnings      []string        `json:"warnings,omitempty"` // codes only
	Report        *FidelityReport `json:"report,omitempty"`   // conversion only
	// Digest is the hex SHA-256 of the canonical JSON of this Plan with Digest
	// empty; Apply refuses a plan whose digest does not match.
	Digest string `json:"digest"`
}

// KindCount is one row of the fidelity report.
type KindCount struct {
	Kept     int `json:"kept"`
	Rendered int `json:"rendered"` // converted to text
	Dropped  int `json:"dropped"`
}

// FidelityReport is the structured conversion report (design §7.2). It holds
// counts and type names only, never text.
type FidelityReport struct {
	SchemaVersion int                  `json:"schema_version"`
	Counts        map[string]KindCount `json:"counts"` // canonical kind -> counts
	VisibleItems  int                  `json:"visible_items"`
	TokenEstimate int                  `json:"token_estimate"`
	ContextNotes  []string             `json:"context_notes,omitempty"`
	EnvGaps       []string             `json:"env_gaps,omitempty"`
	DataEgress    bool                 `json:"data_egress"`
	Route         Route                `json:"route"`
	Dropped       map[string]int       `json:"dropped_types,omitempty"` // record type name -> count
}

// Receipt is returned by Apply and verified by Verify.
type Receipt struct {
	SessionID       string                 `json:"session_id"`
	RevisionID      string                 `json:"revision_id"`
	NativeSessionID string                 `json:"native_session_id"`
	InstallMode     string                 `json:"install_mode"`
	Written         []PlannedWrite         `json:"written"`
	Import          *session.ImportReceipt `json:"import,omitempty"`
	At              string                 `json:"at"`
}

// Tx is the slice of the store's transaction a Writer may use.
type Tx interface {
	// BackupDir returns the directory for this transaction's backups of vendor
	// files; the Writer copies every file it will replace or append to into it
	// BEFORE writing and records it with Backed.
	BackupDir() string
	Backed(relPath string) error
	// Wrote records a completed write so recovery can undo it.
	Wrote(w PlannedWrite) error
}

// Writer installs a revision into the agent's native store.
type Writer interface {
	// Plan has no side effects and does not take locks.
	Plan(ctx context.Context, in WriteInput) (Plan, error)
	// Apply executes the plan: backup, write, using tx for the journal. It
	// re-checks the run state after writing and returns errcode.ClientRunning
	// (after rolling back) when the client started meanwhile.
	Apply(ctx context.Context, in WriteInput, p Plan, tx Tx) (Receipt, error)
	// Verify re-reads the written files (and rows) and checks them against r.
	Verify(ctx context.Context, in WriteInput, r Receipt) error
	// Rollback restores everything recorded by tx.Backed / tx.Wrote.
	Rollback(ctx context.Context, home string, tx Tx) error
}

// ---- the adapter ----

// Adapter is one supported agent.
type Adapter interface {
	Name() string // "codex", "workbuddy"
	Probe(ctx context.Context) (ProbeResult, error)
	Discover(ctx context.Context, o DiscoverOptions) ([]NativeSession, error)
	// Capture streams new bytes of the session's artifacts to sink, starting
	// from cur, and returns the new cursor. It never writes to the native store.
	Capture(ctx context.Context, ref NativeRef, cur Cursor, sink SegmentSink) (CaptureResult, error)
	// Locate finds a session's files again by native id (after a move,
	// archive or compression). found=false means the files are gone.
	Locate(ctx context.Context, ref NativeRef) (NativeSession, bool, error)
	Runtime() RuntimeDetector
	Writer() Writer
}
```

约定：

- `Adapter` 实例无状态；所有位置信息通过 `NativeRef.Home` 与参数传入；
- `Capture` **从不写**厂商目录；`Discover`、`Probe`、`Locate` 同样只读；
- `Cursor` 是 `session.Binding.Artifacts` 的运行时视图：`Capture` 读取传入的 `Cursor`，返回新的 `Cursor`，**由调用方（store 事务）负责持久化**；
- `SegmentSink` 由 store 实现：它对收到的明文计算 `raw_hash`、封装、写 staging，并返回 `SegmentRef`；适配器**不接触**密钥和对象库；
- `Diag` 只含类型名、计数、偏移，**不含记录内容**。

### 8.2 通用采集流程（所有 stream 型 Artifact）

`Capture` 对每个 Artifact 做：

```text
1. 定位当前文件：Locate（按原生 ID，不按保存的路径）；找不到 → 整个会话 CaptureDetached
2. 选定存储形式并打开 jsonl.Source（plain 或 zstd）
3. 校验已采集前缀：
     plain：jsonl.Verify(path, cursor)  → Truncated / Replaced / OK
     zstd ：必须从头解压，逐 Segment 区间哈希并与 raw_hash 比对（边解压边校验）；任一不符 → Replaced
   Replaced/Truncated：Generation+1，从 0 重新采集，结果 State=CaptureReplaced，调用方创建带
   replaces_revision_id 的 revision
4. 从 cursor.Offset 读 jsonl.Reader（RequireJSON=true，MaxRecordBytes=适配器配置）
5. 累积记录到 Segment：当累积字节 ≥ MaxSegmentBytes 且刚好在记录边界时切出一个 Segment（PutSegment）；
   单条记录 ≥ MaxSegmentBytes 时独占一个 Segment
6. 读到 io.EOF：尾部不完整的行不消费（Pending 字节只记入 Diag）；把剩余累积字节切成最后一个 Segment
   InvalidRecordError / RecordTooLargeError：State=CaptureUnsafe，游标停在该记录之前，
   已切出的 Segment 仍然有效（调用方提交这部分进度并设置 Binding.Unsafe）
7. 新 Cursor = jsonl.NewCursor(文件, generation, 最后一个 Segment 的 end)（zstd 的 FileID/HeadHash/TailHash
   留空，校验靠全前缀）
```

**`.jsonl` 与 `.jsonl.zst` 互转不是 Replaced**：Artifact 的 `path` 是逻辑名，offset/哈希基于解压流；同一条流压缩或解压，前缀字节不变，校验通过，不新增 generation。

**孪生文件**（同一逻辑名同时存在 `.jsonl` 与 `.jsonl.zst`）：解压 `.zst` 后与 `.jsonl` 比较，较短者必须是较长者的前缀：是 → 取较长者为当前流；否 → `CaptureUnsafe`，Diag `twin_diverged`，不采集该 Artifact。

### 8.3 Codex 适配器

**目录**：`config.CodexHomes()`（§11）；每个 home 独立发现，`NativeRef.Home` 区分。一个 ChatGPT 账号对应一个 `CODEX_HOME`（据 magpie，设计 §3.3），不同账号的同一线程 ID 不会冲突（UUID）。

**文件名**（Codex 自己的规则，据 magpie 的 `rollout_file_name.rs` 引用）：

```text
rollout-<YYYY-MM-DDTHH-MM-SS>-<线程 UUID>[_<分段 UUID>].jsonl[.zst]
```

正则：`^rollout-\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})(?:_([0-9a-fA-F-]{36}))?\.jsonl(?:\.zst)?$`。线程 UUID 统一转小写作为 `NativeID`；同一线程可以有多个文件（含 `_<分段>`），每个文件是一个 Artifact。

**发现（`Discover`）**：

1. 对每个 home：扫描 `sessions/YYYY/MM/DD/`（三级纯数字目录）和 `archived_sessions/`（递归，深度 ≤ 6），只接受匹配上面正则的文件；
2. 按线程 UUID 分组；同一逻辑名的 `.jsonl` 与 `.jsonl.zst` 归为同一 Artifact 的两种形式（孪生）；
3. 读每个线程**最早的**文件的第一条记录（`session_meta`，经 `jsonl.Source.OpenAt(0)`，`.zst` 只解压第一个块）：取 `cwd`、`originator`、`source`、`cli_version`、`model_provider`、`timestamp`；`Profile.Agent="codex"`，`Surface`：`source` 为 `vscode` 或 `originator` 含 `Desktop` → `desktop`；`cli` → `cli`；`exec` → `exec`；否则原样；`Version=cli_version`；`Format="rollout-jsonl"`；
4. `Title` 取自 `session_index.jsonl` 的 `thread_name`（可为空；设备本地展示文本，绝不写日志）；`Parent` 取自 `state_5.sqlite` 的 `thread_spawn_edges`（只读 `immutable=1` 打开），读不到则为空并记 Diag `parent_unknown`；
5. **不再按“最近 N 小时”或来源（cli/exec）过滤**：是否纳管由用户选择和支持矩阵决定。`Discover` 默认不读任何记录正文。

**Probe 字段**（`HomeProbe`，全部只来自目录结构与记录类型名）：

| 字段 | 取法 |
|---|---|
| `exists` | home 目录存在 |
| `client_version` | 最新一个 rollout 的 `session_meta.payload.cli_version` |
| `stores` | 键：`sessions_dir`、`archived_sessions_dir`、`state_db`（`state_5.sqlite`）、`history_db`（`thread_history_1.sqlite`）、`session_index`、`writer_locks_dir`（`thread-writer-locks`）、`global_state`（`.codex-global-state.json`）；值＝是否存在 |
| `file_forms` | 键 `jsonl`、`jsonl.zst`、`twin`（两种形式并存）；值＝文件数 |
| `record_types` | 最新 ≤ 20 个 rollout 的前 200 条记录里 `type` 与 `payload.type` 的直方图（只计数） |
| `capabilities` | 排序后的指纹：`form:zst`、`dir:thread-writer-locks`、`record:item_completed`、`record:compacted`、`has:ordinal` 等 |

`Compatibility`：`ok`＝`sessions_dir` 存在且至少一个 `session_meta` 可解析；`partial`＝部分不可解析；`unsupported`＝其余。`Notes` 只放固定文案。

**运行检测（默认实现，E2a 前保持保守）**：

1. `thread-writer-locks/` 不存在 → `Unknown`；
2. 打开 `thread-writer-locks/<线程 ID>.lock`（只读）并 `TryLock`：取不到 → `Running`，`DetailWindowOpen`；
3. 取到（随即释放）→ 再看是否有该 home 的 Codex 进程（进程身份按应用包路径或 `--user-data-dir` 判定，不按进程名；`platform` 需新增枚举函数，M5）：有或无法判断 → `Unknown`；确无 → `NotRunning`；
4. E2a 若证明“线程锁未被持有”已足以保证无人写该线程且无过期界面，则放宽第 3 步。

**Writer（待 E1a/E1b/E4）**：行为边界已固定：

- `RouteFiles`（只写会话文件）是默认路线；`RouteRegistry` 默认关闭，仅在配置显式启用且版本在白名单内时可用；
- 新线程：目标文件名按 `rollout-<本地时间>-<uuid v7>.jsonl` 生成（新 ID 时）或沿用源文件名（同 ID 恢复）；放在 `sessions/YYYY/MM/DD/`；格式始终是未压缩 `.jsonl`，**绝不写 `.zst`**；
- 路径变换只作用于已知的结构化字段：`session_meta.payload.cwd`、`turn_context.payload.cwd`（其余字段待 E1b/E1c 逐项确认）。变换必须在**原始字节**层面替换该 JSON 字符串的值，不重新序列化整行；若新旧路径字节长度不同，且目标线程已被 Codex 投影过，则该会话不得原地应用（设计 §6.3）；
- 目标线程已存在且只有 `.zst` 形式时：返回 `needs_action`，不处理，直到 E1b 给出结论；
- 账号、权限、模型类字段取自目标配置，不取自归档（E1c 之前以“不覆盖”为准：Writer 不修改这些字段，也不声称覆盖了它们）。

### 8.4 WorkBuddy 适配器（读取侧已定，写入侧待 E3/E4）

**目录**：`config.WorkBuddyHome()`（环境变量 `WORKBUDDY_CONFIG_DIR`，否则 `~/.workbuddy`）。CodeBuddy Code 的目录（`CODEBUDDY_CONFIG_DIR` 或 `~/.codebuddy`）格式相同；两者指向同一目录时只读一次（按目录身份 `os.SameFile` 判定）。

**Artifact**（相对 home 的逻辑路径）：

| 路径 | kind | 说明 |
|---|---|---|
| `projects/<编码目录>/<会话 ID>.jsonl` | stream | 主会话，条目为 OpenAI Agents SDK 风格 |
| `projects/<编码目录>/<会话 ID>.meta.json` | blob | 仅部分会话有；提供 cwd |
| `projects/<编码目录>/<会话 ID>/subagents/*.jsonl` | stream | 子代理会话，键归属父会话 |
| `projects/<编码目录>/<会话 ID>/tool-results/*` | blob | 溢出的工具输出 |
| `projects/<编码目录>/<会话 ID>.file-rollback.ndjson` | stream | 回滚记录 |
| `file-history/<会话 ID>/` | — | **默认不纳入**，列为依赖缺口，由 E3 决定 |

- `NativeID` = 文件名里的会话 ID；`cwd` 取自记录的 `cwd` 字段，其次取自 `.meta.json`，**不得从目录名反推**（编码有损）；
- `Title`：`custom-title` 优先于 `ai-title`；
- 非人类输入：user 消息内容以 `<` 开头，或匹配 `^@[\w-]+:\S*$`（起点启发式，M2 用真实样本确认）；
- 软删除：`workbuddy.db` 的 `sessions.deleted_at` 非空的会话不当作活跃会话。读取该库一律用 URI `file:<path>?mode=ro&immutable=1`；读不到时不排除任何会话并在 Diag 记 `registry_unreadable`；
- 子代理文件的键归属父会话（`NativeID` 取其上级会话目录名）；
- 采集算法同 §8.2；
- **Writer**：行为待 E3（是否需要登记行、行的最小内容、重启后是否保持）。在 E3 之前 `Writer()` 返回 `errcode.Unsupported`。

## 9. Canonical 与转换

### 9.1 类型

下面是 `internal/canonical` 的类型与 `Validate`。Canonical 由适配器在**使用端**从 Native Snapshot 提取，缓存在 `store/cache/`（键 `(revision_id, adapter_version)`），**从不同步**。

```go
// internal/canonical/canonical.go
// Package canonical defines the neutral item model used to convert a session
// between agents (design §5.4). It is derived on the consuming device by an
// adapter's Extract and never synced.
package canonical

import "time"

// Kind classifies an item.
type Kind string

const (
	KindMessage       Kind = "message"
	KindReasoning     Kind = "reasoning"
	KindToolCall      Kind = "tool_call"
	KindToolResult    Kind = "tool_result"
	KindCompaction    Kind = "compaction"
	KindAttachmentRef Kind = "attachment_ref"
	KindSessionMeta   Kind = "session_meta"
	KindUnknown       Kind = "unknown"
)

// Role of a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
)

// RawRef points back at the origin record inside the native snapshot.
type RawRef struct {
	Artifact string `json:"artifact"` // session.Artifact.Path
	Offset   int64  `json:"offset"`   // byte offset of the record in the decompressed stream
	Length   int64  `json:"length"`
	Hash     string `json:"hash"` // hex SHA-256 of the record bytes
}

// Item is one conversation element.
type Item struct {
	// ID is the stable record key: the origin record's own id or ordinal
	// (Codex: "o<ordinal>"), identical across adapter versions. Only when the
	// origin has neither is it "g<generation>:<offset>:<hash12>".
	ID       string         `json:"id"`
	Kind     Kind           `json:"kind"`
	Role     Role           `json:"role,omitempty"` // messages
	ParentID string         `json:"parent_id,omitempty"`
	CallID   string         `json:"call_id,omitempty"` // pairs tool_call with tool_result
	Time     time.Time      `json:"time"`
	Human    bool           `json:"human,omitempty"` // user message typed by a person (adapter predicate)
	Norm     map[string]any `json:"norm,omitempty"`  // text, tool name, arguments, status, ...
	Raw      RawRef         `json:"raw"`
	Links    []RawRef       `json:"links,omitempty"`  // the other layer's records for the same event
	Branch   string         `json:"branch,omitempty"` // "" = active path; else the branch tag
}

// Issue is a validation finding; Validate never drops items.
type Issue struct {
	ItemID string
	Code   string // "orphan_call", "orphan_result", "out_of_order", "missing_field"
}

// Validate checks ordering and call/result pairing.
func Validate(items []Item) []Issue {
	var out []Issue
	calls := map[string]bool{}
	results := map[string]bool{}
	var last int64 = -1
	for _, it := range items {
		if it.Raw.Offset < last && it.Raw.Artifact != "" {
			out = append(out, Issue{it.ID, "out_of_order"})
		}
		if it.Raw.Offset > last {
			last = it.Raw.Offset
		}
		if it.ID == "" || it.Kind == "" {
			out = append(out, Issue{it.ID, "missing_field"})
		}
		switch it.Kind {
		case KindToolCall:
			calls[it.CallID] = true
		case KindToolResult:
			results[it.CallID] = true
		}
	}
	for _, it := range items {
		switch it.Kind {
		case KindToolCall:
			if it.CallID == "" || !results[it.CallID] {
				out = append(out, Issue{it.ID, "orphan_call"})
			}
		case KindToolResult:
			if it.CallID == "" || !calls[it.CallID] {
				out = append(out, Issue{it.ID, "orphan_result"})
			}
		}
	}
	return out
}
```

`Validate` 不丢弃条目，只返回问题；采集与转换前后各调用一次，`orphan_call`/`orphan_result` 在转换时按 §9.2 处理。

### 9.2 转换（`internal/convert`）

`convert` 只做规则，不认识任何文件格式；每个目标 Agent 的 Writer 把 `OutRecord` 序列化成原生记录。

```go
// internal/convert/convert.go
// Package convert turns canonical items into a target-neutral list of output
// records plus a fidelity report. It knows rules, not file formats: each
// target adapter's Writer serialises OutRecord into its native records.
// convert never imports source/*.
package convert

import (
	"encoding/json"
	"time"

	"session-memory/internal/agent"
	"session-memory/internal/canonical"
)

// Default limits (design §7.2; the 64 KiB cap follows magpie's convention).
const (
	DefaultToolOutputCap = 64 << 10
	TruncationMark       = "\n[…]"
)

// OutKind classifies an output record.
type OutKind string

const (
	OutUser       OutKind = "user"
	OutAssistant  OutKind = "assistant"
	OutToolCall   OutKind = "tool_call" // a call the target has a native tool for
	OutToolResult OutKind = "tool_result"
	OutSummary    OutKind = "summary" // a compaction rendered as a message
)

// OutRecord is one record to write, in order.
type OutRecord struct {
	Kind   OutKind
	Time   time.Time
	Text   string           // messages, summaries; rendered tool notes
	Images []string         // data: URLs of user images
	Tool   *OutTool         // tool_call / tool_result
	Source canonical.RawRef // origin record, for the import receipt and traceability
	Note   string           // "" or a rendering tag: "tool_as_text", "truncated", "compaction"
}

// OutTool is a native tool call or its result.
type OutTool struct {
	Name    string          // target's tool name (e.g. "exec_command")
	CallID  string          // new id, unique within the output
	Args    json.RawMessage // call arguments in the target's shape
	Output  string          // result text
	IsError bool
}

// Options tune a conversion.
type Options struct {
	ToolOutputCap int // bytes; <=0 means DefaultToolOutputCap
	// ToolMap maps an origin tool name to the target's native tool name. A call
	// whose name is absent is rendered as text.
	ToolMap map[string]string
	// IncludeSubagents is decided by E1c; false until then.
	IncludeSubagents bool
}

// Output is the result of Convert.
type Output struct {
	Records []OutRecord
	Report  agent.FidelityReport
}

// Convert applies the rules of design §7.2:
//
//   - message user/assistant -> OutUser/OutAssistant (only the active path;
//     other branches are counted as dropped);
//   - system/developer messages and reasoning -> dropped;
//   - a tool_call+tool_result pair with a ToolMap entry -> OutToolCall +
//     OutToolResult (shell commands included); otherwise one OutAssistant text
//     "[<name>]" + fenced input + "[result]"/"[error]" + fenced output;
//   - an orphan call or result is never written as a native record: it is
//     rendered as text;
//   - tool output longer than ToolOutputCap is cut at a UTF-8 boundary and
//     TruncationMark appended;
//   - compaction -> OutSummary (origin summary text only; encrypted
//     compaction items are dropped);
//   - session_meta / unknown -> dropped, counted by record type name;
//   - Report.Counts must equal what was actually emitted (a test asserts it).
//
// Convert is deterministic: same input, same output, same report.
func Convert(items []canonical.Item, opt Options) (Output, error) {
	return Output{}, errNotImplemented
}

type constError string

func (e constError) Error() string { return string(e) }

const errNotImplemented = constError("convert: not implemented")
```

规则总表（设计 §7.2 的实现细则；**黑体为本文新增的确定值**）：

| 输入 | 处理 |
|---|---|
| `message` user/assistant，在活动路径上 | 输出 `OutUser`/`OutAssistant`，保序保时间；user 消息是否算“人类输入”由 `Item.Human` 决定 |
| `message` system/developer | 丢弃，计入 `dropped` |
| `message` 在非活动分支（`Branch != ""`） | 丢弃，计入 `dropped`，报告里注明分支数 |
| `reasoning` | 丢弃 |
| 成对的 `tool_call`+`tool_result`，`ToolMap` 有映射 | `OutToolCall`+`OutToolResult`，`CallID` 重新生成（输出内唯一） |
| 成对的调用，`ToolMap` 无映射 | 一条 `OutAssistant`：`[工具名]` + 围栏（输入）+ `[result]`/`[error]` + 围栏（输出） |
| 孤立的 `tool_call` 或 `tool_result` | **绝不写成原生记录**，渲染为文本并计入 `rendered` |
| 工具输出 | 超过 `ToolOutputCap`（**默认 64 KiB**）时在 UTF-8 边界截断并追加 `TruncationMark`，`Note="truncated"` |
| 围栏 | 取不被内容包含的最短反引号串（≥3 个）：内容含 ``` 时用 ````，依此类推 |
| `compaction` | 只有来源自带可读摘要文本时输出 `OutSummary`；加密内容（如 Codex 的 `encrypted_content`）丢弃 |
| `attachment_ref` | 能传输的图片作为 `Images`（data URL）；否则丢弃并计入报告的缺口 |
| `session_meta`、`unknown` | 丢弃，按记录类型名计入 `Dropped` |
| 同一事件的两层记录（`Links` 非空） | 只输出主流那一条（Convert 的输入已是主流） |

**确定性与一致性**：`Convert` 是确定性函数；`Report.Counts` 必须与实际输出的记录数完全一致（单元测试逐类型断言）；报告不含任何文本。

## 10. 冲突与落地

参考实现：`internal/session/graph.go`，测试 `session_test.go`（覆盖设计 A5、A6、A13）。

### 10.1 图

`NewGraph(revisions)`：只有**父链完整**的 revision 才进入图；父版本尚未到达的 revision 放入 `Pending`，不参与 heads 和祖先判断。`Heads()` = 没有子版本的 revision。`IsAncestorOrEqual(a, b)`：沿 `b` 的父链向上走，遇到 `a` 即为真（`a == b` 为真）。

### 10.2 `Resolve`

```text
heads = Heads()
len(heads)==0            → 无
len(heads)==1            → Effective = heads[0]，不冲突
否则：
  usable = 所有“涉及的 revision 全部在图里”的 choice（引用未知 revision 的 choice 暂时忽略）
  usable 为空            → 冲突
  deepest = usable 中 Chosen 沿同一条祖先链最深的那个；若存在两个 Chosen 不在同一条祖先链上 → 冲突（不一致的选择）
  retired = 所有 usable 的 (SeenHeads − {Chosen}) 的并集
  live    = heads − retired
  len(live)==1 且 deepest 是 live[0] 的祖先或等于它 → Effective = live[0]，不冲突
  否则 → 冲突
```

由此得到的行为（测试逐条覆盖）：

| 场景 | 结果 |
|---|---|
| 两设备都从 R2 续写得 A、B，没有选择 | 冲突（A5） |
| 选择 A（seen={A,B}） | 解决，Effective=A，B 被退役 |
| 选中的分支继续 A→A2 | 仍解决，Effective=A2 |
| 被放弃的 B 继续得到 B2 | **重新冲突**（A6），因为 B2 不被任何已有选择覆盖 |
| 对 {A,B2} 再选 A | 解决 |
| 两台设备分别选 A 与 B | 冲突（选择不一致） |
| choice 引用本机还没收到的 revision | 忽略该 choice，仍冲突 |
| 选中的分支自己又分叉成 A3、A4 | 冲突 |

### 10.3 落地（`LandingFor`）

| installed 与 effective 的关系 | 结果 | 动作 |
|---|---|---|
| 相等 | `LandUpToDate` | 无 |
| installed 为空（首次安装） | `LandFastForward` | 首次安装（不是落地） |
| installed 是 effective 的祖先 | `LandFastForward` | 满足设计 §9.3 的全部条件时原生更新 |
| 其他（已装的是被放弃的分支或未知 revision） | `LandNeedsAction` | **不自动执行**；`status` 显示“需要处理”；用户确认后走“备份 + 原位替换为选定 head（同一原生 ID，要求本地分支已完整采集为 revision）”，journal 保护，失败回滚；E1b 证明不可行时改为新建原生会话（设计 §9.2 第 6 点，已定） |

冲突期间（`Resolve.Conflict`）**禁止**：自动安装、自动转换、自动选择“最新”。

## 11. 配置变更

`internal/config` 已在 M0 重写。为支持多个 Codex 账号目录，需做下列变更（M2 随发现逻辑一起做）：

1. `AgentConfig` 增加两个字段：

```go
// Homes are additional agent homes (one per account for Codex), expanded and
// validated like Home. The effective list is Home followed by Homes,
// duplicates (same directory, by os.SameFile) removed, order preserved.
Homes []string `json:"homes,omitempty"`
// DiscoverHomes makes the adapter also look for sibling homes: directories in
// the user's home whose name starts with ".codex" or "codex" and that contain
// a "sessions" directory. Only directory names and the existence of
// "sessions" are examined (never auth.json). Off by default.
DiscoverHomes *bool `json:"discover_homes,omitempty"`
```

2. 可设置的键新增：`agents.codex.homes`、`agents.claude.homes`、`agents.workbuddy.homes`（逗号分隔的路径列表，`kindList`）和 `agents.<name>.discover_homes`（布尔）；
3. 新增方法 `func (c Config) CodexHomes() []string`（及 `WorkBuddyHomes()`）：
   - 第一个元素是 `CodexHome()`；`CodexHome()` 增加环境变量规则（与现有 `ClaudeHome()` 对称）：配置里的 `home` 是默认值 `~/.codex` 且环境变量 `CODEX_HOME` 非空时用环境变量；显式配置优先；
   - `WorkBuddyHome()` 同理使用 `WORKBUDDY_CONFIG_DIR`；
   - 随后是 `Homes`（逐个 `ExpandPath` 并要求绝对路径，校验同 `home`）；
   - `DiscoverHomes` 为真时追加发现的目录；
   - 去重按目录身份（`os.SameFile`），不存在的目录保留在列表里（`Probe` 会报告 `exists:false`）。
4. 校验：`homes` 的每一项必须是绝对路径（展开 `~` 后）；`homes` 最多 16 项；
5. 测试：环境变量优先级、重复目录去重、符号链接指向同一目录只算一次、`discover_homes` 只看目录名且不读 `auth.json`（用权限为 000 的 `auth.json` 构造目录：发现仍然成功，即说明没有打开它）。

## 12. 命令行

CLI 代码在 `cmd/session-memory/`，**只做参数解析与输出**，业务在 `internal` 各包。结构：

```text
cmd/session-memory/
  main.go          func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
  app.go           type App struct { Cfg config.Config; Out, Err io.Writer; JSON bool; Now func() time.Time; Log *slog.Logger }
  root.go          func newRoot(a *App) *cobra.Command   // 全局 flag、子命令注册
  cmd_version.go   cmd_agent.go   cmd_config.go   ...    // 每个命令一个文件，只调用包函数
  dev_*.go         //go:build dev    P0 工具，不进入发布构建（§15）
```

`run` 构造 `App`、执行根命令并把返回的 `error` 映射为退出码；测试直接调用 `run(args, nil, &out, &errBuf)` 断言输出与退出码，不启动子进程。

### 12.1 全局规则

- 全局 flag：`--home DIR`（等价于 `SM_HOME`，优先于环境变量）、`--json`、`-v/--verbose`、`--no-color`；
- **stdout** 是命令的结果：默认是给人看的文本；`--json` 时**恰好一个 JSON 文档**（末尾换行），不混入日志；**stderr** 是日志与提示（`-v` 时为 JSON 行）；
- 失败时：文本模式把 `error: <code>: <message>` 和提示写到 stderr；`--json` 模式把 `{"error":{"code":"…","message":"…","details":{…}}}` 写到 **stdout**，让脚本只读一个流；两者都以 `errcode.ExitCode(code)` 退出；
- `details` 只含 ID、计数、偏移、哈希，**不含对话文本、标题或用户项目路径**；
- 需要写本地状态的命令先取 `locks/store.lock`（§3.2）；
- 任何会写厂商目录的命令都有 `--plan`（只展示，不执行），且 `--plan` 的输出与随后真正执行时使用的 plan **摘要相同**（`Plan.Digest`）。

退出码（完整定义见 `internal/errcode`，下表是命令行为）：

| 退出码 | 含义 |
|---|---|
| 0 | 成功 |
| 1 | 一般错误（`internal` 及未列出的码） |
| 2 | 用法错误（`usage`） |
| 3 | 存在冲突或需要用户处理（`conflict`、`needs_action`） |
| 4 | 目标客户端运行中或无法确认（`client_running`） |
| 5 | 版本/格式不受支持（`unknown_format`、`unsupported`） |
| 6 | 本地状态需要恢复（`recovery_needed`、`device_id_conflict`） |
| 7 | 被安全策略阻止（`secret_detected`、`fidelity_blocked`） |
| 8 | 找不到对象（`not_tracked`、`not_found`） |
| 9 | 没有 vault 或认证失败（`no_vault`、`auth_failed`） |

错误码与退出码的完整代码：

```go
// internal/errcode/errcode.go
// Package errcode defines the stable error codes shown to users and scripts
// (CLI --json) and their process exit codes.
package errcode

import (
	"errors"
	"fmt"
)

// Code is a stable, machine-readable error identifier.
type Code string

const (
	UnsafeRecord     Code = "unsafe_record"      // complete but unparsable record; session marked unsafe
	FileReplaced     Code = "file_replaced"      // source truncated, replaced or rewritten
	PrefixMismatch   Code = "prefix_mismatch"    // consumed prefix no longer matches (deep check)
	UnknownFormat    Code = "unknown_format"     // client/version/format outside the compatibility matrix
	ClientRunning    Code = "client_running"     // target client running or state unknown
	BaselineChanged  Code = "baseline_changed"   // target differs from the plan's baseline
	DepsMissing      Code = "deps_missing"       // revision objects or attachments missing
	Conflict         Code = "conflict"           // more than one live head
	NeedsAction      Code = "needs_action"       // choice landing needs the user's confirmation
	SecretDetected   Code = "secret_detected"    // risk scan hit; publish/convert paused
	FidelityBlocked  Code = "fidelity_blocked"   // conversion fidelity too low; nothing written
	TargetRejected   Code = "target_rejected"    // target client rejected the output; rolled back
	RecoveryNeeded   Code = "recovery_needed"    // unfinished journal or damaged local state
	NotTracked       Code = "not_tracked"        // session is not under SessionMemory
	NotFound         Code = "not_found"          // unknown session/revision/binding id
	NoVault          Code = "no_vault"           // vault not initialised / key unavailable
	AuthFailed       Code = "auth_failed"        // object failed authentication (quarantined)
	Usage            Code = "usage"              // bad command line
	Internal         Code = "internal"           // bug or unexpected failure
	Unsupported      Code = "unsupported"        // feature not available for this agent/version
	NotIsolated      Code = "not_isolated"       // dev tool refused: target is not a marked sandbox
	DeviceIDConflict Code = "device_id_conflict" // sync folder has a newer sequence for this device id
)

// Exit codes: 0 success; each family of Code maps to one stable exit code so
// shell scripts can branch without parsing output.
var exitCodes = map[Code]int{
	Usage:            2,
	Conflict:         3,
	NeedsAction:      3,
	ClientRunning:    4,
	UnknownFormat:    5,
	Unsupported:      5,
	RecoveryNeeded:   6,
	DeviceIDConflict: 6,
	SecretDetected:   7,
	FidelityBlocked:  7,
	NotTracked:       8,
	NotFound:         8,
	NoVault:          9,
	AuthFailed:       9,
}

// ExitCode returns the process exit code for c (1 for any other code).
func ExitCode(c Code) int {
	if n, ok := exitCodes[c]; ok {
		return n
	}
	return 1
}

// Error is an error carrying a Code and non-sensitive details (IDs, counts,
// offsets, hashes; never conversation text).
type Error struct {
	Code    Code
	Message string
	Details map[string]any
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// New builds an Error.
func New(c Code, msg string) *Error { return &Error{Code: c, Message: msg} }

// Wrap builds an Error around a cause.
func Wrap(c Code, msg string, err error) *Error { return &Error{Code: c, Message: msg, Err: err} }

// With returns a copy of e with one more detail.
func (e *Error) With(key string, value any) *Error {
	d := make(map[string]any, len(e.Details)+1)
	for k, v := range e.Details {
		d[k] = v
	}
	d[key] = value
	cp := *e
	cp.Details = d
	return &cp
}

// CodeOf extracts the Code of err, or Internal.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return Internal
}
```

### 12.2 M0 要实现的命令（精确规格）

**`session-memory version`**

- 文本：`session-memory <version> (<commit>) <go> <os>/<arch>`；
- JSON：`{"version":"…","commit":"…","go":"go1.25.x","os":"darwin","arch":"arm64"}`；
- `version` 由 `-ldflags "-X main.version=…"` 注入，缺省 `dev`；`commit` 取 `runtime/debug.ReadBuildInfo` 的 `vcs.revision` 前 12 位，没有则 `unknown`。

**`session-memory agent probe <agent>`**（`<agent>` ∈ `codex`、`workbuddy`；M0 只实现 `codex`，其余返回退出码 5 `unsupported`；未知名字退出码 2）

- 输出 `agent.ProbeResult`（§8.1）；JSON 即该结构；文本是每个 home 的表：是否存在、版本、各存储是否存在、存储形式计数、能力指纹、兼容结论；
- **只读**；输出**不得**含标题、cwd、记录文本、会话 ID（只有计数）；
- 退出码：目录不存在或不兼容仍为 0（结论在输出里），仅用法/内部错误才非 0；
- 实现：`internal/source/codex.Probe` 按 §8.3 的 Probe 字段改造现有 `probe.go`（现有函数的头部注释里的旧命令名一并改掉）。

**`session-memory config show | get <key> | set <key> <value> | path`**

- `show`：`config.LoadSources()` 的结果，列 `key / value / source`（`default|global|env`）；
- `get`：打印生效值（`Load()`）；未知键退出码 2；
- `set`：`config.Set`（原子写、先校验再写）；
- `path`：打印全局配置文件路径。

### 12.3 后续命令总表（草案，在各自里程碑冻结）

JSON 输出统一为“包内类型的 JSON”，类型名在括号里；未列出的 flag 视为 `--plan`/`--json` 等全局规则。

| 命令 | 作用 | 里程碑 |
|---|---|---|
| `init` | 创建 vault 与本机 `device.json`，写 `vault.json` | M4 |
| `transport attach-folder DIR` | 记录同步目录（`transport.folder`）并检查可读写 | M4 |
| `device invite --out FILE` / `device join FILE` | 生成/使用加入包（§6.3），口令从 `--passphrase-file` 或交互读取，不接受命令行明文参数 | M4 |
| `device regenerate-id` | 生成新的 `device_id`（§7.4） | M4 |
| `discover --agent A` | 列出可纳管的原生会话（`[]agent.NativeSession`：含标题与 cwd 这类设备本地展示文本，只输出到用户自己的终端，绝不写入日志或同步；不含任何记录正文） | M2 |
| `track --agent A --native-session ID [--project REF]` | 纳管：创建 `session_id`、binding、首个 revision | M3 |
| `list [--agent A] [--project REF]` | 列出已纳管会话与状态 | M3 |
| `checkpoint SESSION_ID` | 立即采集并发布 | M3 |
| `sync [--watch] [--auto-install]` | 采集、发布、接收、（可选）自动安装 | M4/M5 |
| `status [SESSION_ID]` | 设计 §11.3 的状态（§7.5） | M3 |
| `project map REF --path DIR [--rule FROM=TO]...` | 建立/更新 `projects/<ref>.json` | M5 |
| `restore SESSION_ID --agent A --mode native [--plan]` | 原生恢复 | M3/M5 |
| `open SESSION_ID --agent A` | 先 pull、等待客户端关闭、安装、启动客户端 | M5 |
| `continue SESSION_ID --agent A --device D [--mode context]` | 发布 `convert-request` | M6 |
| `convert SESSION_ID --agent A [--plan]` | 转换写入 | M6 |
| `fidelity SESSION_ID --agent A` | 显示保真度报告（`agent.FidelityReport`） | M6 |
| `resume-pack SESSION_ID --revision R --out FILE` | 兜底的上下文包 | M7 |
| `revisions SESSION_ID` | revision 图与 heads | M5 |
| `choose SESSION_ID --revision R` | 发布 `choice` | M5 |
| `verify [SESSION_ID] [--deep] [--repair-binding]` | 校验（`--deep` 做全前缀哈希） | M3 |
| `auto-install enable|disable --agent A` | 逐 Agent 开关后台安装 | M5 |
| `mcp serve` | 兜底路径的只读 MCP | M7 |

## 13. 日志、错误与故障注入

### 13.1 日志（`internal/logx`）

`internal` 下所有日志只经 `logx`：它的属性函数只接受类型化的值（ID、计数、偏移、哈希前缀、错误码、程序自己写死的名字），**没有接受任意字符串的函数**，传入的非 ID 字符串会被替换为 `<invalid>`。

```go
// internal/logx/logx.go
// Package logx is the only way internal/ code logs. Its attribute helpers take
// typed values (IDs, counts, offsets, hashes, error codes), so conversation
// text, titles, paths of user projects and secrets cannot be passed by
// accident. A test (TestNoRawLogAttrs) fails the build if internal/ code calls
// slog's free-form String/Any attributes or fmt-formats into a message.
package logx

import (
	"io"
	"log/slog"
	"regexp"
	"time"

	"session-memory/internal/errcode"
)

var idPattern = regexp.MustCompile(`^(ss|dev|bind|tx|req|vlt)_[a-z2-7]{16,26}$|^(rev|obj)_[0-9a-f]{64}$`)

// New returns a JSON logger at the given level writing to w.
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

// ID logs a SessionMemory identifier; anything that is not one is replaced by
// "<invalid>" so a path or title passed here cannot leak. Long hash-style IDs
// are shortened to 12 hex characters after the prefix.
func ID(key, v string) slog.Attr {
	if !idPattern.MatchString(v) {
		return slog.String(key, "<invalid>")
	}
	for i := 0; i < len(v); i++ {
		if v[i] == '_' && len(v)-i-1 == 64 {
			return slog.String(key, v[:i+1+12])
		}
	}
	return slog.String(key, v)
}

func Count(key string, n int64) slog.Attr  { return slog.Int64(key, n) }
func Offset(key string, n int64) slog.Attr { return slog.Int64(key, n) }
func Dur(key string, d time.Duration) slog.Attr {
	return slog.Int64(key+"_ms", d.Milliseconds())
}

// Hash logs the first 12 hex characters of a hash.
func Hash(key, hex string) slog.Attr {
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return slog.String(key, hex)
}

// Code logs an error code.
func Code(key string, c errcode.Code) slog.Attr { return slog.String(key, string(c)) }

// Name logs one of a fixed set of identifiers chosen by the program (agent
// names, record type names, state names). Callers pass constants only; the
// lint test enforces that the argument is not a variable of type string
// derived from file or network input.
func Name(key, constant string) slog.Attr { return slog.String(key, constant) }
```

**lint 测试 `TestNoRawLogAttrs`**（`internal/logx/lint_test.go`，M0 实现）：用 `go/parser` 遍历 `internal/` 下所有非测试 `.go` 文件，若出现下列任一即失败：

1. 在 `logx` 包之外调用 `slog.String`、`slog.Any`、`slog.Group`、`slog.Attr` 的构造；
2. 调用 `log.Print*`、`log.Fatal*`、`fmt.Print*`（`cmd/` 与 `*_test.go` 除外）；
3. `logx.Name(key, x)` 的第二个参数不是字符串字面量或具名常量。

### 13.2 故障注入（`internal/failpoint`）

参考实现带构建标签：默认 `Hit` 是空操作；`-tags failpoint` 下读取 `SM_FAILPOINTS="名字=动作,…"`，动作 `crash`（`os.Exit(137)`，不跑 defer）、`error`（返回 `errcode.Internal`）、`panic`。

注入点命名规则：对 `store.Flow` 中每个 (kind, state)，在**写该 State 之前**和**之后**各一个点：`<kind>.before_<state>`、`<kind>.after_<state>`；另有 `publish.mid_upload`（上传到一半）与 `install.mid_write`（写了一部分文件）。**每个会写磁盘的事务代码必须在这些位置调用 `failpoint.Hit`**，测试矩阵才有意义（§14.4）。

## 14. 测试规格

### 14.1 向量测试

本文的所有向量（§1.4、§2.3、§2.5、§6.4）各有一个测试，随对应包一起复制到仓库：`canonjson_test.go`、`session_test.go`（`TestSpecExampleVectors`、`TestRevisionIDVector`、冲突与落地场景）、`vault_test.go`（`TestSealVector`、篡改、AAD 绑定、加入包）。这些测试是跨设备兼容的底线，任何实现改动都不得让它们失败或被修改期望值（要改必须同时改本文并说明为何破坏兼容）。

### 14.2 夹具

**合成夹具**（M2 起就能用，不依赖真实数据）：`internal/testutil/codexgen`：

```go
type Options struct {
    Seed       int64    // 决定所有“随机”内容，输出逐字节可重现
    ThreadID   string   // 空则由 Seed 派生一个 UUID v7 形式的 ID
    Cwd        string
    Turns      int
    ToolCalls  int      // 每个 turn 的工具调用数
    Compacted  bool     // 在中间插入一条 compacted 记录（带 replacement_history）
    Images     bool
    Ordinals   bool     // 信封里带连续的 "ordinal"（从 0 起）
    UILayer    bool     // 同时生成 event_msg/item_completed
    CLIVersion, Source, Originator, ModelProvider string
}
func Rollout(o Options) []byte              // 未压缩 JSONL，每行一条记录，以 \n 结尾
func Zstd(plain []byte) []byte              // 单帧 zstd，无校验和（与观察到的 Codex .zst 一致）
```

记录形状（信封 `{"timestamp","ordinal"?,"type","payload"}`）取自本机观察和现有夹具：`session_meta{session_id,id,timestamp,cwd,originator,cli_version,source,model_provider}`、`turn_context{turn_id,timestamp,model,cwd}`、`response_item{type:message|reasoning|function_call|function_call_output|custom_tool_call…}`、`event_msg{type:user_message|agent_message|item_completed|token_count…}`、`compacted{message|replacement_history}`。**`item_completed` 等较新记录的字段以 `dev inventory` 在 M1 的真实结果为准**，生成器据此修订；这不影响采集测试——采集测试断言的是“字节段拼接后等于原文件”，与记录内容无关。

必须覆盖的夹具场景（M2 的测试清单，每项一个用例）：

| 场景 | 断言 |
|---|---|
| 基本会话（plain） | 采集后 Segment 拼接 == 原文件；`segments_digest` 与手算一致 |
| 同内容的 `.zst` 形式 | 与 plain 的 Segment 的 `raw_hash` 序列完全相同；plain↔zst 互转**不**触发 Replaced |
| 超过 `MaxSegmentBytes` 的会话 | 在记录边界切分；每个 Segment ≤ 上限；单条超大记录独占一个 Segment |
| 末尾半行 | 不消费；追加完成后下一次采集读到它；游标不越过半行 |
| 损坏的完整行 | `CaptureUnsafe`；游标停在其前；之前的 Segment 有效 |
| 同长度改写（头/尾 4 KiB 内） | `Verify` 判 Replaced；generation+1 |
| 同长度改写（中部） | 快速校验发现不了（已知限制），`--deep` 发现 |
| 截断后又增长超过旧偏移 | Replaced |
| 孪生文件相等 / 分叉 | 相等→取较长者；分叉→`CaptureUnsafe`+`twin_diverged` |
| 线程有多个分段文件 | 每个文件一个 Artifact，路径不同 |
| 文件被移入 `archived_sessions/` | `Locate` 按线程 ID 找到，绑定保持 |
| 文件被删除且找不到 | `CaptureDetached` |
| 带父子线程 | `Parent` 正确；子线程作为依赖列出 |
| 未知记录类型 | 原样保留进 Segment，Diag 计数，不丢 |

**脱敏夹具**（M1 之后，来自真实会话）：`dev scrub`（§15）。算法：逐行解析 JSON，**保留所有键名和结构**；字符串值默认替换为同长度的确定性填充（`x` 重复，最长 64）；下列键的值原样保留（白名单）：`type`、`role`、`status`、`name`（工具名）、`originator`、`cli_version`、`source`、`model_provider`、`kind`、`phase`、`model`；形如 UUID 的值经一个按文件固定的映射表替换为另一个合法 UUID（相同输入相同输出，保持引用关系）；`timestamp` 类值整体平移一个固定偏移（保持顺序和间隔）；`encrypted_content` 等 base64 大字段替换为固定长度的填充；数字原样保留。**测试**：输入含哨兵串 `SECRET-SENTINEL-123` 的各种位置（键值、嵌套、数组、工具输出），输出中不得出现；行数、记录类型序列与输入一致。脱敏结果入库前必须人工复核一遍。

### 14.3 采集与转换的往返自洽

适配器的 Writer 写出的内容，经同一适配器 `Capture` 读回再 `Extract`，其 Canonical 条目与源的 Canonical 一致（排除按规则丢弃的类型）；`Report.Counts` 与实际条目数一致。

### 14.4 故障注入矩阵

用 `-tags failpoint` 构建测试二进制；每个用例：重新执行自身（`os/exec` 跑 `os.Args[0] -test.run=TestHelper...`，设置 `SM_FAILPOINTS=<点>=crash`），子进程崩溃后在**父进程**里执行 `Store.Recover`，再断言不变量。

| 事务 | 必测崩溃点 | 恢复后必须成立 |
|---|---|---|
| capture | 每个 `before_/after_` 状态点 | 游标未超出已提交 revision 的末端；不存在两个内容相同的 revision；`objects/` 下没有哈希不符的文件；staging 被清理；**再次采集后结果与不崩溃时逐字节相同** |
| publish | `publish.mid_upload`、各状态点 | 同步目录里每个序号至多一个文件；`manifest.sealed` 字节与已发布文件相同；`sequence.json` 不早于已发布序号；重试后不重复上传 |
| pull | 各状态点 | 没有半导入的 revision（要么全在 sessions.db，要么都不在）；重复执行幂等 |
| install | `install.mid_write`、各状态点 | `backed_up`/`written` 崩溃→Undo 后目标目录与安装前**逐字节一致**（含权限）；`verified` 之后崩溃→恢复后 binding 已更新且 receipt 已入队；任何时刻都没有同时存在 `.jsonl` 与 `.jsonl.zst` 的新线程 |

此外：传输层测试模拟乱序（manifest 先于对象到达）、重复、文件被占用（Put/Get 重试）、半文件（`.partial-*` 不被列出）、冲突副本（`Suspect`）；双设备模拟＝两个 `SM_HOME` + 一个共享目录，在单进程里跑 A→B→A，包含 A5/A6/A13 场景。

### 14.5 其他

- `go vet`、`gofmt`、`go test`、`go test -race ./internal/...` 在三平台（CI）必须通过；
- CI 增加一个任务：`go test -tags failpoint ./internal/...`；
- 平台相关（锁、rename、文件占用）的测试放在 `_unix_test.go`/`_windows_test.go`，Windows 上还要覆盖路径大小写和分隔符的 `PathMap` 用例；
- 真实客户端的“显示 / 继续 / 重开”无法进 CI：用 `doc/acceptance.md` 的清单手工验收并记录版本（M1 产出）。

## 15. P0 工具规格（`session-memory dev …`）

这些命令只在 `-tags dev` 构建里存在，发布构建没有。它们是**一次性实验工具**，不追求通用性，但**必须遵守沙箱规则**：任何会写文件的 dev 命令都先调用 `p0.RequireSandbox(目标路径, 真实 home 列表)`，不通过则返回 `not_isolated`、什么都不写。

沙箱规则的完整实现和测试在参考模块 `internal/p0`（`RequireSandbox`、`Marker`、`Inventory` 等类型）：

```go
// internal/p0/types.go
// Package p0 holds the throwaway tooling of the P0 experiments. The commands
// that use it are compiled only with `-tags dev` and never ship (dev.md §8, M1).
package p0

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"session-memory/internal/errcode"
)

// MarkerName is the file that marks a directory as an isolated sandbox. Every
// dev command that writes refuses a target whose sandbox root lacks it.
const MarkerName = ".sm-sandbox"

// Marker is the content of MarkerName.
type Marker struct {
	SchemaVersion int    `json:"schema_version"`
	CreatedAt     string `json:"created_at"`
	Purpose       string `json:"purpose"`
	Owner         string `json:"owner"` // always "session-memory-dev"
}

// Inventory is the output of `dev inventory`: structure only (names, counts,
// table/column names, record type names, JSON key names). It never contains
// titles, prompts, message text, cwd values or any file under an auth or
// credentials path.
type Inventory struct {
	SchemaVersion int          `json:"schema_version"`
	Agent         string       `json:"agent"`
	Home          string       `json:"home"`
	GeneratedAt   string       `json:"generated_at"`
	ClientVersion string       `json:"client_version,omitempty"`
	OS            string       `json:"os"`
	Dirs          []DirInfo    `json:"dirs"`
	SQLite        []SQLiteInfo `json:"sqlite"`
	JSONL         []JSONLInfo  `json:"jsonl"`
}

// DirInfo describes one top-level entry of the home.
type DirInfo struct {
	Path  string         `json:"path"` // relative to the home
	IsDir bool           `json:"is_dir"`
	Files int            `json:"files"` // recursive
	Bytes int64          `json:"bytes"`
	Ext   map[string]int `json:"ext"` // extension -> count ("jsonl", "jsonl.zst", "db", ...)
}

// SQLiteInfo is read through file:<path>?mode=ro&immutable=1.
type SQLiteInfo struct {
	Path        string      `json:"path"`
	UserVersion int         `json:"user_version"`
	Tables      []TableInfo `json:"tables"`
	Error       string      `json:"error,omitempty"` // code only
}

// TableInfo lists a table's columns and row count (COUNT(*) only).
type TableInfo struct {
	Name    string       `json:"name"`
	Rows    int64        `json:"rows"`
	Columns []ColumnInfo `json:"columns"`
}

// ColumnInfo is one column (from PRAGMA table_info).
type ColumnInfo struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	NotNull bool   `json:"not_null"`
	PK      bool   `json:"pk"`
}

// JSONLInfo summarises the JSONL files matching one glob.
type JSONLInfo struct {
	Glob       string              `json:"glob"`
	Files      int                 `json:"files"`
	Forms      map[string]int      `json:"forms"`   // plain | zstd
	Records    int64               `json:"records"` // examined
	ByType     map[string]int      `json:"by_type"` // "type" or "type/payload.type" -> count
	KeysByType map[string][]string `json:"keys_by_type"`
}

// RequireSandbox returns nil only when target lies inside a directory that
// contains MarkerName and does not overlap any real agent home.
//
// Algorithm: resolve target with filepath.EvalSymlinks (the existing prefix
// when it does not exist yet); walk up to the first ancestor containing
// MarkerName; fail with errcode.NotIsolated if none is found, if the resolved
// path is, contains, or is contained by any of realHomes (also resolved), or
// if the user's home directory itself would be the sandbox root.
func RequireSandbox(target string, realHomes []string) error {
	resolved := resolveExisting(target)
	root := ""
	for dir := resolved; ; dir = filepath.Dir(dir) {
		if b, err := os.ReadFile(filepath.Join(dir, MarkerName)); err == nil {
			var m Marker
			if json.Unmarshal(b, &m) == nil && m.Owner == "session-memory-dev" {
				root = dir
				break
			}
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	if root == "" {
		return errcode.New(errcode.NotIsolated, "target is not inside a marked sandbox")
	}
	if home, err := os.UserHomeDir(); err == nil && samePath(root, home) {
		return errcode.New(errcode.NotIsolated, "the user's home directory cannot be a sandbox")
	}
	for _, h := range realHomes {
		rh := resolveExisting(h)
		if samePath(resolved, rh) || within(resolved, rh) || within(rh, resolved) {
			return errcode.New(errcode.NotIsolated, "target overlaps a real agent home")
		}
	}
	return nil
}

func resolveExisting(p string) string {
	p = filepath.Clean(p)
	for cur := p; ; cur = filepath.Dir(cur) {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, strings.TrimPrefix(p, cur))
		}
		if filepath.Dir(cur) == cur {
			return p
		}
	}
}

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(resolveExisting(b)) }

func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}
```

| 命令 | 作用 | 说明 |
|---|---|---|
| `dev sandbox new NAME [--agent codex\|workbuddy]` | 在 `$SM_HOME/dev/sandboxes/NAME/` 创建沙箱：写 `.sm-sandbox` 标记、空的 agent home 目录、`env.sh`/`env.ps1`（导出 `CODEX_HOME`、`WORKBUDDY_CONFIG_DIR`、`CODEBUDDY_CONFIG_DIR` 指向沙箱内目录） | 输出启动客户端的提示（文本） |
| `dev sandbox mark DIR --yes-this-is-not-my-daily-profile` | 把一个**外部已隔离**的目录（如第二个系统用户的 home）标记为沙箱 | 刻意的、需要明示确认的动作 |
| `dev sandbox rm NAME` | 删除沙箱（只删带标记且位于 `dev/sandboxes/` 下的） | |
| `dev inventory --agent A --home DIR --out FILE` | 只读结构清单，输出 `p0.Inventory`（只含名字、计数、表/列名、记录类型名、JSON 键名） | **不要求沙箱**（只读）；读 SQLite 一律用 `file:…?mode=ro&immutable=1`；不读任何 `auth`/`credentials` 路径下的文件 |
| `dev scrub --in F --out G [--seed N]` | 按 §14.2 的算法脱敏一个 JSONL（`.zst` 自动解压） | 输出确定性 |
| `dev codex write --sandbox NAME --rollout FILE [--thread-id UUID]` | 把一个 rollout 作为**新线程**放进沙箱的 `sessions/YYYY/MM/DD/`，文件名按 §8.3 规则生成 | 只写文件，不碰任何数据库（E1a 的“路线 2”） |
| `dev codex append --sandbox NAME --thread-id UUID --rollout FILE` | 用更长的内容替换已有线程文件，按 Codex 的发布协议（先取协调锁、再尝试线程锁，同目录临时文件 + rename，保持权限） | E1b |
| `dev codex locks --home DIR [--thread-id UUID]` | 只读：列出 `thread-writer-locks/` 下的锁文件，并对每个做 `TryLock` 后立即释放，报告“被持有/空闲” | E2a；不要求沙箱 |
| `dev wb register --sandbox NAME --session-id ID --cwd DIR [--user-id UID]` | 向沙箱里的 `workbuddy.db` 的 `sessions` 表插入一条最小行 | **路线 3 实验，仅沙箱**；先备份库文件 |
| `dev convert --from codex\|workbuddy --to codex\|workbuddy --in FILE --out-dir DIR` | 一次性的朴素转换：用 §9.2 的规则（`convert.Convert` 实现后）输出目标格式文件 | P0 用，不进入正式代码 |
| `dev report new E1a` | 在 `doc/p0/` 下按 dev §9.2 的模板创建实验报告 | |

所有 dev 命令支持 `--json`。实验的执行要点与通过标准见 dev §9 与设计 §15。

## 16. 编码任务清单

> 实施状态与已知偏差见 [status.md](status.md)。本节保留为任务定义。

每个任务给出：要创建/修改的文件、依据的章节、完成标准。顺序即建议的提交顺序；**标 ★ 的可与 P0 并行，不依赖实验结论**。

### M0 剩余（★）

| 任务 | 文件 | 依据 | 完成标准 |
|---|---|---|---|
| 0.1 引入 cobra，建立命令行骨架与 `App`、`run` | `cmd/session-memory/{main,app,root}.go` | §12 | `go build ./cmd/...` 成功；`run([]string{"--help"})` 退出码 0 |
| 0.2 `version` 命令 | `cmd_version.go` + 测试 | §12.2 | 文本与 JSON 输出符合规格；`-ldflags` 注入生效（测试用 `go build -ldflags` 构建后执行） |
| 0.3 `config` 命令 | `cmd_config.go` + 测试 | §12.2 | `show/get/set/path`；未知键退出码 2；`set` 不合法值不写文件 |
| 0.4 `agent probe codex` | `cmd_agent.go`、`internal/source/codex/probe.go` 改造 | §8.3、§12.2 | 对合成夹具目录输出符合 `ProbeResult`；输出中不含任何夹具里的标题/cwd/文本（哨兵测试） |
| 0.5 `internal/errcode` | 复制参考实现 | §12.1 | 参考测试通过；退出码映射有表驱动测试 |
| 0.6 `internal/logx` + lint 测试 | 复制参考实现，新增 `lint_test.go` | §13.1 | `TestNoRawLogAttrs` 在现有代码上通过（现有代码没有日志调用） |
| 0.7 `internal/failpoint` | 复制参考实现 | §13.2 | `-tags failpoint` 下 `crash/error/panic` 各有测试 |
| 0.8 CI | `.github/workflows/test.yml` | §14.5 | 增加 `-tags failpoint` 任务；三平台通过 |

### M2 Codex 采集（★ 全部不依赖 P0）

| 任务 | 文件 | 依据 | 完成标准 |
|---|---|---|---|
| 2.1 `jsonl.Source`（plain/zstd） | `internal/jsonl/source*.go`（引入 `klauspost/compress`） | §8.2、`internal/jsonl/source.go` | plain 与 zstd 读出相同字节；zstd 的 `OpenAt` 前缀哈希正确；截断的 zstd 帧报错而不是当作 EOF |
| 2.2 合成夹具生成器 | `internal/testutil/codexgen/` | §14.2 | 同 Seed 输出逐字节相同；`Zstd` 可被 2.1 读回 |
| 2.3 复制 `session`、`canonjson`、`vault`（类型与向量） | `internal/{canonjson,session}/` | §1.4、§2 | 参考测试与全部向量通过 |
| 2.4 Codex 发现 | `internal/source/codex/discover.go` 重写 | §8.3、§11 | 14.2 的“发现”相关场景；多 home；孪生合并；`archived_sessions` |
| 2.5 配置：多 home | `internal/config/` | §11 | §11 第 5 条测试 |
| 2.6 Codex `Capture` | `internal/source/codex/capture.go` | §8.2 | §14.2 夹具表全部通过；首次采集 385 MB 级合成会话的峰值内存有断言（≤ 4×`MaxSegmentBytes`） |
| 2.7 让旧 `parser.go` 退场 | 标记废弃，不再被引用 | dev §7 | `source/codex` 不再 import `conversation`、`checkpoint`（`source/claude` 仍用） |

### M3 本地 store 与回环（Writer 依赖 P0，其余 ★）

| 任务 | 文件 | 依据 | 完成标准 |
|---|---|---|---|
| 3.1 对象库 + 测试用 Sealer | `internal/store/objects.go`（build tag 隔离的透传 Sealer 放 `sealer_test_only.go`，**tag `testonly`，发布构建不编译**） | §4、§6.1 | `PutObject/PutSealed/OpenObject`；`PutSealed` 校验哈希；重复写幂等 |
| 3.2 journal 与恢复 | `internal/store/{journal,tx,recover}.go` | §4 | `Flow` 驱动；§14.4 的 capture 行通过 |
| 3.3 binding 存取 | `internal/store/bindings.go` | §3.3 | 原子写；损坏→`.corrupt-*`+`recovery_needed`；`--repair-binding` 流程有测试 |
| 3.4 sessions.db | `internal/store/db.go`（引入 `modernc.org/sqlite`） | §5 | DDL 建库；`user_version` 不符→重建；`Rebuild` 从 manifests+bindings 重现同样的视图 |
| 3.5 `session` 纯逻辑接入 | 使用参考的 `Graph/Resolve/LandingFor` | §10 | A5/A6/A13 测试在 store 层再跑一遍 |
| 3.6 `track`/`list`/`checkpoint`/`status`/`verify` 命令 | `cmd/…` | §12.3 | 对合成夹具端到端：纳管→采集→revision 入库 |
| 3.7 双设备回环（Noop 传输：目录复制） | `internal/store/loopback_test.go` | dev M3 | 设备 A 采集的 revision 在设备 B 的 store 里可验证、可 `OpenObject` |
| 3.8 Codex Writer（**待 P0**） | `internal/source/codex/writer.go` | §4.4、§8.3 | 依 E1a/E1b 结论；§14.4 的 install 行通过 |

### M4 vault、传输、同步（★ 除自动化的微云验证外）

| 任务 | 文件 | 依据 | 完成标准 |
|---|---|---|---|
| 4.1 vault | 复制参考 `internal/vault/`；接 `go-keyring` | §6 | 参考测试与向量通过；凭据存储不可用时拒绝 |
| 4.2 `init`、`device invite/join/regenerate-id`、`transport attach-folder` | `cmd/…` | §6.3、§7.4 | C2：只有同步目录、没有加入包时无法解密；加入后校验 `key_check` |
| 4.3 FolderTransport | `internal/transport/folder.go` | §7.2 | 乱序、重复、占用、半文件、冲突副本用例；`Put` 幂等/`ErrExists` |
| 4.4 publish 事务 | `internal/sync/publish.go` | §7.3 | §14.4 publish 行通过 |
| 4.5 reconcile/pull | `internal/sync/pull.go` | §7.4 | A8、C1；篡改一个字节→`auth_failed` 进入 quarantine；序号缺口→`pending_prev` |
| 4.6 克隆检测 | `internal/sync/deviceid.go` | §7.4 | 模拟备份恢复：本地序号落后→拒绝发布 |
| 4.7 去掉测试 Sealer | 移除 `testonly` 实现的所有引用 | dev M4 | 发布构建里不存在透传实现（`go list -tags ''` 检查） |

M5–M7 的任务在各自里程碑开始前按同样格式补充（依赖 P0 结论）。

## 17. 仍待 P0 决定的项

这些是本文**有意留空行为**的地方；接口已定，行为由实验定。

| 项 | 位置 | 由谁决定 | 实验前的默认 |
|---|---|---|---|
| Codex 新线程是否只放文件就够（需否登记/索引写入） | §8.3 Writer | E1a | `RouteFiles`；不写任何数据库 |
| 对已登记线程的原地追加做法、路径变换改变字节长度的处理 | §4.4、§8.3 | E1b | 先备份、同目录 rename、取 Codex 写锁；长度改变的变换对已投影线程拒绝 |
| 目标线程只有 `.zst`、孪生文件如何处理；是否会被 Codex 压缩 | §8.2、§8.3 | E1b | `needs_action`，不处理 |
| `model_provider` 取值与历史可见性 | §8.3 | E1a | 取目标当前提供商 |
| 账号/权限/沙箱字段是否需要剔除或改写 | §8.3 | E1c | 不修改这些字段，也不声称覆盖 |
| 子线程是否随父会话一并采集安装 | §8.3、`Options.IncludeSubagents` | E1c | 作为依赖列出缺口，不一并安装 |
| 运行检测的最终信号与“运行中是否需重启”矩阵 | §8.3 运行检测 | E2a、E2b | 线程锁 + 进程的保守组合；未知按运行中 |
| WorkBuddy 是否需要登记行及最小行内容 | §8.4 Writer | E3 | `Unsupported` |
| Codex Desktop 是否需要 UI 层记录与连续 `ordinal` | §9.2、Codex Writer | E4 | 不生成 |
| 导入边界与回采 | `ImportReceipt`、§8.2 | E5 | 无法划定边界则不开放往返续写 |
| 转换后模型的接受度、上下文容量、compaction 映射 | §9.2 | E6 | 规则按表；报告注明容量风险 |
| 微云的冲突副本命名与文件行为 | §7.2 | E7 | 通用模式 + `Suspect` + 内容校验 |
| 导入记录与 `session_id` 绑定的位置 | `BindingEntry` | E8 | 以同步的 binding 条目为准 |
| fork/resume 产生新原生 ID 的归属 | — | E9 | 不自动判定，提示用户 |
| 人类输入判定规则 | `canonical.Item.Human`、§8.4 | M2 真实样本 + E4 | §8.4 的起点启发式 |
| Segment 大小上限、稳定边界间隔的数值 | §2.2、设计 §11.3 | M2/M3 实测 | 8 MiB；5 分钟与空闲 10 秒 |

## 18. 参考模块的验证记录

参考模块 `spec/_reference/`（模块名 `session-memory`，`go 1.25.0`，`golang.org/x/crypto v0.46.0`）验证过：

```bash
cd doc/spec/_reference
gofmt -l .                      # 无输出
go vet ./...
go vet -tags failpoint ./...
go vet -tags dev ./...
go test ./...                   # canonjson、session、vault、p0 通过
go run ./cmd/vectors            # 重新生成本文向量
```

`cmd/vectors` 打印的 `REVISION_ID`、`REVISION2_ID`、`SEGMENTS_DIGEST`、`MANIFEST_NAME` 与 §2.3 一致；`vault_test.go -run TestSealVector -v` 打印的 `SEAL_VECTOR_HEX`、`MANIFEST_VECTOR_HEX`、`MANIFEST_AAD_HEX` 与 §6.4 一致；§5 的 DDL 已在 `sqlite3` 中执行通过。

参考模块里**没有实现**的部分（有意只给类型或合约）：`store` 的实现、`transport` 的实现、各适配器、`convert.Convert`、所有命令。这些就是 §16 的编码任务。
