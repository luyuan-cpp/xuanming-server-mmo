# 真钱支付接入:剩余代码与全部验证交接(2026-11-20,交 Gemini)

## 0. 现状

- **范围**:安卓 App 的微信支付、支付宝与 Google Play,iOS 的 App Store,微信小游戏、抖音小游戏,外加本地联调渠道 devpay。设计与实现记录在 `docs/design/payment-recharge.md` §22,与前文冲突时以 §22 和代码为准。
- **代码都写出来了**:
  - 新服务 `go/payment`
  - 商城 `java/mall_game`:货币发放、付款修订、扣回待办、退款转人工
  - 交易 `go/trade`:货币发放、扣回收敛
  - scene 的交易扣出流允许 `TX_PAYMENT_REVERSAL`
  - Unity 客户端:支付流程和全部平台桥,含 Unity IAP 的苹果 / 谷歌桥
  - K8s:商城进集群、payment、回调 Service、告警、开服手册
- **一行都没验证过**:没编译、没跑单测、没冒烟、没上真机。原 Claude 会话额度用完,剩下的全部交给你。
- **提交情况**:
  - 服务端大部分已在 `9824368eb`、`55eae6be3`。
  - 客户端一个都没提交。
- **还没提交的支付相关文件**都在主工作区,没有 worktree。这些文件里混着别的会话的改动,提交时按内容分清。
  - 服务端:
    - `go/payment/internal/channel/` 下 6 个源文件与 4 个新测试
    - `go/payment/cmd/devpaynotify/`
    - `go/payment/{channels.go,etc/payment.yaml}` 与 `go/payment/internal/{config,core}/`:「充值按年龄段限额闸」会话的改动
    - `go/trade/internal/commerce/official_reversal_tidb_test.go`
    - `java/README.md`
    - `deploy/k8s/{README.md,payment-alerts.yaml}`
    - `tools/scripts/{dev_tools.ps1,k8s_deploy.ps1}`
    - `tools/scripts/tests/k8s_mall_payment_deploy_contract.tests.ps1`
    - `.github/workflows/deploy-config-tests.yml`
  - 客户端 `D:\luyuan\mmorpg-client`:
    - `Packages/manifest.json`、`Packages/packages-lock.json`:Unity 已把 `com.unity.purchasing` 5.0.4 解析进锁文件
    - `Assets/Scripts/MmorpgClient.asmdef`
    - `Assets/Scripts/Game/Jubaozhai/` 下 `PaymentClient`、`StorePayBridge`、`AndroidPayBridges`、`MinigamePayBridges`(含 `.meta`)以及改过的 `CommerceClient.cs`
    - `Assets/Scripts/UI/Ugui/Jubaozhai/{CommerceWindow,JubaozhaiUiRoot}.cs`
    - `Assets/Plugins/WebGL/MinigamePay.jslib`
    - `Assets/Tests/EditMode/Jubaozhai/PaymentClientTests.cs`
- **一次中断的复核**:11-20 01:28–01:47,子代理额度用完,没有交回报告。它留下的东西见 §2.1。

## 1. 开工前必读与规矩

- 规矩与 `docs/handoff/team-follow-afk-hosting-handoff-20261120.md` §1 相同:读 `AGENTS.md`;只在 main 做;不 push;只有负责人要求才 commit,而且只提交自己的改动;不手改生成产物;日志用英文、文档与注释用中文;失败时修根因,不跳测试。
- 模型档位按 `AGENTS.md` §10.3:改代码用 `Gemini 4 Neon Extra`,编译、生成、跑测试、冒烟用 `Carbon Medium`。
- 支付专有的几条:
  - 不写任何真实密钥:测试密钥在测试里现场生成,devpay 密钥只放环境变量。
  - devpay 只许在 dev / test 打开。合规闸(`MinorSpendingGate`)不要为了跑通而放宽。
  - 钱的路径一律 fail-closed:验签失败、对不上订单,都不能记成已付。
  - 渠道的单测不连外网。

## 2. 要写的代码(Neon Extra)

### 2.1 渠道复核收尾(`go/payment/internal/channel/`)

中断的复核改了 6 个源文件。逐处读懂并核对,认可就留;不认可就改回,并在 PROGRESS 写原因:

