# 官方通行证(Casdoor)生产部署清单

设计与取舍见 [official-passport-registration.md](../../../docs/design/official-passport-registration.md) §3.10。本目录是独立的一套,不经 `k8s_deploy.ps1`;本机联调用 [deploy/passport](../../passport/) 的 docker-compose。

| 文件 | 内容 |
|---|---|
| `casdoor.yaml` | Namespace `passport`、Casdoor Deployment(单副本、镜像按摘要固定)、集群内 Service `casdoor:8000` |
| `public-entry.yaml` | nginx 白名单代理:公网入口 8080(Ingress `passport-public`)与集群内入口 8081(Ingress `passport-internal`,给 login);安卓 App Links 的 `assetlinks.json` 与回调兜底页 |
| `networkpolicy.yaml` | Casdoor 只收代理的 8000;代理只收 Ingress 控制器的 8080 / 8081 |
| `verification-record-cleanup.yaml` | 每天清一次 180 天以前的验证码记录 |
| `render-init-data.ps1` | 以 `deploy/passport/init_data.json` 为底,填本环境的 App Links 回调、协议地址与回调密钥,输出 Secret `passport-init-data` 与 ConfigMap `passport-settings` |

契约测试:`pwsh -NoProfile -File tools/scripts/tests/passport_k8s_contract.tests.ps1`(白名单、镜像摘要、初始化数据缺省、渲染脚本)。

## 安装顺序

先把清单里的 `REPLACE_WITH_*` 与 `passport.example.com` / `passport-internal.example.com` 换成本环境的值:

- `public-entry.yaml`:两个 Ingress 的 `ingressClassName`、域名与证书 Secret;`assetlinks.json` 的安卓包名与正式签名证书的 SHA-256 指纹(Play 管理中心「应用完整性」);`set_real_ip_from` 换成 Ingress 控制器 Pod 所在的网段。
- `networkpolicy.yaml`:Ingress 控制器所在的 namespace。

1. 建 namespace 与运维预置的 Secret(值不进仓库):
   ```powershell
   kubectl create namespace passport   # casdoor.yaml 里也有这个 Namespace,先建是为了放 Secret
   kubectl -n passport create secret generic passport-db --from-literal=dsn='casdoor:<口令>@tcp(<库主机>:3306)/'
   kubectl -n passport create secret generic passport-admin --from-literal=password='<首启管理员口令>'
   kubectl -n passport create secret generic passport-db-cleanup --from-literal=host='<库主机>' --from-literal=user='passport_cleanup' --from-literal=password='<口令>'
   ```
   数据库是独立的 MySQL 8.0(库名 `casdoor`),与游戏的库分开(一库一服务)。清理账号只给 `GRANT SELECT, DELETE ON casdoor.verification_record`。
2. 无密钥的基础配置:
   ```powershell
   kubectl -n passport create configmap passport-app-conf --from-file=app.conf=deploy/passport/conf/app.conf
   ```
3. 初始化数据与 origin(回调密钥与网关 `-ZoneUp` 时用的 `MMORPG_GATEWAY_PASSPORT_WEBHOOK_SECRET` 是同一个值,至少 32 字节):
   ```powershell
   $env:MMORPG_GATEWAY_PASSPORT_WEBHOOK_SECRET = '<至少 32 字节>'
   pwsh -File deploy/k8s/passport/render-init-data.ps1 -PublicOrigin https://passport.<域名> `
     -TermsUrl https://<官网>/terms -PrivacyUrl https://<官网>/privacy `
     -WebhookUrl http://gateway.<某个区的 namespace>.svc.cluster.local:8081/internal/passport/events | kubectl apply -f -
   ```
   不给 `-WebhookUrl` 就不开回调:改密 / 封禁照样在登录与刷新时生效,只是不能立刻踢下线(§3.7)。
4. `kubectl apply -f deploy/k8s/passport/`(四份清单)。
5. 首启之后:后台把 `built-in/admin` 的口令改掉并开两步验证;从后台(或 `GET /api/get-application?id=admin/app-xuanming-server`)取出 `app-xuanming-server` 的 clientSecret,放进 login 的 `MMORPG_LOGIN_PASSPORT_ADMIN_CLIENT_SECRET`。
6. 部署 login 与网关时:
   - login:`-LoginPassportIssuer https://passport.<域名> -LoginPassportClientId xuanming-game-client -LoginPassportEndpoint https://passport-internal.<域名> -LoginPassportAdminClientId xuanming-game-server`(Endpoint 必须是集群内入口:公网入口不放行管理接口);
   - 网关:设环境变量 `MMORPG_GATEWAY_PASSPORT_WEBHOOK_SECRET`(同第 3 步),`k8s_deploy.ps1` 经工作负载密钥目录 `gateway-passport-webhook` 注入;网关若有 NetworkPolicy,要放行 `passport` namespace 到 8081;
   - 客户端出正式包:`tools/build_client.ps1 -GatewayUrl … -PassportOrigin https://passport.<域名> -TermsUrl … -PrivacyUrl …`(客户端仓)。
7. 预发环境走一遍(白名单是按源码推的,缺接口页面会直接报错):注册(邮箱验证码 + 协议勾选 + 图片验证码)、登录、找回密码、游戏内「官方账号登录」、安卓从浏览器回到游戏(App Links)、改密后游戏被踢下线;浏览器开发者工具里看有没有被 404 的 `/api/*`,有就补进 `public-entry.yaml` 与契约测试的名单。

## 上线前还要人办的

- 后台给 `app-xuanming-game` 挂邮件(或短信)发码通道;把图片验证码换成行为验证码(规则保持 `Always`)。
- 静态资源自托管:`staticBaseUrl` 不配时登录页的图标、默认头像从 `cdn.casbin.org` 取;配成自己的 CDN,并在后台上传 `deploy/passport/theme/` 的品牌图。
- 要多副本:先配 `redisEndpoint`(共享会话),再把 `casdoor.yaml` 的 `replicas` 调大、`strategy` 改回滚动。
- 数据库备份加密:`cert` 表里有签名私钥,拿到备份就能冒充任何玩家登录。
