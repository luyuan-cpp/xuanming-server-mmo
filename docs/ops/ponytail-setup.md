# ponytail 安装：Claude Code、Codex、Gemini（新电脑照做）

ponytail 装在每台电脑的工具配置或用户目录里，**不随仓库走**。换电脑要按下面各工具的步骤重装一次。

核对日期：2026-11-19。安装命令来自上游仓库的 INSTALL.md 与 README；「本机状态」是当天核对的结果。

## 这是什么

- 上游：`github.com/DietrichGebert/ponytail`，当前版本 5.3.3。
- 作用：写代码时的「懒资深开发」模式，要求只写解决问题所需的代码。
- Claude Code 和 Codex 在会话启动时自动开启，默认 full 级别。Gemini CLI 按上游说明每次会话加载同一套规则；Gemini 桌面版不支持插件，见下文。
- 命令：`/ponytail`（切级别：`lite`、`full`、`ultra`、`off`）、`/ponytail-review`、`/ponytail-audit`、`/ponytail-debt`、`/ponytail-gain`、`/ponytail-help`。Codex 里要写成 `$ponytail:ponytail-review` 这样调用。
- 与仓库规范的关系：规范入口是 [AGENTS.md](../../AGENTS.md)。ponytail 只影响代码的写法和体量，两者冲突时以 AGENTS.md 为准。

## 先决条件

- **Node.js 要在 PATH 上。** Claude Code 和 Codex 的生命周期钩子用 node 运行。没有 node 时技能仍可用，但每次钩子调用都会报 `node: command not found`。
- **只从官方来源安装**：GitHub 的 `DietrichGebert/ponytail`，或 npm 的 `@dietrichgebert/ponytail`。上游声明它不发布 `.exe` 或 `.dll` 文件，带这类文件的副本不是官方版本。

## Claude Code

在终端运行 `claude`，或用桌面应用的 Code 页。分两次发送下面两条命令，每条单独发送：

```text
/plugin marketplace add DietrichGebert/ponytail
```

```text
/plugin install ponytail@ponytail
```

桌面应用也可以点输入框旁的 **+**，选择 **Plugins** → **Add plugin** 来安装（上游 INSTALL.md 的说明）。

任务条（band）需要 Claude Code 2.1.290 或更高版本，只在终端和桌面应用里显示。VS Code 面板、云端会话和 `claude -p` 不显示。

**核对：** `%USERPROFILE%\.claude\plugins\installed_plugins.json` 里有 `ponytail@ponytail`；`%USERPROFILE%\.claude\settings.json` 的 `enabledPlugins` 里有 `"ponytail@ponytail": true`。新开会话，启动提示里应有 `PONYTAIL MODE ACTIVE — level: full`。

## Codex

终端里要能直接运行 `codex`。然后执行：

```bash
codex plugin marketplace add DietrichGebert/ponytail
codex plugin add ponytail@ponytail
```

装完后运行 `codex`，打开 `/hooks`，检查并信任 ponytail 的生命周期钩子，然后新开一个线程。Codex 桌面应用装完后重启即可识别同一个插件。