1. 新函数 `send()`(`channel.go`),所有 `HTTP.Do` 都改走它。传输失败时去掉错误里的完整 URL:微信小游戏的 AppSecret、access_token 和谷歌的购买令牌都在 URL 里,原样会进日志。
2. `apple.go`:
   - 不带 `data` 的通知验过签后照常应答,不再当成验签失败。原来那样会让 App Store 一直重发。
   - 通知历史的起点抬到 179 天内。
   - 一个时间窗翻不完 50 页时,窗口减半再拉,保证游标能往前走。
3. `google.go`:取不到 OIDC 公钥时报 `errGoogleCerts`(验不了),不再算成验签失败。重拉之后仍没有这个 kid 才算验签失败。
4. `wechatpay.go`:关单遇到 `ORDER_CLOSED` 算成功。
5. `alipay.go`、`wechatminigame.go`:只换成了 `send()`。

它写的 4 个测试文件没编译过,其中 `alipay_test.go`、`wechatpay_test.go`、`wechatminigame_test.go` 还没 gofmt。用例名:

- `channel_test.go`:devpay 往返与固定应答、密钥解析、RSA 往返、`send()` 不泄露 URL
- `alipay_test.go`:9 条
- `wechatpay_test.go`:11 条
- `wechatminigame_test.go`:11 条

要做的事:

1. gofmt 这三个文件,再把四个文件编过:核对它们调用的函数、字段确实存在,签名一致。
2. 补三个测试文件,只用标准库,时间用配置里的 `Now`:
   - `apple_test.go`:测试里现场生成根、中间、叶子三级证书(带代码要求的 OID),签交易 JWS 与通知 V2,验证通过。再分别覆盖:链不对、缺 OID、bundle 不符、带撤销日期、没有 `data` 的通知、通知历史的减半与 179 天下限。
   - `google_test.go`:用 `httptest` 起假的 JWKS 与令牌端点。覆盖:RTDN 的 OIDC 验证;取不到公钥不算验签失败;`Verify` 对 PURCHASED、PENDING、CANCELLED 的映射;作废购买通知。
   - `douyin_test.go`:回调签名与解析。
3. 按抖音小游戏的官方文档核对 `douyin.go` 的回调签名、字段和应答格式。实现时文档打不开,是按设计稿附录 A 写的。不符就改,并补测试。

### 2.2 安卓 App 的微信支付与支付宝(客户端)

`AndroidPayBridges.cs` 已经用反射调两家 SDK,还缺打包依赖和微信的回调页。照仓里现成的 `Assets/Plugins/Android/MmorpgGoogleSignIn.androidlib` 新建一个 `MmorpgPay.androidlib`:

- `build.gradle` 引微信 OpenSDK(`com.tencent.mm.opensdk:wechat-sdk-android`)和支付宝 SDK(`com.alipay.sdk:alipaysdk-android`)。版本取两家官方文档当前推荐的。
- `AndroidManifest.xml`:
  - `<queries>` 里声明 `com.tencent.mm` 和 `com.eg.android.AlipayGphone`(Android 11 起的包可见性要求)。
  - 支付回调页写在这个库自己的包里,再用 `<activity-alias android:name="${applicationId}.wxapi.WXPayEntryActivity" android:targetActivity="<库里的类>" android:exported="true"/>` 挂到应用包名下。这样不用写死包名;客户端的安卓包名现在还没定。
- 回调页只做两件事:`handleIntent`,然后 `finish()`。支付结果不采信,`PaymentClient` 会去服务端查。
- 开了代码混淆的话,补两家 SDK 要求的 keep 规则。
- 核对 `AndroidPayBridges.cs` 反射用的类名、字段名与所选 SDK 版本一致。

### 2.3 商城的健康检查改成 HTTP

`deploy/k8s/manifests/java-svc/mall.yaml` 的三个探针现在是 TCP。gateway 用的是 actuator 的 `/actuator/health/readiness`、`/actuator/health/liveness`,商城照它做:

- `java/mall_game` 的 `application.yaml` 只开 health(参照 `java/gateway_node` 的 management 配置)。
- 改 `mall.yaml` 的三个探针。
- 契约测试 `k8s_mall_payment_deploy_contract.tests.ps1` 里钉 TCP 探针的断言改成 HTTP。
- `deploy/k8s/README.md`「商城 mall」一节同步。

### 2.4 Unity IAP 编译后的修正

`Assets/Scripts/Game/Jubaozhai/StorePayBridge.cs` 按 `com.unity.purchasing@5.0` 的官方文档写成。有两处没查到文档页:`CartItem.Product` 和 `PurchaseFailureReason.UserCancelled`。编不过时按官方文档改。

