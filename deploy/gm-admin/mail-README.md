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

## Scene 最终升级入口

邮件密钥单项草案已由 `scene-deploy.py` 的完整方案取代：最终一次只重建 Scene，同时接入新 GM 二进制、独立 `MMORPG_GM_PLAYER_COMMAND_SECRET`、现有邮件附件密钥及 `127.0.0.1:2355 → 50001`。`20001` 是 RPC0，不是 gRPC。当前 etcd 注册核实 nodeId=1。准备、构建、审阅和执行分开；prepare 已完成，正式 Linux 构建单独留回执，尚未进行 stage 或重启。

**当前不能直接 stage/apply。** 用户已明确选择先完成 GM 代码和构建，Scene 等装备、技能统一验收后再重建。旧手绘表目录缺少新节点必读的 `equipartifact.json`、`equipv1affix.json`，直接挂载会在 `File2String` 中 `LOG_FATAL`；本轮不修改表快照、manifest 或 Compose。新增只读覆盖门禁会在 stage/apply 写配置之前拒绝缺表。成功构建批次的审阅清单是 `build/verification/gm-admin-20261117/scene/table-compatibility-review-retry.json`，原 `table-compatibility-review.json` 保留为首次调查证据；两批表内容相同，重试报告重新绑定成功批次的完整源码摘要和冻结表路径。下面是后续统一验收后的计划入口，不是本轮发布指令。表覆盖通过本身也不等于装备/技能已验收。

正式 Linux 重试已通过，结果位于 `scene/cpp-build-retry/result.json`，gate、scene、battle 来自同一镜像；构建后的 SDK 和全量源码守卫均通过。`scene/build-verification.json` 进一步核对三节点和分离调试符号共六项产物摘要、同批表审阅及原运行栈身份/状态；没有 stage、重建 Scene 或玩家资产操作。首次 `scene/cpp-build/result.json` 因构建期间并行装备源码变化而失败，原失败证据完整保留，不能用于发布。

```powershell
python -X utf8 -B deploy/gm-admin/scene-deploy.py prepare
# 本轮构建已完成；后续源码变化须另建输出目录，重新执行正式构建器。
# 正式构建器仍统一封存、验证 SDK 并构建 gate/scene/battle；只交付 Scene 容器。
python -X utf8 -B deploy/gm-admin/scene-deploy.py stage --build-result build/verification/gm-admin-20261117/scene/cpp-build-retry/result.json
# 审阅 scene/review.patch、promotion.json、plan.json 后，由主线释放执行。
python -X utf8 -B deploy/gm-admin/scene-deploy.py apply --execute
```

新材料位于 `build/verification/gm-admin-20261117/scene/`。`stage` 必须核对同一正式构建批次、镜像 ID、当前源码全量快照、镜像内产物摘要、SDK、原运行合同及封存的手绘场景表。`apply` 持原启动锁，要求当前完整合同和状态仍等于准备时的原件；保留原四份 Compose 路径，只给原恢复 overlay 的 Scene 单项 image 前移，其余镜像和卷保持原身份。不执行全栈重建、迁移、注册、发信或玩家 GM。

现役 `start_docker_debug.py` 通过窄 helper 校验前移回执：原私有 `.env` 中的 `GM_SCENE_PROMOTION_SHA256` 必须与 `promotion.json` 完全匹配；原历史证据继续逐字节核验，另核源码/镜像构建链、原 Scene 身份、仅 Scene 的 overlay 差异、固定 loopback 端口以及其余所有原容器签名。没有显式摘要时仍执行原规则，准备文件本身不能启用新镜像。

只有实际替换成功、正式 preflight 与源码/产物核验通过、etcd nodeId/gRPC 地址一致、两端运行密钥匹配后，才写 `ready.json` 和私有摘要。`run_local.py` 清除继承来的四项游戏写通道变量，仅在完整回执和实时身份核验通过后读取 `scene/private/gm-runtime.env`；缺少、陈旧或篡改回执均保持禁用。就绪只证明通道部署，不证明已做玩家命令或附件领取，回执明确记录这两项未实测。

玩家查询另由主线的 `rpc-bridge.py` 独立验证并启用：`127.0.0.1:2356 → data-service:9000`、`127.0.0.1:2357 → player-locator:53200`，不重启原服务。原 `2347/2349` 是 Delve 调试器端口，不能当业务 gRPC。应用两项目标默认留空，启动器只能从桥的真实 `verify_ready()` 取得它们；读通道可先启用，Scene 写通道的最终回执还必须绑定同一份已验证桥回执。

## 早期邮件单项草案（保留证据，不再作为执行入口）

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
