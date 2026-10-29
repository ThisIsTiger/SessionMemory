# 给 B 机助手的指令：清理旧版本并重新接入

请在 B 机（Intel Mac）上按顺序执行，不要跳步，不要永久删除任何数据。

## 第一部分：清理（先做这个，然后停下汇报）

1. 下载新版程序到临时位置，只用来执行清理，不运行 setup：

   ```
   curl -fsSL https://raw.githubusercontent.com/ThisIsTiger/SessionMemory/main/install.sh | SM_BIN_DIR="$HOME/sm-tmp-bin" SM_NO_SETUP=1 sh
   ```

2. 先看会做什么（不会改动任何东西）：

   ```
   "$HOME/sm-tmp-bin/session-memory" reset
   ```

3. 确认输出只涉及 SessionMemory 自己的状态后，执行：

   ```
   "$HOME/sm-tmp-bin/session-memory" reset --yes
   ```

   它会关闭后台同步，并把状态和设置改名备份（加 `.bak-时间` 后缀），不删除。
   如果提示 launchctl 相关限制（沙盒），请让用户在系统「终端」里运行同一条命令。

4. 移除旧程序，旧程序在 `~/.local/bin/session-memory`：

   ```
   mv ~/.local/bin/session-memory ~/.local/bin/session-memory.old
   ```

5. 汇报：`reset` 最后一行（以 `done: RESET` 开头）、`launchctl list | grep sessionmemory` 是否为空（沙盒里无法执行就如实说）。
   **到这里停下，等用户通知再继续。**

## 不要做的事

- 不要删除 Codex、WorkBuddy 里的任何会话（包括 Garmin、GEO 等从 A 机同步来的会话），也不要删除备份目录。
- 不要永久删除任何云端或本地数据。
- 不要退出用户正在用的应用，除非用户要求。
- 不要打印、复述任何微云令牌。

## 第二部分：重新接入（用户通知“云端已清理”之后再做）

把下面这句话作为任务执行：

> 请打开 https://github.com/ThisIsTiger/SessionMemory/blob/main/INSTALL.md ，按里面的说明帮我安装并开启会话同步。

期望最后一行是 `done: JOINED`（A 机已先建立同步库）。
如果出现 `NEEDS_USER`，按提示让用户双击桌面的「开启同步.command」。
完成后汇报：最后一行、`status` 的输出、`list` 里的会话数量。
