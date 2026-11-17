# 本机独立 GM 邮件服务

本批只在原游戏栈旁增加 `xuanming-gm-mail` 项目。游戏依赖沿用 `xuanming-debug-current_default`；邮件 gRPC 仅映射 `127.0.0.1:2354`，健康检查仅映射 `127.0.0.1:9244/healthz`。

`mail-build.py` 从当前 Go 源码编译 Linux mail，封存配置表与前后源码摘要。运行层复用本机已存在且核过 SHA-256 的 mail 镜像；本地固定标签用于避免 Docker 对已安装镜像重复进行远程元数据查询。业务二进制由本批覆盖，版本见 `/app/BUILD_INFO` 和启动日志；运行层继承的旧 OCI revision 标签不代表本批业务二进制版本。

首次构建/准备/启动：

```powershell
python deploy/gm-admin/mail-build.py
pwsh -NoProfile -File deploy/gm-admin/mail-prepare.ps1
docker compose --env-file build/verification/gm-admin-20261117/mail/private/compose.env -f deploy/gm-admin/mail-compose.yaml config --quiet
docker compose --env-file build/verification/gm-admin-20261117/mail/private/compose.env -f deploy/gm-admin/mail-compose.yaml up -d mail
pwsh -NoProfile -File deploy/gm-admin/mail-verify.ps1
```

已有构建目录不覆盖。新尝试用 `--attempt 2` 等新编号；仅在源码、已编译前后快照和封存表都一致时，才可对同一次构建加 `--resume-packaging`。2026-11-17 第二轮已经成功并完成首次启动，不要再照抄首次启动命令。

SQL 沿用 `sql_boot_catalog_docker.ps1`、正式 `mail -migrate` 和 `writer_authority register`：

- 只初始化新的 `mmorpg_mail`；不执行全局 prepare，不迁移其他库，不清库或清卷。
- 原 MySQL 的物理 authority 原件和锁资格声明只读复制至邮件自己的私有卷。
- 邮件独占随机 registrar/maintenance 账号，避免重新设置原栈固定 operator 的口令；运行账号只按正式模板取得邮件表权限。
- 常驻节点以 10001 运行，SQL DSN 由本次 writer grant 绑定并经原 `sql-writer-entry.sh` 加载。授权只消费一次，不能直接 `docker restart`；后续重建必须连新的 mail-register 一起规划，不能在旧 mail 仍运行时重写其 grant 卷。
- 角色围栏密钥继承现有 data-service 的运行配置。`mail`、`mail_asset_op` 号段由真实 data-service 分配，没有替代号源。

私有配置位于 git 忽略且 NTFS ACL 收紧的 `build/verification/gm-admin-20261117/mail/private/`：

- `admin.env`：`MAIL_PRODUCER_SECRET_ADMIN`、`GM_MAIL_TARGET`，供 GM 启动器在内存读取；按 Spring 配置映射为 `GM_MAIL_ADMIN_SECRET`。
- `scene.env`：`MMORPG_ASSET_OP_SECRET_MAIL`，值与当前邮件进程完全相同。
- `runtime.env`、`sql.env`、`migrate.env`：各容器所需配置。不能输出 compose 完整解析结果、Docker 完整 inspect 或这些文件内容。

证据在 `image.json`、`verification.json`、`mail-*.log`。健康和 etcd 注册通过，原 17 个游戏容器的 ID、镜像、启动时间未变；`mail_message`、`mail_asset_op` 均为 0。未发玩家邮件，未改玩家资产。`SendPersonalMail` 成功只代表投递，资产在玩家领取附件后发放；当前 Scene 未配置附件验签密钥，不能把服务健康当成领取成功。

## 待主线审阅的 Scene 最小变更

只影响 **1 个 Scene 容器**，会断开其中当前在线玩家。`scene-key-presence.json` 证明 `Config.Env` 和实际 `/proc/1/environ` 均无邮件资产密钥；进程直接执行 `/app/bin/scene`，没有入口脚本延后注入。运行代码读取并缓存进程环境，修改文件不能热生效。

只读入口复用现有 `start_docker_debug.preflight()`，保留原 17 服务身份、卷、挂载、已批准恢复原件和环境摘要检查：

```powershell
python deploy/gm-admin/mail-scene-preflight.py
git apply --check deploy/gm-admin/mail-scene.compose.patch
```

输出 `scene-runtime-before.json` 与 `scene-restart-plan.json`；本批已执行成功，未应用变更。计划记录私有 `scene.env` 与运行 mail 密钥相同、原私有 `.env` 与当前 Scene 均无同名键。

当前恢复门禁严格要求原栈四份 Compose 来源，直接追加 `mail-scene.override.yaml` 会导致正式预检拒绝。override 只用于查看差异，不能直接应用。最小完整接入步骤：

1. 主线取得重建当前 Scene 的确认后，在现有 startup lock 内重做正式 preflight，要求 contract 与 `scene-runtime-before.json` 完全一致；任一身份或输入变化都重新生成审阅计划。
2. 应用 `mail-scene.compose.patch` 的唯一环境声明；仅向原私有 `build/docker-debug-current/.env` 新增同名键，值从 `private/scene.env` 内存读取。若已有不同值则拒绝，不覆盖、不回显。其他字节保持不变；原字节备份仅存本任务私有目录，以便启动前失败时恢复。
3. 执行计划内 `applyCommandAfterReviewedConfigChanges` 参数数组。仍使用原四份 Compose，只指定 `--no-deps --no-build --pull never --force-recreate scene`；禁止全栈 up。
4. 新 Scene 启动后重跑正式 preflight，确认同一镜像、挂载、网络、端口，只有 Scene 身份改变，其余 16 个运行服务完全未变。仅在内存比较实际进程环境密钥与 mail 密钥。保留新回执后，再准备明确测试角色的附件领取验收；本任务不发该邮件。

若重建失败，保留身份和日志，先判断进程是否替换再制定恢复动作；不能自动全栈重试或重跑注册/迁移。Scene 重建成功不等于附件到账。
