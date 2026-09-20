//go:build assetop_smoke

// 通用资产通道的端到端冒烟(规格 docs/design/guild-phase2/04-asset-channel.md §4.40)。
//
// 它**默认不参与任何构建**:只有显式带标签才编译,免得 `go test ./...` 去连一个不存在的
// 全栈环境。跑法(工作目录 go/shared):
//
//	$env:ASSETOP_SMOKE_PLAYER_ID="<robot_9501 的 player_id>"
//	$env:ASSETOP_SMOKE_REDIS_ADDR="127.0.0.1:6379"
//	$env:ASSETOP_SMOKE_ETCD="127.0.0.1:2379"
//	# MMORPG_ASSET_OP_SECRET_GUILD / _TRADE 与 scene 一致;未设时用 cpp_nodes.ps1 的开发值
//	go test -tags assetop_smoke ./assetop -run TestSceneAssetOpSmoke -count=1 -v
//
// 前置(规格 §4.40 第 1、2 步):全栈已起;专用账号 `robot_9501` 已登录进场并保持在线;
// 已 GmAddCurrency 加金币 1000,记下余额 B0。**这个账号只给本冒烟用**,永远不参与帮会或
// 交易业务 —— 它是不变量 I6「一条流由一个服务独占」的唯一例外,混用会让帮会的 seq 与
// 本测试的 seq 撞在同一条流上。
//
// 为什么每次运行取一个新纪元:`epoch = 当前毫秒`,seq 从 1 起。上一次运行的纪元更小,
// scene 按 §4.29 的规则在首次记账时重置该流,所以反复跑不会把窗口顶走,也不会读到上一轮
// 的结局。这也是为什么本文件里没有任何"清账本"的步骤 —— 纪元本身就是隔离手段。
//
// 与线上的差别:这里不接 outbox 表,也不跑重投循环,只直连 Caller。表与循环的行为由
// seq_integration_test.go 与 reconcile_test.go 覆盖;本文件专门验证**跨语言契约**:
// 签名串、流白名单、纪元、跳号上限、durable 语义在真 scene 上确实如设计所说。
package assetop

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	clientv3 "go.etcd.io/etcd/client/v3"

	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"

	"shared/safego"
	"shared/scenenode"
)

const (
	// smokeGoldCurrency 是金币的 CurrencyType 数值。
	// 权威定义在 C++:cpp/libs/modules/currency/constants/currency.h:8 `kCurrencyGold = 0`。
	// 这个枚举没有 proto 镜像,所以只能写数值 + 出处,改那边要连这里一起改。
	smokeGoldCurrency = 0

	// smokeDebitAmount 是步骤 ①/⑥ 的金额:先扣 100 再发 100,跑完余额回到 B0。
	smokeDebitAmount = 100
	// smokeHugeAmount 用于步骤 ⑤:远超任何账号的余额,必然 REJECTED + 27000。
	smokeHugeAmount = 1_000_000_000_000
	// smokeJumpSeq 用于步骤 ⑨:远大于 max_seq + 1024(此刻 max_seq=3),必判跳号过远。
	smokeJumpSeq = 5000

	// smokeBudget 与重投循环的单行预算 LoopConfig.OpBudget 同值:
	// 冒烟要验的正是"生产路径那 2.5s 够不够拿到 durable 结局"。
	smokeBudget = 2500 * time.Millisecond
	// smokeSyncTimeout 是等 etcd 节点镜像首次全量同步的上限。
	smokeSyncTimeout = 10 * time.Second
)

// 三把密钥。开发值与 tools/scripts/cpp_nodes.ps1 注入 scene 的值一字不差(规格 §4.32);
// 它们是**公开的本地开发值**,不是任何环境的真密钥,所以可以写进仓库。
const (
	smokeDevGuildSecret = "change-me-dev-asset-op-guild-secret-000000"
	smokeDevTradeSecret = "change-me-dev-asset-op-trade-secret-000000"
	// smokeWrongSecret 长度合法但值不对,用来验"签名不符"这一档确实被拒(步骤 ⑩)。
	// 若它与真密钥相同,步骤 ⑩ 会假绿,所以这里刻意取一个不可能被注入的前缀。
	smokeWrongSecret = "wrong-dev-asset-op-guild-secret-0000000000"
)

