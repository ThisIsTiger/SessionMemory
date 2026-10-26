# SessionMemory

SessionMemory 是个人使用的单会话同步工具，以“一个会话”为同步单位，解决两件事：

1. **同一 Agent 跨电脑接续**：公司和家里的 Codex 之间同步选中的会话，在目标电脑的原生历史里恢复并继续，继续后的记录再同步回来。
2. **跨 Agent 接续**：把选中的会话以完整历史转换后写入另一个 Agent（如 Codex → WorkBuddy）的原生会话列表，继续后可再切回；转换不可用时退化为仅上下文的 Resume Pack。

会话数据通过加密的不可变对象，经微云等同步文件夹在设备间传输。同项目下多个会话的合并暂不实现，每个会话独立同步。

当前状态：设计（v0.6）、开发计划、实现规格已定；同 Agent 同步（Codex ↔ Codex）的整条链路已实现并有测试（采集、加密、传输、发布/接收、冲突保护、原生恢复，默认路线），P0 实验工具已就绪，真实客户端行为待 P0 验证；跨 Agent 的转换规则、Canonical 提取、WorkBuddy 读取侧与 Resume Pack 已实现，写入目标 Agent 待 P0。详见 [`doc/status.md`](doc/status.md)。产品设计见 [`doc/unified-agent-session-sync-design.md`](doc/unified-agent-session-sync-design.md)（v0.6）。开发计划见 [`doc/dev.md`](doc/dev.md)，实现规格（类型、格式、算法、编码任务）见 [`doc/spec.md`](doc/spec.md)。尚未实现：MCP server、跨 Agent 的写入（Writer）、Claude Code 适配，以及依赖 P0 实验结论的部分。

## 已复制的基础代码

代码来自同一工作区的 `../summarize/agent-daily`，复制基线为提交 `aebe8f7`，并已把 module、路径、配置名和 hook ownership marker 改为 `session-memory`。复制范围仅包括：

- Claude Code / Codex session 的发现、分类、增量 JSONL parser、测试 fixture；
- checkpoint store 与原子文件操作；
- 统一 conversation model；
- Claude SessionEnd hook 队列与非破坏性安装逻辑；
- Codex probe；
- source adapter 所需的配置和跨平台基础设施。

没有复制日报摘要、博客、LLM provider、倒排索引、报告、scheduler、旧 CLI 或 `clerk` 代码。复制后的代码属于本项目，后续不要求与 agent-daily 同步演进。

## 验证

```bash
gofmt -w .
go vet ./...
go test ./...
go test -race ./internal/...
```

本仓库目前为 private / unlicensed，不应对外分发。