## 3. 验证(Carbon Medium;发现要改代码的问题,保留首错原文,交 Neon Extra 修)

按顺序做。每一步的期望与失败时要留的东西,在下面列的 PROGRESS 条目里都有。

1. **导表与协议生成**:先导表,再跑 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`。限额闸会话加了表 `payment_spend` 和意图上的列,必须重新生成。
2. **go/payment**:`go mod tidy`、`go build ./...`、`go vet ./...`、`go test ./...`。
3. **go/trade**:
   - `go build ./...`、`go test ./internal/commerce/...`。
   - 有隔离 TiDB 时设 `COMMERCE_TEST_DSN` 跑真库用例,含新用例 `TestTiDBMallReversalClawsBackThenRegrants`。
4. **java/mall_game**:`mvn -q test`,含新用例类 `PaymentRevisionStoreTest`。11-13 那次 Codex 下载依赖被 Maven Central 的 DNS 挡住过。
5. **C++**:
   - 先串行(`/m:1`)重编 `cpp/generated/table` 和 scene 的库,再跑 `AssetOpSystemTest` 整组,含新用例 `PaymentReversalDebitsOnTradeStreamAndNeverOverdraws`。
   - 交付前跑一次 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`。
6. **部署脚本测试**:
   - `tools/scripts/tests/` 下的 `k8s_mall_payment_deploy_contract`(22 条)、`k8s_deploy_contract`、`k8s_migrate_gate`、`logical_db_deploy_contract`、`redis_player_cluster_contract`、`sql_boot_catalog_local`。
   - 再跑整个 `tools/scripts/tests/` 目录。
7. **Unity**:
   - 编辑器编译。
   - EditMode 跑 `PaymentClientTests`(5 条)与 `CommerceClientTests`。
   - 切到 Android、iOS、WebGL 各编一次脚本:平台桥只在对应平台参与编译。
8. **本机冒烟(devpay)**:
   - 按 `AGENTS.md` §6.2 清数据。
   - 设环境变量:`COMMERCE_PAYMENT_SECRET` 与商城同值;`PAYMENT_DEVPAY_SECRET` 至少 32 字节;商城的 `COMMERCE_ALLOWED_PAYMENT_CHANNELS` 加上 `devpay`。
   - `go_services.ps1` 起 payment 之前先跑一次 `payment -migrate`。
   - 在 Unity 编辑器(桌面平台只开 devpay)进聚宝斋,下一张官方订单,点「联调支付」。
   - 90 秒内在 `go/payment` 下跑 `go run ./cmd/devpaynotify -order <订单号> -amount <订单金额,分>`。
   - 期望:订单到「已到账」,背包或货币到账。
   - 再加 `-revoke` 发一次。期望:订单经「付款已被渠道撤销,正在核对发放」到「…发放已收回」,资产被扣回。
   - 再发一笔 `-seq 1`。期望:按重复付款进退款待办。
9. **kind 冒烟**:按 PROGRESS「商城 mall、支付 payment 与 trade 的商城接线接进 K8s 部署」那条的 8 步做。第 4 步(手工装商城表)已经不用:建表 Job 会装。
10. **记录结果**:在 `docs/PROGRESS.md` 末尾追加一条「Gemini:真钱支付接入 … 验证」,写命令、通过数和失败原文摘要。

验证细节在这几条 PROGRESS 里(按标题搜):

- 「2026-11-19 Claude:真钱支付接入」
- 「商城 mall、支付 payment 与 trade 的商城接线接进 K8s 部署」
- 「商城业务表的安装接进 mall-schema Job」
- 「客户端苹果、谷歌支付桥」
- 「支付 K8s 的三处缺口补齐」
- 限额闸会话的「未成年人充值限额闸」两条与「go/payment 记账第二步收口」

## 4. 需要负责人,不在本单范围

- **真实商户与后台配置**:
  - 微信支付商户号与 APIv3 密钥、支付宝应用
  - App Store Connect 的商品与 API 密钥、Google Play 的商品与服务账号
  - 微信小游戏虚拟支付、抖音小游戏支付
  - 回调域名与证书
  - 安卓包名与签名(微信开放平台登记要用)
- **验收**:真机与沙盒购买。
- **合规**:生产的合规闸取值(`none` / `realname` / `overseas`)和未成年人限额的数字。这归限额闸那条线。
- **提交**:只在负责人要求时。
- **Java 版**:独立 Java 仓只登记待做,`PARITY.md` 已有「2026-11-19 真钱支付接入」条目。
