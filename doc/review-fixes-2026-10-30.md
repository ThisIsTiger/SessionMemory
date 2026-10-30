# 复审问题修复（2026-10-30）

基于 `5d703300710f654f681e3437bd5b11ae19f07f04` 修复 D01–D03、C01–C04。改动尚未提交或发布。

| 问题 | 修复后的行为 |
| --- | --- |
| D01 认领期间等长改写漏备份 | 最终 Capture 的不可变对象按远端原分段边界重新哈希后才能绑定；校验不符则拒绝认领，后续 Track 能备份改写内容。不同分段的相同字节仍可匹配。 |
| D02 读取失败制造副本 | snapshot 下载、认证及校验错误向上传播；不完整 manifest 或清单链缺口阻止认领。恢复读取后重试仍认领原 SessionID。 |
| D03 hooks 成本保证错误 | INSTALL.md 明确密集对话可能合并上传、稀疏回复可能增加请求；保留现有上传延迟与批处理策略。 |
| C01 reset 移动活动状态 | 状态目录外的持久锁覆盖命令与 hook worker 的生命周期，reset 排他持有，并检查旧版进程的内部锁；忙时立即拒绝，改名后新命令也不能重建状态目录。 |
| C02 URL 错误泄露签名 | 请求构造及网络错误只显示来源主机和安全类别，不展开嵌套解析/重定向异常。底层原因保留给 errors.Is/As，超时、取消、DNS 分类仍有效。 |
| C03 reset 后无法重新授权 | 显式 setup/join 在无设备身份时验证云端 key 文件、VaultID、key_check，再保存新 skill 令牌。现有身份仍要求同库；错误钥匙、错库或损坏身份不替换凭据。 |
| C04 扩大时间窗口无效 | 显式 setup 更新同目录的全部项目映射窗口，完整历史选项清除下限；project track 清除所选项目的窗口；后台沿用已有策略。 |

新增正式测试位于 `internal/sync/claim_review_test.go`、`internal/transport/weiyun_url_test.go`、`cmd/session-memory/review_regression_test.go` 和 `cmd/session-memory/reset_lock_test.go`。除了原反例，还检查了读取恢复后重试、错误钥匙拒绝、目录改名后的互斥、等待中的真实 hook worker、同步期间 hook 仍能记录事件，以及收窄窗口后多个 watcher 不会重新接入旧会话。

D03 是文档契约修复：旧审查脚本 `doc/review-5d70330/run.py` 中“hooks 上传批次不得超过后台”的断言仍会失败，因为它断言的是已撤回的绝对保证；不能将这一条当作当前功能回归。D01/D02 和 C01–C04 的反例已转入正式测试，使用正常 `go test` 验证。

本次没有操作真实微云、launchd、用户 hooks、钥匙串或原生会话。构建结果保存在临时验证目录，没有替换已安装程序。

原延期 A08、A09、A10、A15、A16 未纳入本次修复：损坏 revision 重建处理、同长度远端冲突、备用密钥重试、WorkBuddy 旁路删除与 WAL 软删除仍需后续处理。

## 验证结果

- 全量 `go test -race -tags dev,failpoint -count=1 -timeout=15m ./...`：559 个测试/子测试通过，3 个 live 跳过，26 个有测试的包通过；无构建失败或 DATA RACE。
- 新增回归连续三轮：117 个测试/子测试事件全部通过。
- go vet、gofmt 与 git diff --check 通过；修改代码及新增测试的凭据扫描无命中。
- CLI 的 macOS arm64/amd64、Linux amd64/arm64 构建通过；另验证了修改后的 Windows platform 包可交叉编译（不表示支持 Windows 应用）。
- 详细原始日志、构建哈希及结果在 `/tmp/session-memory-fix-5d70330/`，汇总为 `final-results.json`。没有执行真实账号下的云端或客户端 UI 端到端测试。
