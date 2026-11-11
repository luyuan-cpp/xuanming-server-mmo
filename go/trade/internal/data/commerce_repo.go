package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"google.golang.org/protobuf/proto"
	assetpb "proto/common/asset"
	tradepb "proto/trade"
	"shared/assetop"
	"shared/workshard"
	"strconv"
	"strings"
)

// CommerceRepo只存合同与托管证据。玩家余额/实例的权威始终在scene玩家SQL存档。
// 所有写者统一锁序：guard(仅撮合)→commerce升序→seq升序→outbox；建seq的独立短事务先完成。
type CommerceRepo struct {
	DB  *sql.DB
	Ops *AssetOpRepo
}

const CommerceColumns = "commerce_id,request_key,request_digest,kind,status,seller_player_id,buyer_player_id,market_id,item_config_id,side,unit_price,quantity,filled_quantity,currency_type,expires_ms,created_ms,updated_ms,version,workflow_proto,parent_id,counterparty_id,on_book"

type commerceScanner interface{ Scan(...any) error }

func ScanCommerce(row commerceScanner) (*tradepb.TradeCommerceRecord, error) {
	r := &tradepb.TradeCommerceRecord{}
	err := row.Scan(&r.CommerceId, &r.RequestKey, &r.RequestDigest, &r.Kind, &r.Status, &r.SellerPlayerId, &r.BuyerPlayerId, &r.MarketId, &r.ItemConfigId, &r.Side, &r.UnitPrice, &r.Quantity, &r.FilledQuantity, &r.CurrencyType, &r.ExpiresMs, &r.CreatedMs, &r.UpdatedMs, &r.Version, &r.WorkflowProto, &r.ParentId, &r.CounterpartyId, &r.OnBook)
	return r, err
}

