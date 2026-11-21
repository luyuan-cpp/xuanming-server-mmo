# JeecgBoot GM 扩展

独立的 Vue 3 管理网页和 Java 后台，复用 JeecgBoot 的管理员登录、角色与接口权限。游戏数据只经现有服务协议读取；接入个人邮件多附件发放、全服纯文本系统公告邮件、三段式幂等批量活动发奖，以及既有七项玩家 GM：增加/扣除货币、封禁/解禁货币获取、设置等级、发放宝宝、发放物品。设计、状态和边界见 [GM 设计](../../docs/design/jeecg-gm-administration.md)。

## 准备与检查

在仓库根目录执行（需要 Git、Python、JDK 17+、Maven、Node/pnpm、Docker）：

```powershell
python java/gm_admin/prepare.py
mvn -f java/gm_admin/pom.xml -DskipTests=false test
mvn -s build/gm-admin-runtime/maven-settings.xml -f build/gm-admin-upstream/jeecg-boot/pom.xml -pl jeecg-module-system/jeecg-system-start -am '-Dmaven.compiler.proc=full' -DskipTests package
pnpm --dir build/gm-admin-upstream/jeecgboot-vue3 install --frozen-lockfile --fetch-timeout=300000
```

扩展的鉴权、幂等、审计、协议签名和 gRPC 回环测试独立运行。原生 JeecgBoot 打包跳过其依赖演示库和外部服务的测试，不代表这些演示功能已验收。

准备脚本校验上游 v3.9.6 的固定提交，生成自己的协议描述并复制扩展。第三方源码、依赖、数据库初始化 SQL、运行日志和本机凭据都在 Git 忽略的 `build/` 下；不手动修改游戏的生成代码。再次准备会覆盖自有扩展对应的上游文件，应在 `java/gm_admin` 修改。

## 本机运行

日常和游戏一起启动，在仓库根目录执行：

```powershell
python -X utf8 -B tools/scripts/start_docker_debug.py --execute
```

该入口先检查游戏与 GM 的既有安装，再恢复原 GM MySQL/Redis 和只读桥，依次启动 Java 后台与网页并等待 HTTP 就绪。已运行的健康服务不会重启；去掉 `--execute` 只预检。它不安装依赖、不构建、不新建管理库或重置口令，也不会部署 Scene 或启用未验收的写通道。运行材料或原容器缺失时保留现场并报错，不能用新空库代替。

独立准备和分项维护仍可使用：

```powershell
python java/gm_admin/run_local.py infra
python java/gm_admin/run_local.py backend
python java/gm_admin/run_local.py frontend
python java/gm_admin/run_local.py status
```

网页为 `http://127.0.0.1:3101`，Java 为 `http://127.0.0.1:18090/jeecg-boot`。独立 MySQL/Redis 端口为 13391/16391，不复用游戏的管理库。初始化账号为 `admin`，随机口令由启动脚本保存到 `build/gm-admin-runtime/private.json` 的 `GM_BOOTSTRAP_PASSWORD`，不要提交或贴到公共日志。只在首次数据库初始化时设置密码；后续启动不会重置账号。

数据与定位服务只开放在原 Docker 网络内。`python deploy/gm-admin/rpc-bridge.py deploy --execute` 单独部署固定两路转发，宿主回环 2356/2357 分别连接真实 DataService 9000 和 PlayerLocator 53200，不重启原服务；`verify` 检查就绪凭据。2347/2349 是 Delve 调试端口，不能用作游戏 RPC。读服务默认地址为空；启动器只加载经过源码、注册信息、上游与转发容器身份检查的目标。

启动器仅在 `build/verification/gm-admin-20261117/scene/ready.json`、源码/镜像凭据、私有配置摘要及实时容器身份全部匹配时，加载专用 GM 与邮件通道。缺少或过期的凭据会保持写通道禁用；继承的环境变量不能跳过该验收。独立邮件节点的部署材料在 `deploy/gm-admin/mail-*`。投递成功只表示邮件已生成，实际到账取决于玩家领取及场景服务的资产验签配置。不要对已有玩家发放资产来冒充无副作用的测试。

所有 GM 入口要求原生认证主体。查询、个人邮件与系统/活动邮件权限为 `gm:zone:read`、`gm:player:read`、`gm:mail:prepare`、`gm:mail:send`、`gm:system-mail:prepare`、`gm:system-mail:send`、`gm:audit:read`；玩家命令另有 `gm:currency:add`、`gm:currency:deduct`、`gm:currency:block`、`gm:currency:unblock`、`gm:player:level`、`gm:pet:grant`、`gm:item:grant`。后端在创建及发送时分别检查。初始管理员获得全部权限，其他运营账号通过原生用户和角色管理分配。管理员和玩家账号相互独立。

