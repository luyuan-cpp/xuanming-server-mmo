# JeecgBoot GM 扩展

独立的 Vue 3 管理网页和 Java 后台，复用 JeecgBoot 的管理员登录、角色与接口权限。游戏数据只经现有服务协议读取；发放通过个人邮件进入资产系统。设计、状态和边界见 [GM 设计](../../docs/design/jeecg-gm-administration.md)。

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

邮件签名和地址必须显式配置；未配置时页面和后端都拒绝发放。独立邮件节点的部署材料在 `deploy/gm-admin/mail-*`。投递成功只表示邮件已生成，实际到账取决于玩家领取及场景服务的资产验签配置。不要对已有玩家发放资产来冒充无副作用的测试。

所有 GM 入口要求原生认证主体。五个权限分别为 `gm:zone:read`、`gm:player:read`、`gm:mail:prepare`、`gm:mail:send`、`gm:audit:read`；后端逐次检查。初始管理员获得全部权限，其他运营账号通过原生用户和角色管理分配。管理员和玩家账号相互独立。

## 冻结命令

草稿先落管理库，再核对目标、附件和原因；发送只提交命令号。超时保留原命令并显示不确定结果，核对后显式重试同号，不重新生成发放。`DELIVERED` 不等于附件已领取。生产部署另行配置域名、HTTPS 和服务间 TLS；当前脚本仅监听本机。
