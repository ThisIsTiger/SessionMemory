# 开源发布前审查（2026-10-29）

结论：**未发现真实微云凭据被提交或打进受检发布包，但发现运行时泄露风险；当前版本尚不适合按“普通用户开箱即用、自动同步可靠”的状态推广。** 本轮新增 5 组问题（含工具链风险）；其中 4 组有 5 个行为断言，race 模式连续三轮共 15 次失败。此前延期的 9 项功能问题也全部再次复现。

> **修复状态**：S01、S02、S03、U01、U02 均已修复，反例移入正式测试集（原始复现保留在 [fixed/](fixed/)，`run.py` 现在无未决项）。S01：`go.mod` 升到 `go 1.26.10`，govulncheck 报告 0 个可达漏洞，release 工作流先 `go vet`、`go test`。S02：认证 MCP 请求只允许同源重定向（`internal/transport/weiyun_secret_test.go`）。S03：传输层错误只保留 scheme://host、令牌替换为 `[redacted]`（同上）。U01：不是无条件改用 skill 令牌；保存的令牌被拒绝后，仅当 skill 令牌能读到同一个 vault 才换用并保存（`cmd/session-memory/weiyun_reauth_test.go`；原 overlay 断言“刷新后无条件取新令牌”被有意替换为这个更安全的语义）。U02：`NEEDS_ACTION` 与 `PENDING` 分开（`cmd/session-memory/onboard_test.go`、`INSTALL.md`）。同时关闭的旧项：A05、A06、A07、A13。下文保留审查时的原始描述。

本轮审查没有修改业务代码。审查基线为 `b219d94` 加上上一轮未提交的 F01–F03 修复。精确文件哈希、测试结果与证据位置见 [results.json](results.json)。复现脚本只使用合成凭据、临时目录、本地 HTTP 服务和模拟网络错误。

## 当前公开版本与受检版本

