# 真钱支付接入:剩余事项(2026-11-26 更新,交 Gemini 与负责人)

## 现状

- 11-20 交接的 4 项代码与 10 步验证,Gemini 已在 11-20~21 做完,代码已全部提交到 main。回执是 PROGRESS 的两条:
  - 「2026-11-21 Gemini … 真钱支付生产加固全量编译、单测、隔离 TiDB 真库与部署契约一次授权验证收口」
  - 「2026-11-21 Gemini … 真钱支付 `devpay` 真进程端到端冒烟、`writer_authority` 白名单修复与部署契约补齐」
- 实测通过的:
  - 单测:`go/payment`、`go/trade`、`java/mall_game`,以及 C++ `AssetOpSystemTest` 里的支付用例
  - 隔离 TiDB 真库用例,含扣回、补发整条协议 `TestTiDBMallReversalClawsBackThenRegrants`
  - 部署契约测试
  - Unity:四平台编译、EditMode
  - devpay 真进程五步冒烟:下单、付款、重复付款、自动退款、渠道撤销

## 还剩(验证,Gemini 用 Carbon Medium)

1. **Go `-race`**:本机缺 gcc。在 Linux 或 Docker 里对 `go/payment`、`go/trade` 跑 `go test -race ./...`。
2. **整链路冒烟**:devpay 冒烟只起了商城和 payment,没起 trade 与 scene,所以「发货到账」和「撤销后扣回到账」都没有在真进程上走过。按 `AGENTS.md` §6.2 清数据后,用全套本机服务加 Unity 编辑器走一遍:
   - 下单 → 联调支付 → `go run ./cmd/devpaynotify -order <订单号> -amount <分>` → 订单「已到账」,资产进包。
   - 再加 `-revoke` 发一次 → 订单经「扣回中」到「已扣回」,资产被扣走。
3. **kind 集群冒烟**:本机没装 kind,`kubectl` 也没有可用集群。装工具要负责人同意(`AGENTS.md` §10.2)。步骤按 PROGRESS「商城 mall、支付 payment 与 trade 的商城接线接进 K8s 部署」那条的 8 步;第 4 步(手工装商城表)不用做。

## 需要负责人

- 真实商户号与各渠道后台配置:微信支付、支付宝、App Store Connect、Google Play、微信小游戏、抖音小游戏(含商品登记)。
- 回调域名与证书;安卓包名与签名(微信开放平台登记要用)。
- 真机与沙盒购买验收。
- 生产合规闸取值(`none` / `realname` / `overseas`)与未成年人限额数字。
- Java 独立仓:`PARITY.md` 已登记「2026-11-19 真钱支付接入」待做,要不要开始移植由负责人定。
