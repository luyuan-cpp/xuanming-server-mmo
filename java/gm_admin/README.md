# JeecgBoot GM 扩展

独立的 Vue 3 管理网页和 Java 后台，复用 JeecgBoot 的管理员登录、角色与接口权限。游戏数据只经现有服务协议读取；接入个人邮件发放，以及既有七项玩家 GM：增加/扣除货币、封禁/解禁货币获取、设置等级、发放宝宝、发放物品。设计、状态和边界见 [GM 设计](../../docs/design/jeecg-gm-administration.md)。

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

```powershell
python java/gm_admin/run_local.py infra
python java/gm_admin/run_local.py backend
python java/gm_admin/run_local.py frontend
python java/gm_admin/run_local.py status
```

网页为 `http://127.0.0.1:3101`，Java 为 `http://127.0.0.1:18090/jeecg-boot`。独立 MySQL/Redis 端口为 13391/16391，不复用游戏的管理库。初始化账号为 `admin`，随机口令由启动脚本保存到 `build/gm-admin-runtime/private.json` 的 `GM_BOOTSTRAP_PASSWORD`，不要提交或贴到公共日志。只在首次数据库初始化时设置密码；后续启动不会重置账号。

数据与定位服务只开放在原 Docker 网络内。`python deploy/gm-admin/rpc-bridge.py deploy --execute` 单独部署固定两路转发，宿主回环 2356/2357 分别连接真实 DataService 9000 和 PlayerLocator 53200，不重启原服务；`verify` 检查就绪凭据。2347/2349 是 Delve 调试端口，不能用作游戏 RPC。读服务默认地址为空；启动器只加载经过源码、注册信息、上游与转发容器身份检查的目标。

启动器仅在 `build/verification/gm-admin-20261117/scene/ready.json`、源码/镜像凭据、私有配置摘要及实时容器身份全部匹配时，加载专用 GM 与邮件通道。缺少或过期的凭据会保持写通道禁用；继承的环境变量不能跳过该验收。独立邮件节点的部署材料在 `deploy/gm-admin/mail-*`。投递成功只表示邮件已生成，实际到账取决于玩家领取及场景服务的资产验签配置。不要对已有玩家发放资产来冒充无副作用的测试。

所有 GM 入口要求原生认证主体。查询与邮件权限为 `gm:zone:read`、`gm:player:read`、`gm:mail:prepare`、`gm:mail:send`、`gm:audit:read`；玩家命令另有 `gm:currency:add`、`gm:currency:deduct`、`gm:currency:block`、`gm:currency:unblock`、`gm:player:level`、`gm:pet:grant`、`gm:item:grant`。后端在创建及发送时分别检查。初始管理员获得全部权限，其他运营账号通过原生用户和角色管理分配。管理员和玩家账号相互独立。

## 冻结命令

草稿先落管理库，再核对目标、参数和原因；发送只提交命令号。

- 邮件超时保留原命令，核对后可显式重试同号，复用服务端持久幂等。`DELIVERED` 不等于附件已领取。
- 玩家 GM 仅允许开发或测试服的在线玩家，草稿有效期五分钟。命令进入 `SENDING` 后禁止重新发送；`UNKNOWN` 需核对玩家与服务日志，不能重新生成相同操作来重试。`EXECUTED` 表示同步域操作完成，持久化仍走游戏原有流程。
- 币种来自游戏协议目录；等级为 1–85，直接 GM 发物最多 99 个。金额和玩家 ID 使用十进制字符串。

生产部署另行配置域名、HTTPS 和服务间 TLS；当前脚本仅监听本机。

## Scene 最后部署

当前网页已完成原生登录、退出重登、十二项权限展示和七项操作目录验收；读服务转发已就绪。用户已明确选择先完成 GM 代码和构建，Scene 等装备、技能统一验收后再重建，因此玩家命令和邮件写通道保持禁用。这不影响通过浏览器访问管理网页。

现役手绘表挂载缺少新节点必读的 `equipartifact.json` 和 `equipv1affix.json`，且其余装备、技能表也有变化。本轮不执行下面的 stage/apply，不更新表快照、manifest 或 Compose；后续须先统一验收并准备兼容表快照。部署工具会在写配置前拒绝缺表。差异记录在 `build/verification/gm-admin-20261117/scene/table-compatibility-review.json`。

`python deploy/gm-admin/scene-deploy.py prepare` 只生成私有配置、现役身份快照和 `scene/plan.json`，不构建或重启。按计划完成正式 C++ 镜像构建，再运行 `stage --build-result <构建凭据>`，生成精确配置差异。只有最后显式执行 `apply --execute` 才会替换一个 scene 容器并断开其现有连接；其他 16 个服务必须保持原身份和状态。执行前后均检查四份 Compose 来源、SDK、源码、镜像与配置，检查失败不自动重试。完整就绪凭据通过后重启 GM Java 后台，才会启用写通道。
