# 发号器方案正确性评审记录(2026-10-30)

评审对象:总体方案 v1.3 的 §6(etcd 计数器 + 时间下限 + 水位探测)。一路只读对抗评审,静态推导,未运行。结论:**不能按 v1.3 落码**。无 etcd 回退时 `lo≥读到的计数器` + CAS 在任何时钟下都不重叠;回退路径、时钟污染、地域码有反例。方案已按此重写为 v1.4(内存发段 + 两份独立预留记录)。

## 致命

1. **「旧号≤真实时间+60 秒」不成立。** 门禁只比发号器自己的钟。反例:A 钟快 1 天 → 发 `(T+1天)<<24` 起的段 → 水位未写到 → etcd 回退,钟已恢复 → `lo=now<<24` → 一天后同一秒再领段,seq=0,逐号重叠,此时时钟完全正确。小窗同型:A 快 50 秒不触门禁,etcd 清空速起,B 的 lo 落进 A 发过的秒。「等 60 秒」无代码强制;「按 0 算」还推翻了现行「缺行即拒绝」(`id_segment_store.go:76`)。→ v1.4:先把上限写进两份记录再发号。
2. **一次坏时钟停全服发号。** A 钟跳未来 Δ → 计数器被抬 → 钟恢复 → 全部副本拒发 Δ−60 秒,14 种 ID 全停,没有恢复路径;把计数器改回去又会重号。→ v1.4:发号路径没有时钟门禁,预留时不合理的时钟只是不被采用。
3. **地域码在客户端、计数器在 etcd,无绑定。** 计数器都跟墙钟走且从 seq=0 起,两套 etcd 同码或客户端配错几乎必重号。→ v1.4:地域码放计数器最高位,由发号器配置并与存储核对,客户端不配。

## 必须修(v1.4 已处理)

- 水位竞态:先读 W 再线性读 C,否则误判回退;写入须相对存值单调;`W+间隔最大量` 在写失败或钟跳时不是上界。→ 取消周期水位。
- 打散:cycle walking 要循环到最高位为 0 才是双射;只算一次则 1/4 仍 ≥2^63。更简单的是 63 位原生、无循环的写法;全程 uint64,C++ 用 ULL,Java 用 `>>>`,测逆函数往返。
- 预算冲突:lead≈1e9,store 上限 1e7 时 100 个并发即满;C++ MaxStep 合计 310 万(`bin/etc/base_deploy_config.yaml:232-250`),325 个 scene 同领即满;10 万 scene 按初始步长重启约 6 分钟。→ 取消超前上限,发号器内存切段,可以截短到块尾(客户端不要求 `hi-lo=step`)。
- etcd Txn:客户端最多一次(`etcd/client/v3@v3.5.15/retry.go:83,116`),同段不会给两人。前提:每轮重读、判 `Succeeded`、缺 key 用 `CreateRevision=0`;超时作废,**禁止「重读值等于我的 hi 就认领」**,两人算出同一 hi 会持同段。
- 值域假设:`minter.go:24`「不相交」打散后为假,要删 `conf.go:27` 与 `player_id_minter.go:37`;`raiseIdSegmentFloor`(`id_segment_store.go:303`)失效;2^55 硬编码见 `client.go:81`、`guid_segment_client.h:79`。
- 漏列的单调依赖:`snapshot_store.go:918`、`schema.go:1201`(data_service store)、`go/trade/internal/data/commerce_repo.go:121`、`go/guild/internal/data/asset_store.go:272`。`%N` 分片(`go/shared/placement/route.go:218`)无碍。
- 「唯一键兜底」夸大:`transaction_log_store.go:111`、`snapshot_store.go:322` 撞号静默。

## 可接受残余

- 63 位下 int64 路径安全(文档三处外另有 `proto/player_locator/player_locator.proto:11`)。按名字 grep 未见 `float64(id)`、窄于 BIGINT 的 ID 列。Lua `tonumber` 只抽查,未逐条;客户端、Java 未查。dev 现为小号,须用大号跑全链路。
- 打散公开可逆,ID 能反算计数器,「看不出来」应改「不依赖」。
- 两份记录同时回退且时钟不准:只能演练 runbook,不能消除。