// OnBook 报告合同是不是挂在材料盘口上的订单:kind=4 的买卖单(side≠0),托管已完成、workflow 处在 market_open。
//
// trade_commerce.on_book 是它的持久投影,只由 Insert / Save 按本函数与 workflow_proto 在同一条语句里写出,两者不会分叉;
// 撮合选对手单与 taker 预检(commerce 包 market.go)、盘口深度(logic.GetMarketDepth)都按这一列。
// 托管阶段(stage=escrow)的订单 status 同样是 3:只看 status 会把它选成最优对手单,撮合事务发现阶段不对就不成交,
// 排在它后面、已经上盘的订单一单也撮不上(jubaozhai-market.md §15.9)。
// 剩余量、状态、撤单不并进来:它们各有自己的列,也有不经 workflow 的写法(SaveStatus 只改状态);
// 撮合提交点在锁内复核 workflow,不以这一列为准。
func OnBook(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) bool {
	return r.GetKind() == 4 && r.GetSide() != 0 && w.GetStage() == "market_open"
}
func (r *CommerceRepo) Get(ctx context.Context, id uint64) (*tradepb.TradeCommerceRecord, error) {
	return ScanCommerce(r.DB.QueryRowContext(ctx, "SELECT "+CommerceColumns+" FROM trade_commerce WHERE commerce_id=?", id))
}
func (r *CommerceRepo) ByRequest(ctx context.Context, key string) (*tradepb.TradeCommerceRecord, error) {
	return ScanCommerce(r.DB.QueryRowContext(ctx, "SELECT "+CommerceColumns+" FROM trade_commerce WHERE request_key=?", key))
}
func (r *CommerceRepo) Lock(ctx context.Context, tx *sql.Tx, id uint64) (*tradepb.TradeCommerceRecord, error) {
	return ScanCommerce(tx.QueryRowContext(ctx, "SELECT "+CommerceColumns+" FROM trade_commerce WHERE commerce_id=? FOR UPDATE", id))
}
// Insert 写一条新合同。on_book 不取 v 上的值,按 v.WorkflowProto 现算(OnBook):解不开的 workflow 不进盘口,
// 插入照常进行,坏 workflow 由合同推进按 corrupt 隔离。
func (r *CommerceRepo) Insert(ctx context.Context, tx *sql.Tx, v *tradepb.TradeCommerceRecord) error {
	w, decodeErr := Workflow(v)
	onBook := decodeErr == nil && OnBook(v, w)
	_, err := tx.ExecContext(ctx, "INSERT INTO trade_commerce ("+CommerceColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", v.CommerceId, v.RequestKey, v.RequestDigest, v.Kind, v.Status, v.SellerPlayerId, v.BuyerPlayerId, v.MarketId, v.ItemConfigId, v.Side, v.UnitPrice, v.Quantity, v.FilledQuantity, v.CurrencyType, v.ExpiresMs, v.CreatedMs, v.UpdatedMs, v.Version, v.WorkflowProto, v.ParentId, v.CounterpartyId, onBook)
	if err != nil {
		return err
	}
	v.OnBook = onBook
	return nil
}

// Save 按版本条件重存合同与 workflow。on_book 不取 v 上的值,按要写的 w 现算(OnBook),与 workflow_proto 同一条语句落库。
// seller_player_id 只有人工改绑(commerce.ResolveManual 的 redirect)会改,其余写者原样带回读到的值。
func (r *CommerceRepo) Save(ctx context.Context, tx *sql.Tx, v *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, now uint64) error {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(w)
	if err != nil {
		return err
	}
	onBook := OnBook(v, w)
	result, err := tx.ExecContext(ctx, "UPDATE trade_commerce SET status=?,seller_player_id=?,buyer_player_id=?,filled_quantity=?,expires_ms=?,updated_ms=?,version=version+1,workflow_proto=?,on_book=? WHERE commerce_id=? AND version=?", v.Status, v.SellerPlayerId, v.BuyerPlayerId, v.FilledQuantity, v.ExpiresMs, now, raw, onBook, v.CommerceId, v.Version)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("交易版本已变化")
	}
	v.Version++
	v.WorkflowProto = raw
	v.UpdatedMs = now
	v.OnBook = onBook
	return nil
}
// SaveStatus 只改状态列(版本 +1),workflow_proto 原样保留。只给"合同解不开、走不了 Save"的隔离用:
// 坏字节留作人工取证的原件(2026-10-20,第一轮审计 §2.5 go-asset-services#0)。
func (r *CommerceRepo) SaveStatus(ctx context.Context, tx *sql.Tx, v *tradepb.TradeCommerceRecord, status uint32, now uint64) error {
	result, err := tx.ExecContext(ctx, "UPDATE trade_commerce SET status=?,updated_ms=?,version=version+1 WHERE commerce_id=? AND version=?", status, now, v.CommerceId, v.Version)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("交易版本已变化")
	}
	v.Status = status
	v.Version++
	v.UpdatedMs = now
	return nil
}

