package commerce

// 正式跨zone寄售到账验收（不启动服务、不回滚玩家资产）：
//   cd go/trade
//   go test -p 1 ./internal/commerce -run TestFullstackCrossZoneConsignmentPaysAndTransfersOriginalItem -count=1 -v
//
// 必填 TRADE_ASSET_FULLSTACK_REQUIRE=1、TRADE_ASSET_FULLSTACK_MODE=dev、
// TRADE_ASSET_FULLSTACK_DEDICATED=1、TRADE_ASSET_FULLSTACK_SELLER_ID、
// TRADE_ASSET_FULLSTACK_BUYER_ID、TRADE_ASSET_FULLSTACK_ORIGINAL_ITEM_GUID、
// TRADE_ASSET_FULLSTACK_SELLER_DSN/BUYER_DSN（各自实际玩家TiDB库，仅SELECT）、
// TRADE_ASSET_FULLSTACK_ADMIN_DSN（无库名，只创建/删除本次随机交易库）、
// TRADE_ASSET_FULLSTACK_DATASERVICE_ADDR、TRADE_ASSET_FULLSTACK_ETCD_ADDR、
// TRADE_ASSET_FULLSTACK_REDIS_ADDR、TRADE_ASSET_FULLSTACK_TABLE_DIR、
// MMORPG_ASSET_OP_SECRET_TRADE。Redis密码可另设 TRADE_ASSET_FULLSTACK_REDIS_PASSWORD。
// 所有基础设施和发现出的scene地址只允许回环；任何非dev模式拒绝执行。
//
// 两名专用角色须由外部机器人保持在线/空闲，处于不同zone，不参与正式trade/其他资产写入。
// 卖家主背包须有显式指定GUID的单件装备（含成长数据），买家有至少17金币和空格；
// 两人无冻结、欠款、战斗或交接。scene加载相同物品表/TRADE密钥，home zone DB均为dual/TiDB。
// 使用正式DataService的trade_listing/trade_asset_op号源。随机库新流使用正式建行毫秒纪元，
// 因此严禁与正式trade共享这两个测试角色。成交后原件留在买家、17金币移交卖家，不删除账本。
// 未开启require只是跳过，不代表全栈通过；开启后缺任何前置或真实证据必须失败。
// 实际SQL须已写完整ItemEntry.instance；比较完整原件（含unknown fields），仅入包序号按接收背包重建。

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"shared/authoritysql/sqltest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/proto"
	assetpb "proto/common/asset"
	componentpb "proto/common/component"
	playerpb "proto/common/database"
	tradepb "proto/trade"
	"schemamigrate"
	"shared/assetop"
	"shared/authoritysql"
	"shared/domainhost"
	tables "shared/generated/table"
	"shared/scenenode"
	"trade/internal/data"
)

type tradeFullstackRig struct {
	ctx                 context.Context
	seller, buyer, guid uint64
	players             map[uint64]*sql.DB
	engine              *Engine
	loop                *assetop.Loop
	ledger              *assetop.DataServiceLedger
	caller              *assetop.Caller
}

// 每次只读同一实际玩家SQL行，不以Redis父快照构造资产或账本。
type tradeFullstackSQL struct {
	currency   *componentpb.CurrencyComp
	bag        *playerpb.BagAllData
	ledger     *componentpb.PlayerAssetOpLedgerComp
	epoch, seq uint64
}