// smokeRig 持有一次冒烟运行的全部上下文。
type smokeRig struct {
	playerID uint64
	// epoch 本次运行的流纪元,两条流共用(它们各自有独立的 seq 空间)。
	epoch uint64
	// guild 是 caller=guild + 正确密钥的调用方,绝大多数步骤用它。
	guild *Caller
	// wrongKey 是 caller=guild 但密钥不对(步骤 ⑩)。
	wrongKey *Caller
	// trade 是 caller=trade + trade 正确密钥(步骤 ⑪:调用方不在该流白名单内)。
	trade *Caller
}

// TestSceneAssetOpSmoke 按规格 §4.40 第 3 步依次跑 11 个断言,末尾追加第 4 步"核对"里
// 那条可选的 seq5 验证。任何一步失败都立刻停:后面的步骤依赖前面留下的账本状态。
func TestSceneAssetOpSmoke(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rig := newSmokeRig(ctx, t)
	t.Logf("冒烟开始 player_id=%d epoch=%d", rig.playerID, rig.epoch)

	debit := assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT
	credit := assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT
	txDonate := uint32(rollbackpb.TransactionType_TX_GUILD_DONATE)
	txShop := uint32(rollbackpb.TransactionType_TX_GUILD_SHOP)

	// ① 正常扣款:2.5s 内必须拿到"结局固定且已落盘"。
	seq1 := rig.req(debit, 1, txDonate, smokeGold(smokeDebitAmount))
	res := rig.mustTerminal(ctx, t, "①首次扣款", rig.guild, RPCDebit, seq1)
	rig.wantOutcome(t, "①首次扣款", res, assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, 0)
	if res.Partial {
		t.Fatalf("①首次扣款:Debit 不该出现部分发放")
	}

	// ② 同一请求再发一次。这是整条通道最重要的性质:见过的 seq 只读答复,不重办。
	//    余额只被扣一次的证据由第 4 步"核对"给(拉背包对 B0),这里先确认答复一致。
	res = rig.mustTerminal(ctx, t, "②重投同一 seq", rig.guild, RPCDebit, rig.req(debit, 1, txDonate, smokeGold(smokeDebitAmount)))
	rig.wantOutcome(t, "②重投同一 seq", res, assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, 0)

	// ③ 中止一个从未见过的 seq:从此它永远是 REJECTED + reason 0(中止占位)。
	res = rig.mustTerminal(ctx, t, "③中止未见 seq2", rig.guild, RPCAbort, rig.req(debit, 2, txDonate, smokeGold(smokeDebitAmount)))
	rig.wantOutcome(t, "③中止未见 seq2", res, assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, 0)
	if got := FinalStatus(RPCAbort, res, Op{}); got != StatusAborted {
		t.Fatalf("③中止未见 seq2:终态应为 %s,实际 %s", StatusAborted, got)
	}

	// ④ 被中止的 seq 再来一次正常扣款:必须还是 REJECTED —— 占位就是为了挡住它。
	res = rig.mustTerminal(ctx, t, "④占位后再扣 seq2", rig.guild, RPCDebit, rig.req(debit, 2, txDonate, smokeGold(smokeDebitAmount)))
	rig.wantOutcome(t, "④占位后再扣 seq2", res, assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, 0)

	// ⑤ 余额不足:业务拒绝,记账,带 27000。注意它与 ③/④ 的 reason 0 是两回事 ——
	//    Go 侧靠这个差别区分"中止占位"(要退款)与"真的被拒"。
	res = rig.mustTerminal(ctx, t, "⑤余额不足", rig.guild, RPCDebit, rig.req(debit, 3, txDonate, smokeGold(smokeHugeAmount)))
	rig.wantOutcome(t, "⑤余额不足", res, assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, ReasonCurrencyInsufficient)

	// ⑥ 发放:走另一条流,所以 seq 从 1 重新开始,与 GUILD_DEBIT 的 seq1 互不相干。
	res = rig.mustTerminal(ctx, t, "⑥发放", rig.guild, RPCCredit, rig.req(credit, 1, txShop, smokeGold(smokeDebitAmount)))
	rig.wantOutcome(t, "⑥发放", res, assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, 0)
	if res.Partial {
		t.Fatalf("⑥发放:只发一种货币不该出现部分发放")
	}

	// ⑦ 方向不符:拿 Credit 流去调 AssetDebit。信封校验就该拦下,不记账。
	res = rig.mustReply(ctx, t, "⑦流方向不符", rig.guild, RPCDebit, rig.req(credit, 2, txDonate, smokeGold(smokeDebitAmount)))
	rig.wantOutcome(t, "⑦流方向不符", res, assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN, ReasonInvalidBundle)

	// ⑧ 纪元过期:用比账本小的纪元查。UNKNOWN + reason 0,不记账 —— 这正是帮会库
	//    恢复/重建之后旧行的下场,由人工按运维手册处置。
	stale := rig.req(debit, 4, txDonate, smokeGold(smokeDebitAmount))
	stale.StreamEpoch = rig.epoch - 1
	res = rig.mustReply(ctx, t, "⑧纪元过期", rig.guild, RPCDebit, stale)
	rig.wantOutcome(t, "⑧纪元过期", res, assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN, 0)

	// ⑨ 跳号过远:seq 5000 远超 max_seq + 1024。同样 UNKNOWN + reason 0。
	res = rig.mustReply(ctx, t, "⑨跳号过远", rig.guild, RPCDebit, rig.req(debit, smokeJumpSeq, txDonate, smokeGold(smokeDebitAmount)))
	rig.wantOutcome(t, "⑨跳号过远", res, assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN, 0)

	// ⑩ 签名不符:caller 对、密钥错。27008,不记账。
	res = rig.mustReply(ctx, t, "⑩签名不符", rig.wrongKey, RPCDebit, rig.req(debit, 5, txDonate, smokeGold(smokeDebitAmount)))
	rig.wantOutcome(t, "⑩签名不符", res, assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN, ReasonAuthFailed)

	// ⑪ 调用方不在该流的白名单内:trade 的签名是有效的,但 GUILD_DEBIT 只认 guild。
	//    这条验的是"每个调用方一把密钥"确实把流隔开了,而不只是"有签名就放行"。
	res = rig.mustReply(ctx, t, "⑪调用方越流", rig.trade, RPCDebit, rig.req(debit, 5, txDonate, smokeGold(smokeDebitAmount)))
	rig.wantOutcome(t, "⑪调用方越流", res, assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN, ReasonAuthFailed)

	// 第 4 步"核对"里的那条可选验证:seq5 被 ⑩⑪ 拒过两次,如果它们其实记了账,
	//    这次中止就会读回 APPLIED 而不是把它钉成 REJECTED。
	res = rig.mustTerminal(ctx, t, "⑫核对 seq5 未记账", rig.guild, RPCAbort, rig.req(debit, 5, txDonate, smokeGold(smokeDebitAmount)))
	rig.wantOutcome(t, "⑫核对 seq5 未记账", res, assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, 0)

	t.Log("冒烟 11 步全绿。请按规格 §4.40 第 4 步继续人工核对:" +
		"robot 拉背包金币 == B0;scene 日志里 seq=1 的 outcome=1 应用行恰 1 条。")
}

