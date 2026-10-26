# WorkBuddy 原生 Writer 接入与实际验收

> 日期：2026-10-26～2026-10-27。macOS arm64，WorkBuddy 5.8.3。前置实验：[第二轮报告](p0-live-report.md)。

## 已完成

原生恢复已接入主 `session-memory restore --agent workbuddy` 命令，不再依赖手动 SQL 实验脚本。当前采用默认关闭、版本受限的手动路线 3：原生文件加最小登记行。

正式二进制完成：源端 track → 加密同步 → 目标 restore → WorkBuddy GUI 显示两轮完整历史 → 真实模型续写第 3 轮 → checkpoint/加密回传 → 源端 restore。源/目标项目路径不同；最终源文件包含完整三轮，源路径映射正确。模型在续写时返回历史 ALPHA/BETA 标记及 ACK_WRITER_E2E_OK。

这次不再复制完整登记行。新行以目标 schema 默认值为基础，仅设置会话身份、目标 cwd、标题、状态、时间和目标本地账号；模型、权限、专家等私有配置保持目标默认。界面显示的本次实际模型为 GLM-5.3-Flash；历史两轮仍显示原来的模型信息。

## 实测步骤与结果

| 检查 | 结果 |
|---|---|
| 源/目标使用独立 HOME、SM_HOME、系统测试钥匙串，同一加密目录 | 通过；未复制 WorkBuddy 登录凭据 |
| CLI 从真实 WorkBuddy 合成会话记录构造的新 ID 快照采集并发布 | 通过；源端仅为测试夹具，不声称来自第二台已登录电脑 |
| 目标端 restore --plan | route=3，列出主 JSONL、rollback 流与 sessions 行共 3 项写入 |
| WorkBuddy 正在运行仍声明 client-closed | 退出码 4 / client_running；未创建目标行 |
| 窗口退出但后台进程未结束 | 继续拒绝；等待进程完全退出后安装成功，未强制杀进程 |
| 正式 CLI 恢复 | receipt 的 writer_version 为 workbuddy-writer/0.1-registry，完整目标后置条件校验通过 |
| 重开并点击恢复会话 | 侧边栏出现 SessionMemory Writer E2E，显示源两轮历史 |
| 真实模型继续对话 | 返回两个历史标记与 ACK_WRITER_E2E_OK；新记录 cwd 等于目标项目路径，sessionId 保持一致 |
| B checkpoint/sync → A sync/restore | 通过；源端保留前缀并包含第 3 轮回答 |

测试只使用新建的隔离数据和合成提示。前端登录仍是用户自行完成的隔离登录，未向日常 WorkBuddy 会话目录写入测试会话。

本机证据：`~/.session-memory/dev/sandboxes/live-20261026-pw1nxsm0/workbuddy/writer-e2e/results.json`，及同目录外层的 `writer_e2e.py`。报告只保存计数、ID、plan、receipt 和布尔断言，不把登录配置、凭据、加入包或完整原生对话收入仓库。

## 安装与恢复保护

- 主 CLI 根据 agent 选择正确的默认 home，WorkBuddy 不再被 Codex 类型断言阻断。
- 检查安装版本、完整 sessions schema 指纹、额外触发器、单账号条件及目标路径，未知情况拒绝。已有同 ID 文件/行要求属于受管理 binding，文件只允许已验证前缀的快进。
- SessionMemory 的 home 安装锁贯穿 Apply、Capture/adoption、完整 Verify 与 binding 提交。它只串行化本工具，WorkBuddy 不遵守此锁，因此还要求显式退出应用并重复检查进程；不允许后台自动安装。
- 首次改厂商文件之前完成 staging、文件备份、行级 before/after 备份和 journal。登记使用 SQLite 事务与行基线比较，不恢复整个数据库；无关会话即使在恢复前改变也保留。
- 目标后置条件覆盖全部声明的 artifact、文件权限和登记行，含本次未改写的文件；adoption 后再次校验，才写 verified/binding。
- WorkBuddy 中途启动时保留 recovery_needed 现场，待完全退出后再恢复。恢复遇到已前进的文件/行、新增 artifact 或缺失的原文件时拒绝覆盖，不能用备份抹掉客户端新数据。
- 预写入校验失败时尚未发布 RegistryUndo 指针，能正常清理 staging，不会因不存在的登记备份导致恢复卡住。

## 自动化验证

新增/快进各覆盖 14 个中断点，共 **28 项**：六个安装状态的 before/after、文件写入中间、SQLite 登记提交后。用 panic 保留中断状态，关闭并重开 store 后验证：written 及以前撤销，verified/binding_committed 前滚，最终没有悬挂 journal。它模拟进程中断顺序，不等于 OS 断电测试。

其他回归覆盖：目标账号与默认权限/模型、未知 schema、关闭声明、账户切换导致基线变化、多账号、符号链接、无关行保留、厂商文件/行已前进时拒绝回滚、未改写 artifact 的后置条件、客户端在 adoption 阶段启动、写入前失败、跨路径原生往返。

验证通过：默认全量、failpoint 全量、dev CLI/P0 测试；internal race；默认/dev/failpoint vet；Windows/Linux 交叉 vet。最后收紧运行中回滚条件后，重跑 WorkBuddy/engine 的 failpoint、race 及默认 vet。交叉 vet 不代表在 Windows/Linux 实机运行过此 Writer。

## 使用条件

目标应为独立、单账号配置，已经登录并创建过本地会话。配置中的 home 必须明确指向准备使用的 WorkBuddy 数据目录；本轮只对隔离测试 HOME 启用开关。

```bash
# 先将 agents.workbuddy.home 配置为目标数据目录，再手动启用。
session-memory config set agents.workbuddy.allow_registry_write true

session-memory restore <session_id> --agent workbuddy --cwd <本地项目目录> --plan
# 完全退出 WorkBuddy（含后台进程）后执行；整个安装/恢复期间保持退出。
session-memory restore <session_id> --agent workbuddy --cwd <本地项目目录> --client-closed
```

若安装中途启动了客户端而出现 recovery_needed，先退出客户端，再运行 SessionMemory 触发启动恢复；若提示原生状态已前进，不要删除 journal 或覆盖厂商文件，应先保留现场处理。

## 仍未支持或未验收

- 只允许 `/Applications/WorkBuddy.app` 的 macOS 5.8.3 和已知 registry schema；其他版本/安装位置/操作系统保持关闭。
- 路径编码仅验证无空格的 ASCII 绝对路径；单 artifact 最多 64 MiB、单次最多 128 MiB，超过明确 unsupported。
- 缺少厂商协作锁，不开放 WorkBuddy 无人值守自动安装；真实账号切换、多账号目录需另行设计和验收。
- 真实跨两台电脑、微云、工具/附件/子代理/压缩及编辑分支的大样本仍待验收。
- Codex ↔ WorkBuddy 的原生转换写入仍 unsupported；已通过的跨 Agent 方式仍是 Resume Pack。多 session 合并继续延期。