// Defer 把合同的下次到期推迟到 untilMs(写 updated_ms,见 Due),返回本次是否推迟成功。
//
// 只在行仍是 version 那一版时生效:被别的写者推进过的行由新状态决定何时到期,绝不拿旧观察去改它(不旧盖新)。
// 不改 version:推迟不是业务变化,客户端看到的版本号不该因后台重试跳动;也因此并发的 Save 不会被它挡住 ——
// Save 在同一行锁上排在它后面,读到的仍是同一 version,照常提交并把 updated_ms 改回"现在"(推进优先于推迟)。
// 锁序与 Save 相同:先主键点锁合同行(Lock),再带版本条件的主键点改。
func (r *CommerceRepo) Defer(ctx context.Context, id, version, untilMs uint64) (bool, error) {
	deferred := false
	err := r.Tx(ctx, func(tx *sql.Tx) error {
		deferred = false
		cur, err := r.Lock(ctx, tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if cur.Version != version {
			return nil
		}
		result, err := tx.ExecContext(ctx, "UPDATE trade_commerce SET updated_ms=? WHERE commerce_id=? AND version=?", untilMs, id, version)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		deferred = n == 1
		return nil
	})
	return deferred, err
}

// RescheduleTx 只把合同的下次到期改成 dueMs,不改版本、不重写 workflow。给推进一步之后合同没有任何变化时用
// (commerce.Engine.advance,缺口清单第 16 条,docs/design/jubaozhai-market.md §15.8)。
//
// 调用方必须已在 tx 里主键点锁了这一行(Lock),所以版本条件必然命中;影响 0 行只说明 updated_ms 本来就是这个值。
// 不改版本是有意的:后台的一次空转不是业务变化,玩家按刚读到的版本提交的购买、取消不该因此撞乐观冲突;
// 它们的 Save 在同一行锁上排在后面,照常提交并把 updated_ms 写回现在(立即到期)。
func (r *CommerceRepo) RescheduleTx(ctx context.Context, tx *sql.Tx, id, version, dueMs uint64) error {
	_, err := tx.ExecContext(ctx, "UPDATE trade_commerce SET updated_ms=? WHERE commerce_id=? AND version=?", dueMs, id, version)
	return err
}

// Wake 把仍在推进中的合同的下次到期提前到 atMs:只提前、不推后,不改版本。给「别处的事件让这条合同可以往前走」用:
// 撮合失败后下一轮重撮、成交子单收尾后唤醒母单(commerce.Engine),以及资产指令终结后唤醒腿所属的合同
// (AssetOpRepo.wakeContractOf,同一条语句)。
//
// 自动提交的单条主键点改,只锁这一行,不在任何别的事务里,所以与合同推进的锁序不成环。行已不在推进状态
// (结案、转人工)或本来就更早到期时影响 0 行,不是错误。
func (r *CommerceRepo) Wake(ctx context.Context, id, atMs uint64) error {
	return wakeCommerce(ctx, r.DB, id, atMs)
}

// wakeCommerce 是 Wake 的语句,资产指令终结一侧(AssetOpRepo)也用它,两边不各写一份。
func wakeCommerce(ctx context.Context, db *sql.DB, id, atMs uint64) error {
	_, err := db.ExecContext(ctx, "UPDATE trade_commerce SET updated_ms=? WHERE commerce_id=? AND updated_ms>? AND status IN ("+dueStatuses+")", atMs, id, atMs)
	return err
}

func (r *CommerceRepo) Tx(ctx context.Context, body func(*sql.Tx) error) error {
	return assetop.WithTxRetry(ctx, r.DB, 2, IsRetryableTxError, body)
}

// CommerceCursor 是合同列表的翻页游标。commerce_id 是打散过的,不代表先后,所以按 (created_ms, commerce_id) 翻页。
// 对客户端它只是一个不透明的字符串。
type CommerceCursor struct {
	CreatedMs  uint64
	CommerceId uint64
}

// ParseCommerceCursor 解析 CommerceCursor.String 的结果。空串或格式不对都从头开始。
func ParseCommerceCursor(raw string) CommerceCursor {
	created, id, ok := strings.Cut(raw, ":")
	if !ok {
		return CommerceCursor{}
	}
	createdMs, err := strconv.ParseUint(created, 10, 64)
	if err != nil {
		return CommerceCursor{}
	}
	commerceID, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return CommerceCursor{}
	}
	return CommerceCursor{CreatedMs: createdMs, CommerceId: commerceID}
}

func (c CommerceCursor) String() string {
	return strconv.FormatUint(c.CreatedMs, 10) + ":" + strconv.FormatUint(c.CommerceId, 10)
}

// CommerceCursorOf 是排在 r 之后那一页的游标。
func CommerceCursorOf(r *tradepb.TradeCommerceRecord) CommerceCursor {
	return CommerceCursor{CreatedMs: r.GetCreatedMs(), CommerceId: r.GetCommerceId()}
}