// newSmokeRig 装配定位链路与三个调用方。
//
// 这里刻意用**生产装配**(etcd 镜像 + Redis 位置键 + 连接缓存),不走任何测试替身:
// 冒烟要验的就是这套装配本身能不能把包送到持有玩家的那个进程。
func newSmokeRig(ctx context.Context, t *testing.T) *smokeRig {
	t.Helper()

	playerID := smokePlayerID(t)
	endpoints := smokeEnv("ASSETOP_SMOKE_ETCD", "127.0.0.1:2379")
	redisAddr := smokeEnv("ASSETOP_SMOKE_REDIS_ADDR", "127.0.0.1:6379")

	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(endpoints, ","),
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("连 etcd 失败(%s): %v", endpoints, err)
	}
	t.Cleanup(func() { _ = etcdCli.Close() })

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("连 Redis 失败(%s): %v", redisAddr, err)
	}

	conns := scenenode.NewConnCache()
	t.Cleanup(conns.Close)

	// onRemove 接到连接缓存:节点下线后要把旧连接丢掉,否则重连上来的新进程会复用
	// 一条指向已死地址的连接。这与 guild 的生产装配是同一份接线。
	watcher := scenenode.NewWatcher("scene", scenenode.SceneNodeRpcPrefix, nil,
		func(entry scenenode.NodeEntry) { conns.Remove(entry.Endpoint) })
	safego.Go("assetop.smoke_watcher", func() { watcher.Run(ctx, etcdCli) })

	deadline := time.Now().Add(smokeSyncTimeout)
	for !watcher.Synced() {
		if time.Now().After(deadline) {
			t.Fatalf("节点镜像 %v 内未完成首次全量同步(etcd=%s prefix=%s):"+
				"确认 scene 节点已注册", smokeSyncTimeout, endpoints, scenenode.SceneNodeRpcPrefix)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("节点镜像已同步,已注册 scene 节点 %d 个", watcher.Count())

	locator := &scenenode.Locator{
		Reader:  scenenode.GoRedisReader{Client: rdb},
		Watcher: watcher,
		Conns:   conns,
	}

	rig := &smokeRig{
		playerID: playerID,
		// 每次运行一个新纪元(规格 §4.40):上一轮的纪元更小,scene 首次记账时会重置该流。
		epoch:    uint64(time.Now().UnixMilli()),
		guild:    smokeCaller(t, locator, "guild", smokeEnv("MMORPG_ASSET_OP_SECRET_GUILD", smokeDevGuildSecret)),
		wrongKey: smokeCaller(t, locator, "guild", smokeWrongSecret),
		trade:    smokeCaller(t, locator, "trade", smokeEnv("MMORPG_ASSET_OP_SECRET_TRADE", smokeDevTradeSecret)),
	}
	return rig
}

// smokeCaller 造一个只差密钥/调用方的 Caller。超时取生产默认值,保证冒烟量的是生产预算。
func smokeCaller(t *testing.T, r Resolver, caller, secret string) *Caller {
	t.Helper()
	signer, err := NewSigner(caller, secret)
	if err != nil {
		t.Fatalf("建 %s 签名器失败(密钥至少 %d 字节): %v", caller, MinSecretLen, err)
	}
	return &Caller{
		Resolver:    r,
		Signer:      signer,
		CallTimeout: DefaultCallTimeout,
		Requery:     DefaultRequery,
	}
}

// req 拼一条请求。correlation_id 取本次运行的纪元(规格 §4.40),这样 scene 流水里
// 这一轮冒烟产生的记录能被一次 grep 出来。
func (r *smokeRig) req(stream assetpb.AssetOpStream, seq uint64, txType uint32, bundle *assetpb.AssetBundle) *assetpb.AssetOpRequest {
	return &assetpb.AssetOpRequest{
		PlayerId:      r.playerID,
		Stream:        stream,
		Seq:           seq,
		StreamEpoch:   r.epoch,
		CorrelationId: r.epoch,
		TxType:        txType,
		Bundle:        bundle,
	}
}

// smokeGold 造一个"只有金币"的资产包。Debit v1 只收这种形状(恰 1 种货币、0 个物品)。
func smokeGold(amount uint64) *assetpb.AssetBundle {
	return &assetpb.AssetBundle{
		Currencies: []*assetpb.CurrencyAmount{{CurrencyType: smokeGoldCurrency, Amount: amount}},
	}
}

// mustTerminal 在 smokeBudget 内反复投递同一请求,直到"结局固定且已落盘"。
//
// 为什么可以放心重投:scene 见过的 seq 只读答复(不变量 I2/I3),重投至多是再问一遍。
// 这也正是生产重投循环的行为,所以这里量到的时间就是玩家能感知到的时间。
// 超预算即失败,并把最后一次的结局打出来 —— RETRY 通常意味着 robot 在战斗中或被冻结。
func (r *smokeRig) mustTerminal(ctx context.Context, t *testing.T, step string, c *Caller, rpc RPC, req *assetpb.AssetOpRequest) Result {
	t.Helper()

	budgetCtx, cancel := context.WithTimeout(ctx, smokeBudget)
	defer cancel()

	var (
		last     Result
		lastErr  error
		attempts int
	)
	for {
		res, err := c.Do(budgetCtx, rpc, req)
		attempts++
		if err == nil {
			last, lastErr = res, nil
			if res.Terminal() {
				t.Logf("%s:rpc=%s seq=%d outcome=%s reason=%d durable=%t partial=%t 投递 %d 次",
					step, rpc, req.GetSeq(), outcomeLabel(res.Outcome), res.Reason, res.Durable, res.Partial, attempts)
				return res
			}
		} else {
			lastErr = err
			// 预算没用完的传输失败照常重试:scene 可能正好在重启,重投是安全的。
			// 预算用完再报,报的时候要说清是超时还是真失败(见下面的分支)。
			if budgetCtx.Err() == nil {
				t.Logf("%s:第 %d 次投递失败,继续重试: %v", step, attempts, err)
			}
		}
		if budgetCtx.Err() != nil {
			t.Fatalf("%s:%v 内未拿到已落盘的固定结局(投递 %d 次)。"+
				"最后一次 outcome=%s reason=%d durable=%t local=%t err=%v。"+
				"RETRY 通常意味着 robot 在战斗中 / 被冻结;NOT_HERE 意味着人不在线或还没进场",
				step, smokeBudget, attempts,
				outcomeLabel(last.Outcome), last.Reason, last.Durable, last.Local, lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// mustReply 只投一次,拿到答复即可 —— 用于那些**本就不会记账**的步骤(UNKNOWN 一族)。
// 它们永远不会 durable,用 mustTerminal 等只会白等满预算。
func (r *smokeRig) mustReply(ctx context.Context, t *testing.T, step string, c *Caller, rpc RPC, req *assetpb.AssetOpRequest) Result {
	t.Helper()

	callCtx, cancel := context.WithTimeout(ctx, smokeBudget)
	defer cancel()

	res, err := c.Do(callCtx, rpc, req)
	if err != nil {
		t.Fatalf("%s:投递出错 rpc=%s seq=%d: %v", step, rpc, req.GetSeq(), err)
	}
	t.Logf("%s:rpc=%s seq=%d outcome=%s reason=%d durable=%t",
		step, rpc, req.GetSeq(), outcomeLabel(res.Outcome), res.Reason, res.Durable)
	return res
}

// wantOutcome 断言结局与原因码。原因码一起断言是有必要的:REJECTED+0(中止占位)与
// REJECTED+27000(余额不足)对业务侧是**相反**的处置(退款 vs 不退款)。
func (r *smokeRig) wantOutcome(t *testing.T, step string, res Result, want assetpb.AssetOpOutcome, wantReason uint32) {
	t.Helper()
	if res.Outcome != want {
		t.Fatalf("%s:结局应为 %s,实际 %s(reason=%d durable=%t local=%t)",
			step, outcomeLabel(want), outcomeLabel(res.Outcome), res.Reason, res.Durable, res.Local)
	}
	if res.Reason != wantReason {
		t.Fatalf("%s:原因码应为 %d,实际 %d", step, wantReason, res.Reason)
	}
}

// smokePlayerID 读必填的玩家 id。未设就跳过:本文件带标签编译,但 `-tags assetop_smoke`
// 可能是整包一起带上的,不该因为没配环境就红。
func smokePlayerID(t *testing.T) uint64 {
	t.Helper()
	raw := os.Getenv("ASSETOP_SMOKE_PLAYER_ID")
	if raw == "" {
		t.Skip("未设 ASSETOP_SMOKE_PLAYER_ID(robot_9501 的 player_id),跳过端到端冒烟")
	}
	id, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil || id == 0 {
		t.Fatalf("ASSETOP_SMOKE_PLAYER_ID 不是合法的玩家 id: %q", raw)
	}
	return id
}

// smokeEnv 读环境变量,未设则用默认值。密钥的默认值是本地开发值,与 scene 脚本一致;
// 若 scene 那边注入了别的密钥而这边没设,步骤 ① 就会以 27008 失败 —— 这是预期的
// fail-closed,不要在这里加"猜密钥"的逻辑。
func smokeEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
