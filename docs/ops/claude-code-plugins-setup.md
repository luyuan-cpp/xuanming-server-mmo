# Claude Code 插件安装：ponytail（新电脑照做）

插件装在每台电脑的用户目录里，**不随仓库走**，换电脑要重新装一次。本文件记录安装步骤、核对方法和本机当前的安装信息。

核对日期：2026-11-19（本机安装记录与上游 README 对照）。

## 这是什么

- 插件 ID：`ponytail@ponytail`；来源：GitHub `DietrichGebert/ponytail`。
- 作用：写代码时的「懒资深开发」模式，要求只写解决问题所必需的代码。会话启动时自动开启，默认 full 级别。
- 附带的命令：`/ponytail`（切级别）、`/ponytail-review`、`/ponytail-audit`、`/ponytail-debt`、`/ponytail-gain`、`/ponytail-help`。
- 与仓库规范的关系：本仓库的规范入口是 [AGENTS.md](../../AGENTS.md)。ponytail 只影响代码的写法和体量，两者冲突时以 AGENTS.md 为准。

## 安装（每台新电脑做一次）

前提：已安装 Claude Code。任务条（band）功能需要 Claude Code 2.1.290 或更高版本，在终端或桌面应用里显示；VS Code 面板、云端会话和 `claude -p` 不显示。

1. 打开 Claude Code：终端里运行 `claude`，桌面应用的 Code 页也可以。
2. 在输入框里分两次发送下面两条命令，每条单独发送：

   ```text
   /plugin marketplace add DietrichGebert/ponytail
   ```

   ```text
   /plugin install ponytail@ponytail
   ```

   如果桌面应用里不支持 `/plugin`，就在终端的 `claude` 里执行同样两条。

3. 装完后新开一个会话。

## 安装后核对

1. 在 PowerShell 里运行下面这条（只读，不改任何文件）：

   ```powershell
   Select-String -Path "$env:USERPROFILE\.claude\plugins\installed_plugins.json","$env:USERPROFILE\.claude\settings.json" -Pattern "ponytail@ponytail"
   ```

   两个文件都要搜到：`installed_plugins.json` 里有 `ponytail@ponytail` 的安装记录；`settings.json` 的 `enabledPlugins` 里有 `"ponytail@ponytail": true`。
2. 新开的会话启动时，提示里应有 ponytail 模式和级别（本机看到的是 `PONYTAIL MODE ACTIVE — level: full`）。

## 本机当前安装记录

- 安装时间：2026-11-18（UTC 04:44）
- 版本：5.3.3，commit `96608e078b2d5ea607c5c3b8b5a54d1c1fba6846`
- 新电脑装到的通常是上游当时的最新版本。版本号以 `installed_plugins.json` 里的 `version` 与 `gitCommitSha` 为准。

## 更新与卸载

上游 README 没有写更新和卸载命令。请用 Claude Code 里的 `/plugin` 管理界面操作，具体菜单以你的 Claude Code 版本为准。

## 注意

- **只从 `DietrichGebert/ponytail`（GitHub）或 `@dietrichgebert/ponytail`（npm）安装。** 上游声明它不发布 `.exe` 或 `.dll` 文件，带这类文件的副本不是官方版本。
- ponytail 每条回复末尾会写「跳过了什么 / 没检查什么 / 有什么风险」，这是它的正常输出。
- 它会在代码里写 `shortcut: <限制>, <何时升级>` 注释，标记有意偷的懒；`/ponytail-debt` 把这些注释汇总成清单。这些注释是偏离原则时留下的记录（见 AGENTS.md §11），不要当作普通注释删掉。
- 上游 README 还写了 Codex、Cursor 等其他工具的安装方式。本文件只覆盖 Claude Code。

## 以后加别的插件

在本文件末尾加一节，写清插件 ID、来源仓库、安装命令和核对方法。
