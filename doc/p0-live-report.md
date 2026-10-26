# P0 第二轮：真实客户端与隔离往返测试

后续已把版本受限、默认关闭的 WorkBuddy 原生 Writer 接入主 CLI，并完成真实续写往返，见 [第三轮报告](p0-workbuddy-writer-report.md)。本文保留第二轮当时的结论。

> 日期：2026-10-26。macOS arm64；Codex CLI 0.162.1；WorkBuddy 5.8.3。
> 第一轮报告：[p0-report.md](p0-report.md)。验收清单：[acceptance.md](acceptance.md)。

## 结论

Codex 的 CLI / app-server 原生往返链路通过；WorkBuddy 的真实模型对话、采集、转换计划及 Resume Pack 通过。Codex → WorkBuddy 的 Resume Pack 上下文交接通过。WorkBuddy 的原生文件恢复实验表明：仅放文件不会显示，补充同账号的登记行后，完整历史可见且真实模型可续写。

**这不代表两个产品的全部原生同步已交付。** WorkBuddy Writer 和跨 Agent 转换写入仍返回 `unsupported`；登记实验是独立沙箱脚本，尚未满足生产写入的锁、版本白名单、最小字段和事务恢复要求。Codex Desktop GUI、真实双机和微云同步仍未验收。

## 环境与证据

- 全部会话为新生成的合成验收内容；没有导入日常对话、复制登录凭据或向日常客户端目录写入会话。
- 两个 Codex 配置使用独立 HOME、CODEX_HOME、SM_HOME、独立 macOS 测试钥匙串、同一加密 FolderTransport。两个 Git 夹具的 HEAD 相同，本地目录长度不同。
- Codex 使用**真实安装的 CLI 与 app-server**；模型服务为只监听 127.0.0.1 的受控 Responses SSE 替身。它验证续写请求中确实携带历史，不能证明真实 Codex 模型的语义回答。
- WorkBuddy 以独立 HOME / WORKBUDDY_CONFIG_DIR / CODEBUDDY_CONFIG_DIR / Electron user-data 启动，由用户在此配置中登录。实际回答来自界面标示的 Deepseek-V4.1-Flash。
- 测试前后，日常 WorkBuddy 数据库的 mtime 相同，正常用户的默认钥匙串仍为 login.keychain-db。测试 vault 密钥由系统钥匙串保存，没有增加明文 KeyStore。
- 本机证据目录：`~/.session-memory/dev/sandboxes/live-20261026-pw1nxsm0/`。其中 `roundtrip-2/results.json`、`workbuddy/sessionmemory-results.json`、`workbuddy/file-only-import-results.json`、`workbuddy/registration-results.json` 记录结果；脚本与隔离数据留在同一目录。钥匙串、加入包、登录配置与原生记录不收入仓库。

## Codex 往返链路

1. 在 A 用真实 `codex exec` 创建 ALPHA 会话；SessionMemory `init → track → sync` 发布加密对象。
2. B 通过受口令保护的加入包加入 vault，`sync → list → restore --cwd`。未安装的 received session 可见；恢复时映射至不同长度的项目目录。
3. B 的真实 app-server 执行 `thread/list`、`thread/resume`、`thread/turns/list`，读取首轮历史。
4. B 用真实 CLI 继续 BETA；本地模型服务收到的请求包含 ALPHA 的问题与回答。
5. B `checkpoint → sync`，A `sync → restore`，A 继续 GAMMA；请求含前两轮完整标记。
6. A 再次发布，B 接收恢复并重启 app-server；分页历史包含完整 3 轮、6 个问题/回答标记。
7. 单独沙箱内让真实 app-server 加载线程，`dev codex append` 即使声明 client-closed 也以 `client_running` 拒绝。测试没有改动被加载的原始往返线程。
8. 扫描该轮传输目录的 13 个文件，未发现任何合成对话标记的明文。

这些验证覆盖产品 CLI、真实 OS 凭据存储、加密发布/加入/接收、原生文件落地、路径映射、继续写、回传与重开。未直接写 Codex 数据库；登记和历史投影由 Codex 自己完成。

### 加载旧线程也会造成原生变化

一个前置尝试先在目标端 `thread/resume` 旧版本，再安装已收到的新版本。Codex 即使未发起新对话也会追加 `thread_settings_applied`，两端时间戳不同，目标因此有本地变化。恢复按设计返回 `needs_action: target thread has diverged`，没有覆盖它。

成功实验使用“同步并安装，再打开线程”的顺序。这里只证明安全拒绝；没有把上述元数据变化静默忽略，也没有合并两端会话。

## WorkBuddy 真实对话与采集