func TestFullstackCrossZoneConsignmentPaysAndTransfersOriginalItem(t *testing.T) {
	rig := newTradeFullstackRig(t)
	sellerBefore, buyerBefore := rig.readSQL(t, rig.seller), rig.readSQL(t, rig.buyer)
	original := tradeFullstackItem(t, sellerBefore.bag, rig.guid)
	if original == nil || original.Instance == nil || original.BagType != 0 || original.StackSize != 1 || original.Equip == nil ||
		original.Equip.GetEnhanceLevel() == 0 && len(original.Equip.GetAffixes()) == 0 && len(original.Equip.GetGems()) == 0 {
		t.Fatal("必须显式提供主背包中一件已写完整instance且带成长数据的真实装备GUID")
	}
	if tradeFullstackItem(t, buyerBefore.bag, rig.guid) != nil {
		t.Fatal("验收前原GUID已经同时属于两名玩家")
	}
	for player, row := range map[uint64]*tradeFullstackSQL{rig.seller: sellerBefore, rig.buyer: buyerBefore} {
		if len(row.currency.Values) == 0 || len(row.currency.BlockedTypes) != 0 || len(row.currency.Debts) != 0 {
			t.Fatal("专用角色金币槽、欠款或冻结不满足前置")
		}
		for _, stream := range row.ledger.Streams {
			if (stream.Stream == assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT || stream.Stream == assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT) && stream.StreamEpoch >= uint64(time.Now().UnixMilli()) {
				t.Fatalf("专用角色已有不早于当前建行时钟的交易流纪元 player_id=%d", player)
			}
		}
	}
	if buyerBefore.currency.Values[0] < 17 || sellerBefore.currency.Values[0] > math.MaxUint64-17 {
		t.Fatal("专用角色余额不满足17金币成交前置")
	}
	if !contains(rig.engine.Policy.Items, original.ConfigId) {
		t.Fatal("真实原件模板不在已加载物品表中")
	}
	request := &tradepb.CreateConsignmentRequest{ClientRequestId: "fullstack-consignment", Assets: &tradepb.TradeAssetSelection{ItemUuids: []uint64{rig.guid}}, CurrencyType: 0, Price: 17, DurationMs: 10 * 60 * 1000}
	offer, err := rig.engine.CreateConsignment(rig.ctx, rig.seller, request)
	if err != nil {
		t.Fatalf("正式创建寄售失败: %v", err)
	}
	if err := rig.engine.Tick(rig.ctx); err != nil {
		t.Fatalf("正式寄售扣出计划失败: %v", err)
	}
	var sellerDebit *tradepb.TradeAssetOpRecord
	tradeFullstackAwait(t, rig.ctx, "真实Loop提交卖家原件托管", func() (bool, error) {
		// 暂不跑下一次Engine.Tick：先验证交易SQL和玩家SQL同时留有原件收据，再由正式Engine发送ACK。
		rig.loop.Tick(rig.ctx)
		ops := rig.readOps(t, offer.CommerceId)
		if len(ops) != 1 {
			return false, fmt.Errorf("首个寄售阶段应只生成一条卖家扣出腿，实际%d", len(ops))
		}
		sellerDebit = ops[0]
		if sellerDebit.Status != tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING && sellerDebit.Status != tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED {
			return false, errors.New("卖家原件扣出被拒绝或部分应用")
		}
		return sellerDebit.Status == tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED && sellerDebit.Durable, nil
	})
	if sellerDebit.PlayerId != rig.seller || sellerDebit.Stream != uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT) || !sellerDebit.ReceiptAckPending {
		t.Fatal("真实托管腿的属主、流或可靠ACK待办不符")
	}
	var escrow assetpb.AssetSnapshot
	if proto.Unmarshal(sellerDebit.EscrowSnapshot, &escrow) != nil || len(escrow.Items) != 1 || escrow.Items[0].ItemUuid != rig.guid {
		t.Fatal("交易SQL没有保留真实扣出的完整原件")
	}
	var escrowItem componentpb.ItemComp
	if proto.Unmarshal(escrow.Items[0].InstanceProto, &escrowItem) != nil || !proto.Equal(&escrowItem, original.Instance) {
		t.Fatal("交易SQL托管原件的身份或成长数据丢失")
	}
	sellerHeld := rig.readSQL(t, rig.seller)
	if tradeFullstackItem(t, sellerHeld.bag, rig.guid) != nil {
		t.Fatal("交易SQL托管提交后原件仍在卖家实际SQL背包")
	}
	heldReceipt := tradeFullstackReceipt(t, sellerHeld.ledger, sellerDebit)
	if heldReceipt == nil || !proto.Equal(heldReceipt.Snapshot, &escrow) {
		t.Fatal("真实玩家SQL账本未携带与交易SQL相同的原始扣出收据")
	}
	rig.assertProof(t, rig.seller, sellerHeld)
	tradeFullstackAwait(t, rig.ctx, "寄售正式开放", func() (bool, error) {
		if err := rig.engine.Tick(rig.ctx); err != nil {
			return false, err
		}
		row, err := rig.engine.Repo.Get(rig.ctx, offer.CommerceId)
		if err != nil {
			return false, err
		}
		w, err := data.Workflow(row)
		if err != nil {
			return false, err
		}
		return w.Stage == "open", nil
	})
	if _, err := rig.engine.Mutate(rig.ctx, rig.buyer, offer.CommerceId, "buy", "fullstack-buy"); err != nil {
		t.Fatalf("正式买单失败: %v", err)
	}
	tradeFullstackAwait(t, rig.ctx, "双边实际到账与SQL收据释放", func() (bool, error) {
		if err := rig.engine.Tick(rig.ctx); err != nil {
			return false, err
		}
		rig.loop.Tick(rig.ctx)
		if err := rig.engine.Tick(rig.ctx); err != nil {
			return false, err
		}
		row, err := rig.engine.Repo.Get(rig.ctx, offer.CommerceId)
		if err != nil {
			return false, err
		}
		if row.Status == Manual || row.Status == Cancelled {
			return false, errors.New("正式交易没有全额成交")
		}
		if row.Status != Settled {
			return false, nil
		}
		for _, op := range rig.readOps(t, offer.CommerceId) {
			if op.ReceiptAckPending {
				return false, nil
			}
		}
		return true, nil
	})
	sellerAfter, buyerAfter := rig.readSQL(t, rig.seller), rig.readSQL(t, rig.buyer)
	wantedSeller := proto.Clone(sellerBefore.currency).(*componentpb.CurrencyComp)
	wantedSeller.Values[0] += 17
	wantedBuyer := proto.Clone(buyerBefore.currency).(*componentpb.CurrencyComp)
	wantedBuyer.Values[0] -= 17
	if !proto.Equal(wantedSeller, sellerAfter.currency) || !proto.Equal(wantedBuyer, buyerAfter.currency) {
		t.Fatal("实际玩家SQL的买家扣款、卖家到账或其他币种不符")
	}
	transferred := tradeFullstackItem(t, buyerAfter.bag, rig.guid)
	if tradeFullstackItem(t, sellerAfter.bag, rig.guid) != nil || transferred == nil || transferred.Instance == nil || transferred.ConfigId != original.ConfigId || transferred.StackSize != original.StackSize || !proto.Equal(transferred.Equip, original.Equip) || !tradeFullstackSameInstance(original.Instance, transferred.Instance) {
		t.Fatal("原GUID没有唯一换主，或完整成长数据在实际SQL中变化")
	}
	ops := rig.readOps(t, offer.CommerceId)
	if len(ops) != 4 {
		t.Fatalf("寄售成交必须恰好四条资产腿，实际%d", len(ops))
	}
	for _, op := range ops {
		row := sellerAfter
		if op.PlayerId == rig.buyer {
			row = buyerAfter
		} else if op.PlayerId != rig.seller {
			t.Fatal("交易腿属主越出两名专用角色")
		}
		outcome, partial, _ := assetop.ClassifyPersisted(row.ledger, assetpb.AssetOpStream(op.Stream), op.StreamEpoch, op.Seq)
		if op.Status != tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED || !op.Durable || !op.BusinessConsumed || op.ReceiptAckPending || outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED || partial {
			t.Fatal("交易业务终态没有逐腿对应实际玩家SQL的固定结局")
		}
		receipt := tradeFullstackReceipt(t, row.ledger, op)
		if receipt == nil || len(receipt.GetSnapshot().GetItems()) != 0 || receipt.GetSnapshot().GetPet() != nil {
			t.Fatal("成交及ACK后的实际SQL必须保留匹配摘要/关联号的收据，且已释放托管原件")
		}
	}
	rig.assertProof(t, rig.seller, sellerAfter)
	rig.assertProof(t, rig.buyer, buyerAfter)
	if _, err := rig.engine.Mutate(rig.ctx, rig.buyer, offer.CommerceId, "buy", "fullstack-buy"); err != nil {
		t.Fatalf("正式买单重放失败: %v", err)
	}
	if err := rig.engine.Tick(rig.ctx); err != nil {
		t.Fatal(err)
	}
	if n := rig.loop.Tick(rig.ctx); n != 0 {
		t.Fatal("成交重放生成了新的在途资产操作")
	}
	if len(rig.readOps(t, offer.CommerceId)) != 4 {
		t.Fatal("成交重放生成第二套资产腿")
	}
	replayedSeller, replayedBuyer := rig.readSQL(t, rig.seller), rig.readSQL(t, rig.buyer)
	if !proto.Equal(replayedSeller.currency, sellerAfter.currency) || !proto.Equal(replayedBuyer.currency, buyerAfter.currency) || !proto.Equal(replayedSeller.bag, sellerAfter.bag) || !proto.Equal(replayedBuyer.bag, buyerAfter.bag) {
		t.Fatal("买单重放改变了实际玩家资产或原GUID")
	}
	// 业务request_id回放不会重发scene；另用真实库中最初四条意图重投实际资产RPC。
	identities := make(map[uint64]*componentpb.AssetOpReceipt, len(ops))
	for _, op := range ops {
		row := sellerAfter
		if op.PlayerId == rig.buyer {
			row = buyerAfter
		}
		identities[op.OpId] = proto.Clone(tradeFullstackReceipt(t, row.ledger, op)).(*componentpb.AssetOpReceipt)
		rpc := assetop.RPCDebit
		if assetpb.AssetOpStream(op.Stream) == assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT {
			rpc = assetop.RPCCredit
		} else if assetpb.AssetOpStream(op.Stream) != assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT {
			t.Fatal("原始交易腿出现非交易资产流")
		}
		res, err := rig.caller.Do(rig.ctx, rpc, tradeFullstackRequest(t, op))
		if err != nil || res.Outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED || res.Partial || !res.Durable || len(res.Snapshot.GetItems()) != 0 || res.Snapshot.GetPet() != nil {
			t.Fatalf("原交易资产请求真实重投未回放已ACK的固定结局 op_id=%d: %v", op.OpId, err)
		}
	}
	// 同一原件按1金币真实转售往返：不造SQL/腿/ACK，双方都经历后续真实资产提交。
	// 两笔合计净资产不变；入包AcquireSeq按每次接收背包重建，不作为成长数据比较。
	rig.roundTripConsignment(t, rig.buyer, rig.seller, "fullstack-barrier-return", original)
	rig.roundTripConsignment(t, rig.seller, rig.buyer, "fullstack-barrier-forward", original)
	barrierSeller, barrierBuyer := rig.readSQL(t, rig.seller), rig.readSQL(t, rig.buyer)
	for player, pair := range map[uint64][2]*tradeFullstackSQL{
		rig.seller: {sellerAfter, barrierSeller}, rig.buyer: {buyerAfter, barrierBuyer},
	} {
		if pair[1].epoch != pair[0].epoch || pair[1].seq <= pair[0].seq {
			t.Fatalf("重投后的真实资产操作未推进同归属玩家SQL提交序列 player_id=%d", player)
		}
		rig.assertProof(t, player, pair[1])
	}
	if !proto.Equal(barrierSeller.currency, sellerAfter.currency) || !proto.Equal(barrierBuyer.currency, buyerAfter.currency) {
		t.Fatal("真实重投及两次转售的新SQL提交重复扣款或增发金币")
	}
	rig.assertOriginalOwner(t, barrierSeller, barrierBuyer, original)
	for _, op := range ops {
		row := barrierSeller
		if op.PlayerId == rig.buyer {
			row = barrierBuyer
		}
		outcome, partial, _ := assetop.ClassifyPersisted(row.ledger, assetpb.AssetOpStream(op.Stream), op.StreamEpoch, op.Seq)
		if outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED || partial || !proto.Equal(tradeFullstackReceipt(t, row.ledger, op), identities[op.OpId]) {
			t.Fatal("后续真实SQL提交丢失或篡改最初资产请求的固定结局/身份摘要")
		}
	}
	if len(rig.readOps(t, offer.CommerceId)) != 4 {
		t.Fatal("资产重投生成第二套原交易资产腿")
	}
	t.Logf("跨zone寄售及四笔真实资产重投通过；两次1金币转售形成双方新SQL提交且净资产不变 commerce_id=%d；原件和17金币留在最初成交双方", offer.CommerceId)
}

