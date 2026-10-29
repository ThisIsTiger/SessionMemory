# 给 AI 助手的安装说明（SessionMemory）

> 你是用户电脑上的 AI 助手（Codex、WorkBuddy、Claude Code 等）。用户把这个页面发给你，是想让多台电脑之间的对话会话自动同步。
> 请自己完成下面的步骤，不要让用户执行命令、做选择或理解技术细节；只在表里明确写了的时候才打扰用户。只支持 macOS（Linux 没有后台同步，Windows 不支持）。

## 步骤

1. 运行：

   ```sh
   curl -fsSL https://raw.githubusercontent.com/ThisIsTiger/SessionMemory/main/install.sh | sh
   ```

   它会下载程序、校验、装到 `~/.local/bin/session-memory`，然后自动运行 `setup`：连接微云、创建或加入同步库、纳入本机最近 60 天的会话、开启后台同步、上传一次。
   **`setup` 可能要几分钟，不要中断；如果被打断，重新运行 `~/.local/bin/session-memory setup`**（可以反复运行，不会重复）。

2. 如果出现 `operation not permitted`、keychain 或 launchctl 的报错，说明你在沙盒里：先在沙盒外重新运行 `~/.local/bin/session-memory setup`，再决定要不要打扰用户。

3. 看输出，按下表处理（一次输出里可能同时有 `PENDING` 或 `NEEDS_ACTION` 和最后的 `done:`）：

   | 输出 | 你要做的 |
   |------|---------|
   | `done: FIRST_COMPUTER` | 完成。这是微云里的第一台。告诉用户：其他电脑要用**同一个微信/QQ 登录**微云。如果用户说之前已经配过别的电脑，说明这次登录的是另一个微云账号：让用户改用原来的登录后再运行一次 `setup`。 |
   | `done: JOINED` | 完成。这台电脑已经加入微云里原有的同步库，什么都不用再做。 |
   | `done: ALREADY_CONNECTED` | 完成。这台电脑之前就接好了，这次只是检查并补做遗漏的部分。 |
   | `PENDING: connected; …` | 已连接，第一次上传会自己完成（微云当天额度用完，或网络不通）。不用重试，也不用打扰用户；照常往下走。 |
   | `NEEDS_ACTION: N sessions …` | 已连接，其他会话照常同步，但有 N 个会话**不会自己好**（同一个会话在两台电脑上都改过，或之前的安装被中断过，需要先检查）。这和 `PENDING` 不同：用白话告诉用户“有 N 个对话暂时没有同步，需要你决定怎么处理”，并把输出里的原文给用户看；可以运行输出里给的 `relay` 命令查看是哪几个。不要自己执行 `adopt`、`choose` 或删除任何文件，除非用户明确要求。最后的 `done:` 行只表示已经连接，不表示这几个会话已同步。 |
   | `NEEDS_WEIYUN` | 微云还没接通。告诉用户：「需要先连一下微云，我来帮你。」然后安装微云 skill（https://cdn.addon.tencentsuite.com/static/tencent-weiyun.zip），按它自带的 `references/auth.md` 引导用户在浏览器里用微信或 QQ 扫码授权；授权成功后重新运行 `setup`（用输出里给的完整路径）。 |
   | `NEEDS_USER: … double-click <文件>` | 你所在的环境不允许启动后台任务。告诉用户：「请双击桌面上的「开启同步.command」一次，弹出的窗口显示“完成”后关闭它。」 |
   | `NEEDS_USER: … In Terminal run: <命令>` | 先自己在沙盒外运行这条命令；做不到再请用户在“终端”里运行这一行。 |
   | 其他 `error:` | 把原文告诉用户，不要自己修改配置或删除任何文件。 |

4. 第二台、第三台电脑做同样的事：程序会自己发现微云里已有同步库并加入，不需要口令或文件。

## 告诉用户（用用户的语言，三句话以内）

- 已经接好了，这台电脑最近的对话会自动同步到你自己的微云（保存在你自己的微云里，安全程度和你的微云账号一样）。
- 在另一台电脑上也把这个页面发给 AI 助手，并用同一个微信/QQ 登录微云，两边就会自动互相同步。
- 如果用了 WorkBuddy：别的电脑来的新对话会在 WorkBuddy 用 Cmd+Q 完全退出后十分钟内自动装进去，之后重新打开 WorkBuddy 就能看到。

## 规则（不要违反）

- 不要打印、复述或让用户粘贴微云令牌（token）；令牌由微云 skill 或程序自己保管。
- `setup` 不会删除任何会话或文件。它会打开“把别的电脑的对话写进 WorkBuddy 列表”的功能（只在 WorkBuddy 完全退出时写入，写之前有备份，可以回滚）。
- 不要永久删除任何云端或本地数据。
- 不要执行 `adopt`、`choose`，也不要改 launchd 配置、`~/.zshrc`，除非用户明确要求。
- 不要替用户退出 WorkBuddy 或 Codex。
- 同步的是对话记录；项目里的代码文件不会被同步。
