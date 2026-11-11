# 补丁说明:Go 服务与 gateway 的共享 Redis 口令改走 Secret(2026-11-06)

> **已应用,勿重打。** 2026-11-06 已用 `git apply` 打入主工作区 `D:\luyuan\xuanming-server-mmo-cpp`,
> 打入前后都做了核对:`apply --check` 通过;打完 `apply --check -R` 通过;逐文件比对,主工作区多出的改动与补丁逐行一致(36 个文件,0 个不符)。
> PROGRESS 条目已纯追加,交接 §9.5 第 5 步已同步改成用比对脚本。**状态:未编译、未测试,待 Codex 按交接 §9.5 验证。**
> 11-10 补记:代码已随 `1194c4d46` 入库;11-08 Codex 的命名审查复验让 shared、login、scene_manager、data_service 过了生产 build 与完整 `go vet`,
> 本补丁的单测、其余六个服务、部署契约与回归仍待跑。

- 补丁:`shared-redis-secret.patch`(36 个文件,新增 11 个;1650 行增、58 行删)。它是对主工作区 11-06 当时的未提交状态做三方合并后生成的,
  保留了同期其他会话的改动(发布轨道金丝雀、chat 世界频道、login / player-locator 的 RPC 段等)。
- 设计、取舍、验证命令:`docs/handoff/handoff-global-singletons-p0-20261015.md` §9。
- 换行:`git apply` 按 `.gitattributes`(`*.ps1 eol=crlf`)把 `tools/scripts/k8s_deploy.ps1` 整份写成了 CRLF,已按字节还原成打入前的 LF(内容不变)。
  契约测试文件本来就是 CRLF,保持不变。
- 打入前的版本:`shared-redis-secret-backup/`(26 个文件,含 `docs/PROGRESS.md`,`hashes.txt` 记了各自的 blob 哈希)。要撤回本补丁,在仓库根执行
  `git apply -R <本目录>\shared-redis-secret.patch`,不要直接拿备份覆盖:主工作区在打入之后还会被别的会话改。
- dev 档生成物不变的比对:`shared-redis-secret-dev-compare.ps1`。在仓库根先清掉 `MMORPG_*`,然后执行 `pwsh -NoProfile -File <本目录>\shared-redis-secret-dev-compare.ps1`。
  它比对的是「打入前的 k8s_deploy.ps1」与「它 + 本补丁」,不受其他会话之后改动的影响;通过 = 两组都打印 `IDENTICAL CONTENT`、退出码 0。
  2026-11-10 起按内容比(manifest 逐份逐字、份数相同,其余输出行一一对应,先后不计):部署脚本按普通哈希表遍历服务,先后每次运行都可能不同,
  11-06 第一次运行判 DIFFERENT 就是这个原因(内容一致,只是换了位置)。`-SavedOutputDir build/redis-secret-finish-20261106` 重判 11-06 留存的输出,
  `-ControlRun` 把改前那份多跑一次作对照;命令与结论见交接 §9.5 第 5 步。
- 2026-11-06 起本目录是 `docs/handoff/shared-redis-secret-20261106/`:原会话 worktree(`.claude/worktrees/interesting-cohen-c97262`)删除前,把补丁、比对脚本、本说明、PROGRESS 草稿,以及比对脚本要用的 `shared-redis-secret-backup/tools/scripts/k8s_deploy.ps1` 与 `hashes.txt` 挪了过来;其余备份文件没有保留(撤回只用 `git apply -R`,不用备份)。