func (r *tradeFullstackRig) assertOriginalOwner(t *testing.T, without, with *tradeFullstackSQL, original *playerpb.ItemEntry) {
	t.Helper()
	transferred := tradeFullstackItem(t, with.bag, r.guid)
	if tradeFullstackItem(t, without.bag, r.guid) != nil || transferred == nil || transferred.Instance == nil || transferred.BagType != 0 || transferred.ConfigId != original.ConfigId || transferred.StackSize != original.StackSize || !proto.Equal(transferred.Equip, original.Equip) || !tradeFullstackSameInstance(original.Instance, transferred.Instance) {
		t.Fatal("后续真实成交未保持原GUID唯一换主、主包位置或完整成长数据")
	}
}

func (r *tradeFullstackRig) roundTripConsignment(t *testing.T, seller, buyer uint64, requestID string, original *playerpb.ItemEntry) {
	t.Helper()
	sellerBefore, buyerBefore := r.readSQL(t, seller), r.readSQL(t, buyer)
	r.assertOriginalOwner(t, buyerBefore, sellerBefore, original)
	if len(buyerBefore.currency.Values) == 0 || buyerBefore.currency.Values[0] == 0 || len(sellerBefore.currency.Values) == 0 || sellerBefore.currency.Values[0] == math.MaxUint64 {
		t.Fatal("1金币提交屏障转售余额不满足前置")
	}
	offer, err := r.engine.CreateConsignment(r.ctx, seller, &tradepb.CreateConsignmentRequest{ClientRequestId: requestID, Assets: &tradepb.TradeAssetSelection{ItemUuids: []uint64{r.guid}}, CurrencyType: 0, Price: 1, DurationMs: 60000})
	if err != nil {
		t.Fatalf("创建正式1金币屏障寄售失败: %v", err)
	}
	tradeFullstackAwait(t, r.ctx, "正式1金币屏障寄售开放", func() (bool, error) {
		if err := r.engine.Tick(r.ctx); err != nil {
			return false, err
		}
		r.loop.Tick(r.ctx)
		if err := r.engine.Tick(r.ctx); err != nil {
			return false, err
		}
		row, err := r.engine.Repo.Get(r.ctx, offer.CommerceId)
		if err != nil {
			return false, err
		}
		if row.Status == Manual || row.Status == Cancelled {
			return false, errors.New("屏障寄售扣出未成功")
		}
		workflow, err := data.Workflow(row)
		return err == nil && workflow.Stage == "open", err
	})
	if _, err := r.engine.Mutate(r.ctx, buyer, offer.CommerceId, "buy", requestID+"-buy"); err != nil {
		t.Fatalf("正式1金币屏障买单失败: %v", err)
	}
	tradeFullstackAwait(t, r.ctx, "正式1金币屏障成交及SQL收据ACK", func() (bool, error) {
		if err := r.engine.Tick(r.ctx); err != nil {
			return false, err
		}
		r.loop.Tick(r.ctx)
		if err := r.engine.Tick(r.ctx); err != nil {
			return false, err
		}
		row, err := r.engine.Repo.Get(r.ctx, offer.CommerceId)
		if err != nil {
			return false, err
		}
		if row.Status == Manual || row.Status == Cancelled {
			return false, errors.New("屏障寄售没有全额成交")
		}
		if row.Status != Settled {
			return false, nil
		}
		for _, op := range r.readOps(t, offer.CommerceId) {
			if op.ReceiptAckPending {
				return false, nil
			}
		}
		return true, nil
	})
	sellerAfter, buyerAfter := r.readSQL(t, seller), r.readSQL(t, buyer)
	wantedSeller, wantedBuyer := proto.Clone(sellerBefore.currency).(*componentpb.CurrencyComp), proto.Clone(buyerBefore.currency).(*componentpb.CurrencyComp)
	wantedSeller.Values[0]++
	wantedBuyer.Values[0]--
	if !proto.Equal(sellerAfter.currency, wantedSeller) || !proto.Equal(buyerAfter.currency, wantedBuyer) {
		t.Fatal("正式1金币屏障成交的实际SQL余额不符")
	}
	r.assertOriginalOwner(t, sellerAfter, buyerAfter, original)
	ops := r.readOps(t, offer.CommerceId)
	if len(ops) != 4 {
		t.Fatalf("正式屏障成交应恰好生成四条资产腿，实际%d", len(ops))
	}
	for _, op := range ops {
		row := sellerAfter
		if op.PlayerId == buyer {
			row = buyerAfter
		} else if op.PlayerId != seller {
			t.Fatal("屏障交易腿属主越出两名专用角色")
		}
		outcome, partial, _ := assetop.ClassifyPersisted(row.ledger, assetpb.AssetOpStream(op.Stream), op.StreamEpoch, op.Seq)
		receipt := tradeFullstackReceipt(t, row.ledger, op)
		if op.Status != tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED || !op.Durable || !op.BusinessConsumed || op.ReceiptAckPending || outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED || partial || receipt == nil || len(receipt.GetSnapshot().GetItems()) != 0 || receipt.GetSnapshot().GetPet() != nil {
			t.Fatal("正式屏障成交没有逐腿实际SQL提交及释放后的身份摘要")
		}
	}
	r.assertProof(t, seller, sellerAfter)
	r.assertProof(t, buyer, buyerAfter)
}