func (r *CommerceRepo) List(ctx context.Context, player uint64, after CommerceCursor, limit int) ([]*tradepb.TradeCommerceRecord, error) {
	return r.Query(ctx, "SELECT "+CommerceColumns+" FROM trade_commerce WHERE (created_ms>? OR (created_ms=? AND commerce_id>?)) AND kind<>1 AND (seller_player_id=? OR buyer_player_id=?) ORDER BY created_ms,commerce_id LIMIT ?", after.CreatedMs, after.CreatedMs, after.CommerceId, player, player, limit)
}
// Due 取到期待推进的合同。updated_ms 兼作调度键:Save 写成当前时刻(立即到期),Defer 写成将来时刻(推迟),
// 所以只取 updated_ms<=nowMs 的行。以前不过滤到期时刻,失败行与等待拍品结算的持有竞价 updated_ms 不变,
// 每轮都排在队头,堆满 limit 之后全服合同停摆(2026-10-20,第一轮审计 §2.5 go-asset-services#0)。
// updated_ms 不进客户端视图(logic.orderView / marketView / lotView 都不映射它),改它不动任何对外契约。
// 各副本按本机时钟算 nowMs,时钟偏差 δ 只会让行晚到期至多 δ,不会漏行。
//
// owned 把行限定在本副本负责的片里(commerce_id % 64,缺口清单第 16 条,docs/design/jubaozhai-market.md §15.8),
// 放在 LIMIT 之前:各副本只取自己的片,不再读同一个队头在行锁上互撞。零值不过滤。
func (r *CommerceRepo) Due(ctx context.Context, nowMs uint64, limit int, owned workshard.Filter) ([]*tradepb.TradeCommerceRecord, error) {
	args := append([]any{nowMs}, owned.Args()...)
	return r.Query(ctx, "SELECT "+CommerceColumns+" FROM trade_commerce WHERE status IN ("+dueStatuses+") AND updated_ms<=? AND "+owned.SQL()+" ORDER BY updated_ms,commerce_id LIMIT ?", append(args, limit)...)
}

