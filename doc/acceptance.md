# 实际验收清单

> 更新：2026-10-27。证据：[第一轮 P0](p0-report.md)、[第二轮 P0](p0-live-report.md)、[WorkBuddy Writer 验收](p0-workbuddy-writer-report.md)。勾选仅代表所述层级通过。

## 已通过

- [x] 独立状态目录、系统钥匙串和加密目录上的 init / invite / join / track / publish / pull / restore。
- [x] Codex CLI 0.162.1：原生生成会话，经 A→B→A→B 同步和两次继续写，重开后完整 3 轮。
- [x] Codex app-server：列表、加载、分页历史、路径长度不同的恢复；由客户端自行登记和投影。
- [x] Codex app-server 持有线程锁时拒绝实验写入；目标本地出现额外设置记录时安全拒绝覆盖。
- [x] WorkBuddy 5.8.3：隔离登录、真实模型两轮对话、发现、采集、深度验证。
- [x] WorkBuddy 用户 query 包装实测修复：同一原始 revision 可导出完整两轮；转换计划保留 4 条消息。
- [x] WorkBuddy 原生快照加密发送到另一隔离配置，received 可见，两端从快照导出的 Resume Pack 字节一致（未安装原生 WorkBuddy）。
- [x] Codex 三轮 Resume Pack 导出后粘贴给 WorkBuddy，真实模型正确引用全部测试标记（context-only）。
- [x] WorkBuddy 文件写入路线对照：仅文件重启后不显示；增加同账号完整登记行后历史可见、真实模型续写成功（仅隔离候选实验）。
- [x] 主 CLI 的 WorkBuddy 原生 Writer：版本/schema 白名单、最小登记、目标账号、路径映射、进程保护、备份和行级回滚；28 项中断恢复断言通过。
- [x] 主 CLI 原生往返：独立状态目录接收并恢复 → WorkBuddy GUI 完整两轮 → 真实模型第 3 轮 → 同步回传并恢复源端；声明关闭不能覆盖实际运行中的检测。
- [x] 无默认 macOS 钥匙串时快速失败；不落盘明文 vault key。

## 尚未通过，不能作为已支持能力发布

- [ ] Codex Desktop GUI 的侧边栏、点开、真实模型续写与重开（CLI / app-server 成功不能代替 GUI 验收）。
- [ ] 两台物理电脑之间的同 Agent 原生同步；当前是同一台 Mac 的独立配置模拟。
- [ ] WorkBuddy Writer 扩大支持范围：其他版本/路径编码/更大容量、厂商协作锁、无人值守自动安装及真实跨账号切换。
- [x] Codex ↔ WorkBuddy 转换写入（命令行/app-server 级）：见 [p0-convert-report.md](p0-convert-report.md)。
- [ ] 转换后的会话在 Codex Desktop 与 WorkBuddy 图形界面里的显示、继续与重开（只验证了 app-server/CLI 与测试）；WorkBuddy 目标尚无真实客户端续写验证；原生工具记录、附件、压缩边界仍按文本/丢弃处理。
- [ ] 跨账号隔离、工具调用与结果、附件、子代理、编辑分支、摘要/压缩的真实客户端样本。
- [ ] 微云真实传输：乱序、延迟、冲突副本、离线恢复与重连（E7）；共享本地目录不替代该验收。
- [ ] Windows / Linux 实机和相应客户端上的操作系统锁与凭据存储行为。
- [ ] 后台常驻与登录项；完整无人值守的安装/恢复流程。

多 session 合并按设计延期，不列为本期通过条件。