func newTradeFullstackRig(t *testing.T) *tradeFullstackRig {
	t.Helper()
	if os.Getenv("TRADE_ASSET_FULLSTACK_REQUIRE") != "1" {
		t.Skip("未显式开启TRADE_ASSET_FULLSTACK_REQUIRE=1；未执行全栈到账验收")
	}
	if tradeFullstackEnv(t, "MODE") != "dev" || tradeFullstackEnv(t, "DEDICATED") != "1" {
		t.Fatal("只允许显式dev专用角色，拒绝prod或共享业务角色")
	}
	readID := func(name string) uint64 {
		n, err := strconv.ParseUint(tradeFullstackEnv(t, name), 10, 64)
		if err != nil || n == 0 {
			t.Fatalf("专用%s无效", name)
		}
		return n
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	t.Cleanup(cancel)
	rig := &tradeFullstackRig{ctx: ctx, seller: readID("SELLER_ID"), buyer: readID("BUYER_ID"), guid: readID("ORIGINAL_ITEM_GUID"), players: map[uint64]*sql.DB{}}
	if rig.seller == rig.buyer {
		t.Fatal("必须显式提供两名不同的专用角色")
	}
	for player, key := range map[uint64]string{rig.seller: "SELLER_DSN", rig.buyer: "BUYER_DSN"} {
		cfg := tradeFullstackDSN(t, key, true)
		db, err := authoritysql.Open(ctx, cfg.FormatDSN(), cfg.DBName, true, 2, authoritysql.Reader())
		if err != nil {
			t.Fatalf("实际玩家TiDB连接失败: %v", err)
		}
		rig.players[player] = db
		t.Cleanup(func() { _ = db.Close() })
		row := rig.readSQL(t, player)
		if row.epoch == 0 || row.seq == 0 {
			t.Fatal("实际玩家SQL没有dual围栏")
		}
	}
	items := tables.NewItemTableManager()
	if err := items.Load(tradeFullstackEnv(t, "TABLE_DIR"), false); err != nil {
		t.Fatalf("真实物品表未就绪: %v", err)
	}
	original := tradeFullstackItem(t, rig.readSQL(t, rig.seller).bag, rig.guid)
	if original == nil {
		t.Fatal("显式原件GUID不在卖家实际SQL，禁止自动选择替代品")
	}
	config, ok := items.FindById(original.ConfigId)
	if !ok || config.MaxStackSize != 1 {
		t.Fatal("原件必须属于真实非堆叠模板")
	}
	signer, err := assetop.NewSigner("trade", os.Getenv("MMORPG_ASSET_OP_SECRET_TRADE"))
	if err != nil {
		t.Fatal("缺少与真实scene一致的TRADE资产签名密钥")
	}
	dsAddr := tradeFullstackEnv(t, "DATASERVICE_ADDR")
	tradeFullstackLocal(t, dsAddr)
	ids, err := domainhost.OpenIDs(ctx, zrpc.RpcClientConf{Endpoints: []string{dsAddr}, Timeout: 2000}, "trade_listing", "trade_asset_op")
	if err != nil {
		t.Fatalf("真实交易号段/DataService未就绪: %v", err)
	}
	t.Cleanup(ids.Close)
	rig.ledger = &assetop.DataServiceLedger{Client: ids.Data, Timeout: 300 * time.Millisecond}
	redisAddr, etcdAddr := tradeFullstackEnv(t, "REDIS_ADDR"), tradeFullstackEnv(t, "ETCD_ADDR")
	tradeFullstackLocal(t, redisAddr)
	tradeFullstackLocal(t, etcdAddr)
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, Password: os.Getenv("TRADE_ASSET_FULLSTACK_REDIS_PASSWORD"), PoolSize: 2, MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatal("本地玩家位置Redis不可用")
	}
	etcd, err := clientv3.New(clientv3.Config{Endpoints: []string{etcdAddr}, DialTimeout: time.Second})
	if err != nil {
		t.Fatal("本地etcd配置无效")
	}
	t.Cleanup(func() { _ = etcd.Close() })
	conns := scenenode.NewConnCache()
	t.Cleanup(conns.Close)
	watcher := scenenode.NewWatcher("trade_fullstack", scenenode.SceneNodeRpcPrefix, nil, func(e scenenode.NodeEntry) { conns.Remove(e.Endpoint) })
	watchCtx, stopWatch := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); watcher.Run(watchCtx, etcd) }()
	t.Cleanup(func() {
		stopWatch()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("场景镜像未在关闭预算内退出")
		}
	})
	tradeFullstackAwait(t, ctx, "etcd首次同步", func() (bool, error) { return watcher.Synced(), nil })
	locator := &scenenode.Locator{Reader: scenenode.GoRedisReader{Client: rdb}, Watcher: watcher, Conns: conns}
	var sellerZone uint32
	for _, player := range []uint64{rig.seller, rig.buyer} {
		target, err := locator.Resolve(ctx, player)
		if err != nil || target.Location == nil || target.Location.GetOwnerEpoch() != rig.readSQL(t, player).epoch {
			t.Fatalf("专用角色不在线、正在交接或SQL归属未收敛: %v", err)
		}
		tradeFullstackLocal(t, target.Endpoint)
		if player == rig.seller {
			sellerZone = target.Location.GetZoneId()
		} else if sellerZone == 0 || target.Location.GetZoneId() == 0 || target.Location.GetZoneId() == sellerZone {
			t.Fatal("全球通服验收必须是两个不同zone的真实在线角色")
		}
		rig.assertProof(t, player, rig.readSQL(t, player))
	}
	caller := &assetop.Caller{Resolver: locator, Signer: signer}
	rig.caller = caller
	adminCfg := tradeFullstackDSN(t, "ADMIN_DSN", false)
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "trade_fullstack_" + hex.EncodeToString(nonce[:])
	opts, err := sqltest.OptionsFromEnv(adminCfg.FormatDSN(), name+"_writer", name)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := sqltest.New(ctx, t, opts)
	if err != nil {
		t.Fatal(err)
	}
	install, err := fixture.OpenSchema(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	schemaTables := data.Tables()
	report, err := schemamigrate.Up(ctx, install, schemamigrate.Options{Database: name, Tables: schemaTables})
	if err != nil || len(report.Manual) != 0 {
		t.Fatalf("正式业务schema迁移未完成: %v", err)
	}
	if err = fixture.GrantProtoTables(ctx, name, schemaTables); err != nil {
		t.Fatal(err)
	}
	db, err := fixture.Open(ctx, name, 2)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := data.NewAssetOpRepo(db, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := ops.EnsureSeqGuardRow(ctx, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	rig.engine = &Engine{Repo: &data.CommerceRepo{DB: db, Ops: ops}, IDs: ids.IDs["trade_listing"], OpIDs: ids.IDs["trade_asset_op"], Policy: Policy{Items: []uint32{original.ConfigId}, Currencies: []uint32{0}, AllowInstances: true}, Applier: caller, Ledger: rig.ledger, Now: time.Now}
	roles, err := NewRoleAuthority(tradeFullstackEnv(t, "GLOBAL_SCHEMA"))
	if err != nil {
		t.Fatal(err)
	}
	rig.engine.Roles = roles
	cfg := assetop.DefaultLoopConfig()
	cfg.RequireSQLCommit, cfg.Workers, cfg.Batch = true, 1, 4
	cfg.Streams = []assetpb.AssetOpStream{assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT, assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT}
	cfg.AbandonAfter, cfg.AuditInterval, cfg.OfflineAfterAttempts = 0, 0, 0
	rig.loop, err = assetop.NewLoop(cfg, ops, caller, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.loop.Ledger = rig.ledger
	if err := rig.loop.CheckWiring(); err != nil {
		t.Fatal(err)
	}
	return rig
}

func (r *tradeFullstackRig) readSQL(t *testing.T, player uint64) *tradeFullstackSQL {
	t.Helper()
	row := &tradeFullstackSQL{currency: &componentpb.CurrencyComp{}, bag: &playerpb.BagAllData{}, ledger: &componentpb.PlayerAssetOpLedgerComp{}}
	var currency, bag, ledger []byte
	if err := r.players[player].QueryRowContext(r.ctx, "SELECT currency,bag_component,asset_op_ledger,fence_owner_epoch,fence_save_seq FROM player_database WHERE player_id=?", player).Scan(&currency, &bag, &ledger, &row.epoch, &row.seq); err != nil {
		t.Fatalf("直接读取实际玩家SQL失败: %v", err)
	}
	if proto.Unmarshal(currency, row.currency) != nil || proto.Unmarshal(bag, row.bag) != nil || proto.Unmarshal(ledger, row.ledger) != nil {
		t.Fatal("实际玩家SQL资产/账本编码无效")
	}
	return row
}

func (r *tradeFullstackRig) readOps(t *testing.T, commerce uint64) []*tradepb.TradeAssetOpRecord {
	t.Helper()
	rows, err := r.engine.Repo.DB.QueryContext(r.ctx, "SELECT op_id,player_id,stream,stream_epoch,seq,status,durable,escrow_snapshot,receipt_ack_pending,business_consumed,tx_type,payload FROM trade_asset_op WHERE ref_kind=2 AND ref_id=? ORDER BY op_id", commerce)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []*tradepb.TradeAssetOpRecord
	for rows.Next() {
		op := &tradepb.TradeAssetOpRecord{}
		if err := rows.Scan(&op.OpId, &op.PlayerId, &op.Stream, &op.StreamEpoch, &op.Seq, &op.Status, &op.Durable, &op.EscrowSnapshot, &op.ReceiptAckPending, &op.BusinessConsumed, &op.TxType, &op.Payload); err != nil {
			t.Fatal(err)
		}
		result = append(result, op)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func (r *tradeFullstackRig) assertProof(t *testing.T, player uint64, row *tradeFullstackSQL) {
	t.Helper()
	proof, err := r.ledger.ReadSQLCommittedLedger(r.ctx, player)
	if err != nil || proof == nil || !proto.Equal(proof, row.ledger) {
		t.Fatalf("DataService提交证据必须与真实玩家SQL账本一致: %v", err)
	}
}

func tradeFullstackRequest(t *testing.T, op *tradepb.TradeAssetOpRecord) *assetpb.AssetOpRequest {
	t.Helper()
	bundle := &assetpb.AssetBundle{}
	if proto.Unmarshal(op.Payload, bundle) != nil {
		t.Fatal("正式交易意图payload编码损坏")
	}
	return (&assetop.Op{PlayerID: op.PlayerId, Stream: assetpb.AssetOpStream(op.Stream), StreamEpoch: op.StreamEpoch, Seq: op.Seq, CorrelationID: op.OpId, TxType: op.TxType, Bundle: bundle}).Request()
}

func tradeFullstackReceipt(t *testing.T, ledger *componentpb.PlayerAssetOpLedgerComp, op *tradepb.TradeAssetOpRecord) *componentpb.AssetOpReceipt {
	t.Helper()
	req := tradeFullstackRequest(t, op)
	req.Auth = &assetpb.AssetOpAuth{Caller: "trade"}
	digest := sha256.Sum256(assetop.Canonical(assetop.RPCDebit, req, 0))
	var selected *componentpb.AssetOpStreamLedger
	for _, stream := range ledger.GetStreams() {
		if uint32(stream.GetStream()) != op.Stream {
			continue
		}
		if selected != nil || stream.GetStreamEpoch() != op.StreamEpoch {
			return nil
		}
		selected = stream
	}
	var found *componentpb.AssetOpReceipt
	for _, receipt := range selected.GetReceipts() {
		if receipt.GetSeq() != op.Seq {
			continue
		}
		if found != nil || receipt.GetCorrelationId() != op.OpId || !bytes.Equal(receipt.GetRequestDigest(), digest[:]) {
			return nil
		}
		found = receipt
	}
	return found
}

func tradeFullstackSameInstance(a, b *componentpb.ItemComp) bool {
	left, right := proto.Clone(a).(*componentpb.ItemComp), proto.Clone(b).(*componentpb.ItemComp)
	left.AcquireSeq, right.AcquireSeq = 0, 0
	return proto.Equal(left, right)
}

func tradeFullstackItem(t *testing.T, bag *playerpb.BagAllData, guid uint64) *playerpb.ItemEntry {
	t.Helper()
	var found *playerpb.ItemEntry
	visit := func(items []*playerpb.ItemEntry) {
		for _, item := range items {
			if item.GetItemUuid() == guid {
				if found != nil {
					t.Fatal("实际SQL中同一GUID出现两次")
				}
				found = item
			}
		}
	}
	visit(bag.GetItems())
	for _, dynamic := range bag.GetDynamicBags() {
		visit(dynamic.GetItems())
	}
	return found
}

func tradeFullstackEnv(t *testing.T, suffix string) string {
	t.Helper()
	name := "TRADE_ASSET_FULLSTACK_" + suffix
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("已要求全栈验收，缺少%s", name)
	}
	return value
}

func tradeFullstackLocal(t *testing.T, address string) {
	t.Helper()
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
		t.Fatal("全栈验收只允许显式本机回环开发服务地址")
	}
}

func tradeFullstackDSN(t *testing.T, name string, hasDatabase bool) *mysql.Config {
	t.Helper()
	cfg, err := mysql.ParseDSN(tradeFullstackEnv(t, name))
	if err != nil || cfg.Net != "tcp" || (cfg.DBName != "") != hasDatabase {
		t.Fatalf("%s必须使用本地tcp且库名约束不符；不打印DSN", name)
	}
	tradeFullstackLocal(t, cfg.Addr)
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = time.Second, 5*time.Second, 5*time.Second
	return cfg
}

func tradeFullstackAwait(t *testing.T, ctx context.Context, stage string, check func() (bool, error)) {
	t.Helper()
	for {
		ok, err := check()
		if err != nil {
			t.Fatalf("%s失败: %v", stage, err)
		}
		if ok {
			return
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("%s超过全栈验收截止时间", stage)
		case <-timer.C:
		}
	}
}