// dueStatuses 是还在推进中的合同状态(Waiting、Escrowing、Delivering、CancelPending,以及官方退款与人民币寄售用的 8)。
// Due 与 Wake 共用,不能各写一份:Wake 只唤醒 Due 会取的行。
const dueStatuses = "1,3,4,6,8"
// ListManual 列转人工的合同(status=10),最早转人工的在前,给人工处置入口用。按 (updated_ms, commerce_id) 翻页,
// 从 (afterUpdatedMs, afterID) 之后取;两个都是 0 从头取。走 (status, updated_ms) 索引:转人工之后 updated_ms 不再变,
// 直到人工处理。
func (r *CommerceRepo) ListManual(ctx context.Context, afterUpdatedMs, afterID uint64, limit int) ([]*tradepb.TradeCommerceRecord, error) {
	return r.Query(ctx, "SELECT "+CommerceColumns+" FROM trade_commerce WHERE status=10 AND (updated_ms>? OR (updated_ms=? AND commerce_id>?)) ORDER BY updated_ms,commerce_id LIMIT ?", afterUpdatedMs, afterUpdatedMs, afterID, limit)
}
func (r *CommerceRepo) Query(ctx context.Context, query string, args ...any) ([]*tradepb.TradeCommerceRecord, error) {
	rows, err := r.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tradepb.TradeCommerceRecord
	for rows.Next() {
		v, err := ScanCommerce(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
// LegOutcome 是一条合同腿对应 outbox 行的结局(LegResult 的返回值)。
type LegOutcome struct {
	Status     tradepb.TradeAssetOpStatus
	Snapshot   []byte
	ResolvedBy string
	// FinalMs 是终态行的终结时刻(next_attempt_ms:终结、人工终结、放弃同写);未决行上这一列是下次投递时刻,没有这层意思。
	// 人工改绑时拿它对照过户时刻,判断一条已到账的发放是不是到了新主手里(jubaozhai-market.md §15.7.1 第 1 类)。
	FinalMs uint64
}

// Pending 报告这一行还没有终局,合同等下一轮。
func (o LegOutcome) Pending() bool {
	return o.Status == tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING
}

// Final 报告 Status 是否为五个终态之一。**不看 durable**(2026-10-20,第一轮审计 §2.5 go-asset-services#1)。
//
// 终态的三个写者都以 status=PENDING 为 CAS 条件,一行只终结一次,终结之后 status 不再变化:
//   - Finalize(scene 落盘结局):同一条语句置 durable=1;
//   - ResolveManually(人工终结):刻意不置 durable,让对账分得清"scene 答过"与"人判定的";
//   - Abandon(自动放弃,只放弃 TRADE_DEBIT):刻意不置 durable,依据是已落盘账本确认 scene 从未见过这个 seq。
//
// 以前合同推进只认 durable=1,后两种终局永远读不到:托管扣款腿被放弃或人工终结后合同停在 pay / escrow,
// 另一方托管的资产被扣住。UNSPECIFIED 与未知值既不是终局也不是待办,调用方按坏数据处理。
func (o LegOutcome) Final() bool {
	switch o.Status {
	case tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED,
		tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_REJECTED,
		tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABORTED,
		tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL,
		tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABANDONED:
		return true
	default:
		return false
	}
}

// Source 只给日志区分终局来源("scene" / "manual" / "abandoned");合同推进不按来源分支。
func (o LegOutcome) Source() string {
	switch {
	case o.Status == tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABANDONED:
		return "abandoned"
	case o.ResolvedBy != "":
		return "manual"
	default:
		return "scene"
	}
}

// LegResult 在调用方(已锁合同行)的事务里普通读一条腿的结局,不锁 op 行:终态行的 status 不再变化,
// 读到 PENDING 就等下一轮。Finalize / ResolveManually / Abandon 只点锁 op 行、从不碰合同行,
// 与"合同行 → op 行"的引擎锁序不成环。行不存在时回 sql.ErrNoRows,由调用方按坏数据处理。
// 人工恢复工具只读查看时传 *sql.DB。
func (r *CommerceRepo) LegResult(ctx context.Context, q rowQuerier, id uint64) (LegOutcome, error) {
	var status int32
	var resolvedBy sql.NullString // MEDIUMTEXT 列:按可空扫,不赌建表口径
	var out LegOutcome
	var nextAttemptMs uint64
	err := q.QueryRowContext(ctx, "SELECT status,escrow_snapshot,resolved_by,next_attempt_ms FROM trade_asset_op WHERE op_id=?", id).Scan(&status, &out.Snapshot, &resolvedBy, &nextAttemptMs)
	out.Status = tradepb.TradeAssetOpStatus(status)
	out.ResolvedBy = resolvedBy.String
	if out.Final() {
		out.FinalMs = nextAttemptMs
	}
	return out, err
}
func Workflow(v *tradepb.TradeCommerceRecord) (*tradepb.TradeCommerceWorkflow, error) {
	w := &tradepb.TradeCommerceWorkflow{}
	if err := proto.Unmarshal(v.WorkflowProto, w); err != nil {
		return nil, fmt.Errorf("交易合同损坏: %w", err)
	}
	return w, nil
}

// ACK只读已提交的托管快照；完成标志不能先于远端玩家SQL证明写入。
// 按 op_id 翻页(op_id > afterOpID):离线卖家的 ACK 一直 done=false,只从表头取会让它们永远占住名额(2026-10-20)。
// payload 解不开的行照样按序返回、Bundle 为 nil,由调用方跳过并告警:以前整页报错返回,翻页游标停在它前面,
// 一行坏数据让全服收据 ACK 停摆(2026-10-20,第一轮审计 §2.5 go-asset-services#0)。解得开的行 Bundle 一律非 nil。
// owned 把行限定在本副本负责的片里(op_id % 64,缺口清单第 16 条),放在 LIMIT 之前;零值不过滤。
func (r *CommerceRepo) AckDue(ctx context.Context, afterOpID uint64, limit int, owned workshard.Filter) ([]assetop.Op, error) {
	args := append([]any{afterOpID}, owned.Args()...)
	rows, err := r.DB.QueryContext(ctx, "SELECT op_id,player_id,stream,stream_epoch,seq,tx_type,payload FROM trade_asset_op WHERE receipt_ack_pending=1 AND status=2 AND durable=1 AND op_id>? AND "+owned.SQL()+" ORDER BY op_id LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []assetop.Op
	for rows.Next() {
		var op assetop.Op
		var payload []byte
		if err := rows.Scan(&op.OpID, &op.PlayerID, &op.Stream, &op.StreamEpoch, &op.Seq, &op.TxType, &payload); err != nil {
			return nil, err
		}
		op.CorrelationID = op.OpID
		op.Bundle = &assetpb.AssetBundle{}
		if err := proto.Unmarshal(payload, op.Bundle); err != nil {
			op.Bundle = nil
		}
		out = append(out, op)
	}
	return out, rows.Err()
}
func (r *CommerceRepo) Acked(ctx context.Context, id uint64) error {
	_, err := r.DB.ExecContext(ctx, "UPDATE trade_asset_op SET receipt_ack_pending=0 WHERE op_id=? AND status=2 AND durable=1", id)
	return err
}