- GitHub 仓库已经是 public，远端 main 为 `b219d94`。
- [v0.1.0](https://github.com/ThisIsTiger/SessionMemory/releases/tag/v0.1.0) 已在北京时间 2026-10-29 11:29:29 发布，指向 `2649c40`。下载的 macOS arm64 程序也报告这个 revision，`vcs.modified=false`。
- **工作区的旧 journal 保护、增量旧段校验、接收历史补传这三个修复均未进入 main 或 v0.1.0。** 对工作区测试通过，不能据此认为下载入口已经安全。
- 通过真实发布 URL 执行 `install.sh`，使用临时 `SM_BIN_DIR` 和 `SM_NO_SETUP=1`：下载、SHA-256 校验、安装、version/help 均成功。受检资产 SHA-256：`22dfc50ab29d574ae5588724fb01e16848cee3b4aca867a9439f9994ec7f6cbd`。

## 凭据与数据边界

1. **未发现真实微云 token 进入仓库。** 本地读取 skill 的 `~/.mcporter/mcporter.json`，仅在内存中提取 token，与 22 个可达提交、444 个唯一历史 blob、扫描时的 256 个工作区文件做精确/URL 编码形式比对，均无命中。受检发布二进制也不包含该 token。没有打印、保存或向网络发送它。
2. **通用扫描：** Gitleaks v8.24.3 对工作区零命中；可达 Git 历史有一处 GitHub PAT 规则命中，位于初始提交 `be96516` 的 `internal/source/codex/codex_test.go:452`，属于 `TestParserCanaryInExcludedRecordsNeverLeaks` 的专用 canary 夹具。上下文还包含 AWS 示例 key 和人工构造的其他 canary；不作为真实凭据泄露报告，也未在线验证其有效性。
3. 程序从环境变量、系统凭据库或本机 mcporter 配置读取令牌；不是将作者的令牌写死到源码。后台令牌保存到系统 Keychain，launchd plist 没有嵌入 token。
4. **本机额外风险：** 当前 mcporter 配置为 `0644`，其父目录为 `0755`，用户 home 为 `0750`。拥有目录遍历权限的其他主体可以读这个明文配置。这属于仓库外的本机凭据文件，开源本身不会把它上传；建议将该文件收紧到 `0600`，凭据目录收紧到 `0700`。本轮未修改其权限。
5. **同步库的密钥边界：** `cmd/session-memory/cmd_key.go:104–114` 把 vault 主密钥以 Base64 编码放入云端 key 文件，默认与加密数据处于同一个微云账号。能读取该同步库和 key 文件的人可以解密历史；这不是 Git 泄露，也不是独立于云账号的端到端密钥保护。README 已说明安全程度等同微云账号，发布说明应继续明确这一点。源码公开不应暴露随机生成的用户密钥。

这些检查不等于证明不存在任何未知凭据，也不覆盖不可达旧提交、别人已经复制的内容或用户自行附到 issue 的日志。当前最需要修的是下面两条运行时输出/发送路径。

## 新增问题

### S01 / P1：发布工具链仍固定到已有安全修复的 Go 1.26.0

- 位置：`go.mod:3`、`.github/workflows/release.yml:12–14`。
- 发布程序的 `go version -m` 和 `version --json` 均确认使用 Go 1.26.0，不只是本机工具版本旧。release 工作流从 go.mod 选择工具链。
- govulncheck v1.9.0 使用 2026-10-27 的 Go 官方漏洞数据库，对 `./cmd/session-memory` 报告 **27 个符号可达的标准库 advisory ID**。第三方模块也存在模块级提示，但没有在本轮作为符号可达的独立发现计数。
- 明确相关的客户端例子：[GO-2026-4918](https://pkg.go.dev/vuln/GO-2026-4918)，恶意 HTTP/2 SETTINGS 可导致客户端循环，Go 1.26.3 已修；官方另有 [GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809)，在部分系统信任库场景中拒绝已失信的中间证书，Go 1.26.10 / 1.27.3 已修。
- **27 不是已证明可利用的漏洞数量。** 静态接口分派会带来保守路径，一些告警涉及服务端或平台特定条件；本轮没有执行漏洞利用。具体调用路径保存在 results.json，发布前应升级到当前支持分支的安全补丁版本，重新构建及扫描。
- 注意：`govulncheck -json` 即使有 finding 也可能退出 0；本报告解析了 finding，未把退出码 0 当作无漏洞。

### S02 / P2：跨主机重定向会携带微云认证头

- 位置：`internal/transport/weiyun.go:64–65,82–84`。
- `WyHeader: mcp_token=…` 是自定义认证头。默认 `http.Client` 跟随重定向时，不会把这个自定义头作为 Authorization 那样的敏感头处理。
- 本地复现：原 MCP 服务返回 307 到不同 hostname，目标服务实际收到完整合成 token；连续三轮失败。测试没有访问真实微云。
- 前提是 MCP 端点或配置的代理返回这样的重定向；**没有证据表明真实腾讯服务已经这样转发或泄露。** 但代码“token 只发送到 URL”的约束不成立。
- 建议：认证 MCP 请求拒绝跨 origin、HTTPS 降级重定向，或对新请求清除认证头。下载链路需要 CDN 时另设策略，避免与认证请求共用宽松策略。
- 反例：`TestReleaseMCPRedirectDoesNotForwardCredential`。

### S03 / P2：错误携带签名下载 URL，服务端回显 token 时也不脱敏

- 位置：`internal/transport/weiyun.go:228–230`；相关 `failure:52–60`、`cmd/session-memory/weiyun.go:164–167`、`cmd/session-memory/app.go:57–74`。
- 下载遇到网络错误时，原样返回 `http.Client.Do` 的 `url.Error`，错误字符串包含完整下载 URL 和查询签名。另一路径直接使用服务端 JSON 错误消息，如果消息回显请求头，MCP token 也进入错误字符串。
- 两条路径都用合成凭据复现，分别连续失败三轮。第一条只需要网络失败；第二条需要服务端回显，不是说每个正常 HTTP 错误都会包含 token。
- 上层把错误写到 stderr 或 JSON；后台 plist 将 stderr 写到 autosync.log。用户分享日志/截图、助手将输出写进 issue 时，敏感信息可能被二次公开。日志的 typed slog 检查不覆盖这些直接 fmt 输出路径。
- 建议：在传输错误边界去掉 URL 查询与 userinfo，对已知 token 做精确脱敏，服务端错误只保留受控字段；保留错误类别以支持重试与额度识别。签名 URL 是否单独可下载还取决于有效期及 Cookie 等约束，报告不假定它总能独立授权。
- 反例：`TestReleaseNetworkErrorRedactsSignedDownloadURL`、`TestReleaseServerErrorRedactsMCPToken`。

### U01 / P2：skill 重新授权后 setup 仍使用保存的旧令牌

- 位置：`cmd/session-memory/weiyun.go:47–53,99–103`。
- 无显式环境变量时，Keychain 令牌始终优先于 skill 配置；`saveWeiyunToken` 仅在 Keychain 没有条目时导入 skill 令牌。
- 场景：第一次 setup 已保存令牌，之后令牌失效，用户按 skill 完成重新授权。skill 已更新，但再次 setup/后台同步仍选旧令牌，没有失败后的刷新路径。安装说明也没有可完成这个恢复流程的命令。
- 复现直接验证 setup 使用的选择/保存函数，提供旧 KeyStore 和新 mcporter 配置，刷新后仍选择旧值；没有使真实令牌过期或发起认证。
- 建议：提供明确的重新连接流程；认证失败后允许重新导入并验证新 token。切换账号前要核对 vault 身份，不能无条件以“较新的配置”静默换账号。
- 反例：`TestReleaseSkillReauthorizationRefreshesSavedToken`。

### U02 / P2：需要人工恢复的状态被承诺为自动完成

- 位置：`cmd/session-memory/cmd_onboard.go:61–70,147–152`；`INSTALL.md:26`。
- `relayCanFinishLater` 把 recovery_needed、need attention 与断网、额度耗尽放进同一分支。setup 输出 `PENDING: ... will finish by itself`，随后 `done:` 并返回 0；安装说明还要求助手不要打扰用户。
- 旧 journal 保护所拒绝的回滚、分叉冲突等状态可能必须人工处理，单纯等候不会解决。把拒绝破坏性恢复的正确行为包装成“已完成、会自动好”，会让用户误以为另一台已经安全同步。
- 复现用隔离 setup 和返回 recovery_needed 的模拟 relay；不是对真实云端恢复的测试。
- 建议：网络/额度可延迟；需要处理的会话应有独立 NEEDS_ACTION 状态、明确的会话计数和下一步。已连接的其他会话可继续同步，但结束提示不能承诺受阻会话自动恢复。
- 反例：`TestReleaseSetupDoesNotPromiseAutomaticRecovery`。

## 仍未解决的 9 项功能问题

本轮使用原审查脚本重新执行，9 个顶层断言均失败，1 个 live 守卫跳过，无构建失败或数据竞争。它们不是重复计算的新发现。

| 编号 | 优先级 | 用户可见影响 |
| --- | --- | --- |
| A05 | P1 | WorkBuddy 元数据更新后，未在本地修改的另一台也无法合法快进 |
| A06 | P1 | 关闭 IncludeSubagents 仍把子代理对话混入转换后的主会话 |
| A07 | P2 | 保存 binding 报冲突后，事实源留下第二个 active binding |
| A08 | P2 | 索引重建跳过损坏 revision 却报告成功 |
| A09 | P2 | 微云同路径、同长度但不同内容被误判为幂等成功 |
| A10 | P2 | 密钥部分发布失败后，备用账号缺失副本不重试 |
| A13 | P2 | 失败的 sync --json 输出两份 JSON，自动化解析失败 |
| A15 | P2 | WorkBuddy 旁路文件删除未触发新快照 |
| A16 | P2 | WAL 中已提交的软删除仍被 Discover 列出 |

详细触发步骤和源代码位置见 [原审查报告](../review-2026-10-29/README.md)。尤其 A05、A06 直接影响承诺的跨机与跨 Agent 接续，应在面向普通用户推广前关闭。

## 已完成的验证与体验结论

| 验证 | 结果与边界 |
| --- | --- |
| `go test -race -tags dev,failpoint -count=1 -timeout=12m -json ./...` | 437 个测试/子测试通过，3 个 live 用例跳过；26 个有测试的包通过，3 个包无测试；无 DATA RACE |
| 新增安全/体验反例，三轮 race | 5 个不同测试，每个失败 3 次，共 15 次；无构建错误或数据竞争 |
| 原延期问题反例 | 9 失败、1 live 跳过 |
| `go vet -tags dev,failpoint ./...` / `sh -n install.sh` / diff 检查 | 通过 |
| Gitleaks 当前工作区 / 可达历史 | 0 / 1；历史项为专用 canary 夹具 |
| 真实发布安装入口 | 临时目录安装、校验和 version/help 通过；未执行真实 setup 或启动 launchd |
| 本机 WorkBuddy 只读 probe | compatibility=ok；发现 98 个会话，抽查 20 个流的结构；未输出会话正文或执行写入 |

正常测试集涵盖隔离的首次接入、再次 setup、双设备同步、恢复、冲突、Keychain 替身与安装校验等路径。通过这些测试说明已有主流程有基础保障，**不能抵消已经复现的失败分支**。用户需要真正双机、真实云端、WorkBuddy GUI 展示及模型续写的验收，才可以宣称端到端体验完成；本轮的 live 用例未执行。

发布建议：先合入并发布上一轮 3 项修复，升级构建工具链；修复 S02/S03、A05/A06 和授权/恢复提示，再关闭其余已知问题。把凭据扫描、漏洞扫描、反例回归纳入发布检查；当前 release workflow 只构建和上传，未依赖 test workflow 完成。

## 复现

```bash
# 新增的五个行为断言；当前预期非零，修复后应全部通过
python3 doc/release-review-2026-10-29/run.py --count 3

# 既有九项；当前预期非零
python3 doc/review-2026-10-29/run_audit.py --race

# 正常测试矩阵
go test -race -tags dev,failpoint -count=1 -timeout=12m ./...
go vet -tags dev,failpoint ./...
```

查看脚本 summary 中的 build_errors 和 data_races；构建失败不算问题复现。本次未提交代码、创建 tag、发布 release、修改云端内容、轮换真实凭据或改动用户的原生会话。