## 冻结命令

草稿先落管理库，再核对目标、参数和原因；发送只提交命令号。

- 个人邮件（`MAIL`）支持按权威 `CurrencyType` 组合发放最多 4 种不重复货币（银两/灵石/绑定灵石）与最多 16 种不重复物品（总数 ≤ 4096，兼容既有单币种/单物品字段）；过期草稿在 `FOR UPDATE` 事务内原子拦截；超时保留原命令，核对后可显式重试同号，复用服务端持久幂等。`DELIVERED` 不等于附件已领取。
- 全服系统公告（`SYSTEM_MAIL`）走 `mailpb.MailAdmin/SendSystemMail` 纯文本广播，零玩家扩散写，禁止携带附件或单人/帮会字段；批量活动发奖（`MAIL_CAMPAIGN`）在建草稿时将收件人去重、按 uint64 升序每 1000 人切分并计算与 `go/mail/internal/data/campaign.go` 逐字节一致的 SHA-256 `manifestHash`（网页单次上限 10,000 人），发送时按三段式幂等协议 `SendSystemMail → ImportMailRecipients → SealMailCampaign` 执行，建活动后任一故障均保留 `UNKNOWN` 供原单幂等续传，HTTP 列表/详情仅返回前 20 条样例与汇总计数以控制响应体积。
- 玩家 GM 仅允许开发或测试服的在线玩家，通过 `PlayerLocator/GetSession(include_placement=true)` 原子读取 `scene_manager` 权威落点 `(zone_id, node_id, scene_id)`，草稿有效期五分钟（在数据库 `FOR UPDATE` 事务内原子拦截过期）。命令进入 `SENDING` 后禁止重新发送；`UNKNOWN` 需核对玩家与服务日志，不能重新生成相同操作来重试。`EXECUTED` 表示同步域操作完成，持久化仍走游戏原有流程。
- 币种来自游戏协议目录；等级为 1–85，直接 GM 发物最多 99 个。金额和玩家 ID 使用十进制字符串。
- 审计列表支持按玩家 ID 与状态筛选及分页（`page` / `pageSize`），核对弹窗支持按命令 ID 精确刷新原命令状态并查看完整 `gm_command_audit` 状态变更轨迹（`/gm/mail/commands/{id}`、`/gm/system-mail/commands/{id}`、`/gm/mail-campaigns/commands/{id}`、`/gm/player-commands/{id}` 及各自 `/audit`）。

生产部署另行配置域名、HTTPS、服务间 TLS、`GM_HTTP_ADDRESS` 与 `GM_REDIS_HOST`（支持优雅停机与 `/actuator/health/liveness`、`/actuator/health/readiness` K8s 探针）；水平扩展场景节点时，除静态映射 `GM_SCENE_TARGETS_JSON`（支持 `"<zoneId>/<nodeId>"` 与 `"<nodeId>"`）外，还可配置 `GM_SCENE_ETCD_URL`（仅允许 HTTPS 或本机回环 HTTP）按 `(zoneId, nodeId)` 动态发现 etcd `SceneNodeService.rpc/` 下的场景 `grpcEndpoint`；多副本 GM 管理后端支持并发安全启动迁移（`gm_install` 版本 1–6，含查询/审计复合索引与系统邮件独立权限）。

## Scene 部署与通道状态

本机 11-18 重建完成后，`deploy/debug/compose.game.yml` 与 `deploy/gm-admin/scene-deploy.py` 已支持在 `x-xuanming-rebuild` 之上完成 GM Scene 提升并保持重启自愈（`start_docker_debug.py --execute` 自动恢复 GM MySQL/Redis、只读桥、独立 Mail、已封印 Scene 与 GM 前后端）；`probe_scene.py --execute` 已验证无副作用探针通过。

如需重新执行 Scene GM 提升：`python deploy/gm-admin/scene-deploy.py prepare` 生成私有配置、现役身份快照和 `scene/plan.json`；`stage`（或重建后自动对齐现役镜像）生成精确配置差异；显式执行 `apply --execute` 替换 `scene` 容器并校验其余容器身份不变。完整就绪凭据通过后重启 GM Java 后台，才会启用写通道。