**核对：** `%USERPROFILE%\.codex\config.toml` 里有 `[plugins."ponytail@ponytail"]` 且 `enabled = true`，并且有 `[hooks.state."ponytail@ponytail:…"]` 条目，其中带 `trusted_hash`。`%USERPROFILE%\.codex\plugins\cache\ponytail\ponytail\` 下面有版本目录。

## Gemini

### Gemini CLI（终端）

先装 Gemini CLI，官方方式是 `npm install -g @google/gemini-cli`（Node 要在 PATH 上）。然后执行：

```bash
gemini extensions install https://github.com/DietrichGebert/ponytail
```

装完后启动 Gemini CLI，输入 `/ponytail-help`。能看到帮助，说明命令已注册。

如果 GitHub 地址安装报 `Use of "GIT_CONFIG_SYSTEM" is not permitted without enabling allowUnsafeConfigPaths`，说明 Gemini CLI 0.70.0 的 git 安全检查拦下了克隆。本机就是这样。这时不要开启 `allowUnsafeConfigPaths`，它会放宽整个 Gemini CLI 的 git 检查，不推荐。改用本地克隆安装：

```bash
git clone https://github.com/DietrichGebert/ponytail.git <本地目录>
gemini extensions install <本地目录> --consent --skip-settings
```

安装时会问 `Do you trust this folder…`，要输入 `y` 才会继续。信任只针对这一个文件夹，记录在 `~/.gemini/trustedFolders.json`。

上游的 Gemini 版有意不带生命周期钩子，所以没有 Claude Code / Codex 那种启动提示。规则靠每次会话都加载的常驻上下文生效。

### Gemini 桌面版（不能装插件）

本机的 Gemini 桌面版是 1.18.0（`%LOCALAPPDATA%\Google\Gemini\app-1.18.0\`），是 Electron 外壳。我检查了它的主进程和预加载脚本，里面没有插件、技能、`AGENTS.md` 或 `GEMINI.md` 的加载代码；程序包里也没有附带 Gemini CLI。所以上面的 `gemini extensions install` 装不进桌面版。

可行的替代做法：在 Gemini 账号里建一个 Gem（自定义指令），把 `skills/ponytail/SKILL.md` 的内容贴进去。这是账号设置，要在 Gemini 里手动操作。它只带规则，没有 `/ponytail` 命令和级别切换。

## 更新与卸载

上游 INSTALL.md 的卸载命令：

| 工具 | 卸载命令 |
|------|----------|
| Claude Code | `/plugin remove ponytail` |
| Codex | `codex plugin remove ponytail` |

Gemini CLI 卸载：`gemini extensions uninstall ponytail`。信任记录写在 `~/.gemini/trustedFolders.json`，卸载后可以检查这个文件是否还留着那一行。

卸载前先运行上游的清理脚本 `node scripts/uninstall.js`。它会清掉 `~/.claude/.ponytail-active` 等状态文件。这个脚本本身是插件的文件，所以要在卸载插件之前运行，或者在另一份克隆里运行。

上游没有单独的更新命令。Claude Code 用 `/plugin` 管理界面操作，具体以你的版本为准。Codex 装的是本地克隆时，要先在克隆里更新（见下方「本机状态」）。Gemini 扩展也是从本地克隆装的，同样不会自动跟随 GitHub。要更新：先在克隆目录里 `git pull`，再运行 `gemini extensions update ponytail`（这条命令还没在本机验证）。

## 本机状态（2026-11-19 核对）

| 工具 | 状态 | 细节 |
|------|------|------|
| Claude Code | 已装 | 5.3.3，commit `96608e078b2d5ea607c5c3b8b5a54d1c1fba6846`，2026-11-18 安装，`enabledPlugins` 为 true |
| Codex | 已装并启用 | 5.3.3，同一个 commit，钩子已信任。marketplace 指向本地克隆 `D:\luyuan\tools\ponytail-source`。克隆里有一处未提交改动：`.agents/plugins/marketplace.json` 把插件来源从 GitHub 改成了本地路径。所以它不会自动跟随 GitHub 更新。新电脑按上面的 GitHub 命令装即可，不需要这个改动 |
| Gemini 桌面版 | 已装，不支持插件 | 1.18.0，Electron 外壳。主进程和预加载脚本里没有插件、技能或规则文件的加载代码，ponytail 装不进去 |
| Gemini CLI | 已装 0.70.0，ponytail 扩展已装并启用 | `npm install -g @google/gemini-cli` 装在 Node 目录下，`gemini` 已在 PATH 上。扩展来源是本地克隆 `%USERPROFILE%\.claude\plugins\marketplaces\ponytail`（commit `96608e0`），装在 `%USERPROFILE%\.gemini\extensions\ponytail`，`gemini extensions list` 能看到。GitHub 地址安装被 `allowUnsafeConfigPaths` 检查拦下，所以走的是本地克隆。信任记录在 `~/.gemini/trustedFolders.json`。命令还没有实际运行过，那需要先登录 Gemini |

本机 node 已在 PATH 上（v26.13.1）。

## 注意

- ponytail 每条回复末尾会写「跳过了什么 / 没检查什么 / 有什么风险」，这是它的正常输出。
- 它会在代码里写 `shortcut: <限制>, <何时升级>` 注释，标记有意偷的懒；`/ponytail-debt` 把这些注释汇总成清单。这些注释是偏离原则时留下的记录（见 [AGENTS.md](../../AGENTS.md) §11），不要当普通注释删掉。
- 上游 INSTALL.md 还写了 Cursor、Copilot、OpenCode 等工具的安装方式。本文件只覆盖 Claude Code、Codex 和 Gemini（CLI 与桌面版）。

## 以后加别的工具或插件

在本文件末尾加一节，写清工具名、安装命令、核对方法和卸载命令。