- 在隔离项目中创建会话，第一轮返回 `ACK_WORKBUDDY_ALPHA`；第二轮根据历史返回 ALPHA 和 BETA。
- 原生主文件为 JSONL，包含毫秒时间戳、顶层 role/content、parentId 链、session-meta、ai-title、reasoning 和 file-history-snapshot；同时生成 file-rollback.ndjson。
- SessionMemory 正确发现标题/cwd，纳管主文件与 rollback 文件；`verify --deep` 返回 0 个问题。
- A 发布 WorkBuddy 快照，B 接收后显示 received；B 从加密对象重新导出的 Resume Pack 与 A 字节完全一致。此次没有原生安装到 B 的 WorkBuddy。追加此轮对象后扫描传输目录全部 22 个文件，未发现测试对话标记明文。
- `convert --agent codex --plan` 可生成规则与保真度报告；实际写入命令返回退出码 5 / unsupported，未伪报转换成功。

### 实测发现并修复：用户问题被当成系统上下文

WorkBuddy 5.8.3 将多个 `<system-reminder>` 与 `<user_query>真正的问题</user_query>` 放入同一个 input_text。原先“以 `<` 开头即非人类”的判定丢掉了两个用户问题：转换计划只保留 2 条助手消息，Resume Pack 为 0 轮。

修复后，适配器仅从派生文本去掉前置 reminder、提取 query，原始快照及 RawRef 保持完整；无包装记录仍使用保守规则。适配器版本升为 0.1.1，避免新旧派生语义共用版本标识。相同已采集 revision 重新导出后为完整 2 轮，转换保留 4 条消息、丢弃 0 条消息；系统上下文未进入 Resume Pack。回归覆盖包装、HTML 问题、场景标记、不完整包装、活动路径、转换及 Resume Pack。

## Codex → WorkBuddy 的上下文交接

用真实 `resume-pack` 命令导出 Codex 三轮历史，将产物作为引用资料粘贴到 WorkBuddy 的新会话，要求按顺序回答历史标记。真实模型返回 `ACK_ALPHA`、`ACK_BETA`、`ACK_GAMMA`、`ACK_BRIDGE_OK`；原生落盘记录也确认这些回答。

这是 **context-only 的文件导出与人工粘贴路径**，不等于转换后的原生历史已自动安装。没有给 WorkBuddy 复制 Codex 的系统提示或凭据。

## WorkBuddy 原生写入路线实验

实验只在同一个已登录的隔离配置中进行，源和目标使用同一项目路径、同一账号。源会话已有两轮完整历史；目标采用新的 session ID 和独立标题。

| 路线 | 操作 | 结果 |
|---|---|---|
| 仅文件 | 恢复新 ID 的 JSONL 和 rollback，保持 parentId 链；不改数据库，重启客户端 | 新会话未出现在侧边栏，sessions 表没有自动补登记 |
| 文件与登记 | 关闭隔离应用，先用 SQLite backup 备份隔离数据库，事务复制源会话的登记行，修改目标 ID/标题/更新时间，再重启 | 新会话可见，显示完整两轮历史 |
| 登记后的续写 | 打开上述目标，请真实模型从历史取回 ALPHA/BETA，附加新的验收标记 | 返回两个历史标记及 `ACK_NATIVE_IMPORT_OK`，原生文件增加第 3 轮 |

结论仅支持“5.8.3 上，同账号、相同项目路径，完整登记行可作为路线 3 的候选”。未证明最小登记行、跨账号字段迁移、不同路径、工具结果/附件/子代理/压缩、已有线程快进、运行中写入安全、数据库版本兼容、崩溃恢复。生产 Writer 保持关闭，也不据此否定尚未调查的官方导入接口。

## 实测发现并修复：缺默认钥匙串时初始化挂起

空 HOME 下没有 macOS 默认钥匙串，底层 `security -i` 写入可能一直等待。增加不带密钥的默认钥匙串预检并设置 5 秒超时；不可用时拒绝初始化。最终二进制在此夹具中 0.05 秒返回失败（退出码 9），有测试钥匙串的完整链路正常。已有钥匙串锁定时仍按 OS 流程解锁；没有关闭安全提示或回退为明文存储。

## 自动化复核

Review 修复后的完整测试见 [status.md](status.md)。本次新增实测修复后再次运行：

- `go test ./...`、`go test -tags failpoint ./...`、`go test -tags dev ./cmd/... ./internal/p0/...`：通过。
- `go vet ./...`、dev / failpoint 两种标签的 vet、`git diff --check`：通过。
- 最终 query 边界收紧后，重跑 WorkBuddy / bridge / convert / vault 的 race 测试，并用最终二进制对同一个真实快照重新导出，仍为完整两轮。
- 新增 macOS 钥匙串隔离预检；Windows / Linux 对新增涉及包的交叉 vet 通过，仍不能替代实机测试。

新增依赖：无。未提交 Git commit。

## 仍需验收

见 [acceptance.md](acceptance.md)。优先实现并验证有版本限制、运行保护、备份与 journal 的 WorkBuddy Writer，再完成 Codex ↔ WorkBuddy 的原生转换安装；之后做真正双机和微云断网/重连。多 session 合并继续不在范围内。
