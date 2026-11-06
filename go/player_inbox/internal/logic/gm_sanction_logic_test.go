package logic

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"player_inbox/internal/constants"
	"player_inbox/internal/inbox"

	inboxpb "proto/player_inbox"
	"shared/rolefence"
	"shared/serverbase"

	"github.com/zeromicro/go-zero/core/logx/logtest"
	"google.golang.org/protobuf/proto"
)

// fakeSanctionStore 记录 Enqueue 的入参;调 inTx(nil, seq) 模拟「事务内写审计」。
type fakeSanctionStore struct {
	registry *inbox.Registry
	params   []inbox.EnqueueParams
	result   inbox.EnqueueResult
	err      error
	auditErr error
}

func (f *fakeSanctionStore) Registry() *inbox.Registry { return f.registry }

func (f *fakeSanctionStore) Enqueue(_ context.Context, p inbox.EnqueueParams, inTx func(*sql.Tx, uint64) error) (inbox.EnqueueResult, error) {
	f.params = append(f.params, p)
	if f.err != nil {
		return inbox.EnqueueResult{}, f.err
	}
	if f.result.Created && inTx != nil {
		if err := inTx(nil, f.result.Seq); err != nil {
			return inbox.EnqueueResult{}, err
		}
	}
	return f.result, nil
}

func newDeps(t *testing.T, store *fakeSanctionStore, audits *[]*inboxpb.PlayerInboxAudit) GmSanctionDeps {
	t.Helper()
	reg, err := inbox.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	store.registry = reg
	return GmSanctionDeps{
		Store: store,
		Audit: func(_ context.Context, _ *sql.Tx, a *inboxpb.PlayerInboxAudit) error {
			if store.auditErr != nil {
				return store.auditErr
			}
			*audits = append(*audits, a)
			return nil
		},
		Now: func() time.Time { return time.UnixMilli(1_800_000_000_000) },
	}
}

func validRequest() *inboxpb.GmSanctionPlayerRequest {
	return &inboxpb.GmSanctionPlayerRequest{
		PlayerId:   42,
		RequestId:  9001,
		IssuedAtMs: 1_799_999_999_000,
		Action: &inboxpb.GmSanctionPlayerRequest_AttachDebt{AttachDebt: &inboxpb.GmCurrencyDebtPayload{
			CurrencyType: 1, OweAmount: 500, Reason: "补缴 #123", GmOperator: "spoofed", ExpiresAt: 1_900_000_000,
		}},
		Reason:   "工单 #123 多发返还",
		Operator: "ops_alice",
	}
}

// TestGmSanctionAttachDebtOverridesOperator:gm_operator 由服务端用 operator 覆盖后再入箱(设计 §3.4),
// 不改调用方的请求对象;审计行同事务写入,带 operator / 对端地址 / 理由 / seq / 域。
func TestGmSanctionAttachDebtOverridesOperator(t *testing.T) {
	logtest.Discard(t)
	store := &fakeSanctionStore{result: inbox.EnqueueResult{Seq: 77, Created: true, State: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING}}
	var audits []*inboxpb.PlayerInboxAudit
	req := validRequest()
	resp := GmSanctionPlayer(context.Background(), newDeps(t, store, &audits), req, "10.0.0.5:51234")

	if resp.GetErrorMessage() != nil || resp.GetSeq() != 77 || !resp.GetCreated() {
		t.Fatalf("resp = %+v", resp)
	}
	if req.GetAttachDebt().GetGmOperator() != "spoofed" {
		t.Fatal("不得改调用方的请求对象")
	}
	if len(store.params) != 1 {
		t.Fatalf("Enqueue 调用 %d 次", len(store.params))
	}
	p := store.params[0]
	if p.Type != inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_ATTACH_CURRENCY_DEBT ||
		p.Producer != inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_GM ||
		p.PlayerID != 42 || p.RequestID != 9001 || p.IssuedAtMs != req.GetIssuedAtMs() ||
		p.Operator != "ops_alice" || p.Note != req.GetReason() {
		t.Fatalf("EnqueueParams = %+v", p)
	}
	debt := &inboxpb.GmCurrencyDebtPayload{}
	if err := proto.Unmarshal(p.Payload, debt); err != nil {
		t.Fatal(err)
	}
	if debt.GetGmOperator() != "ops_alice" || debt.GetOweAmount() != 500 {
		t.Fatalf("载荷 = %+v,gm_operator 应被覆盖为 ops_alice", debt)
	}
	if len(audits) != 1 {
		t.Fatalf("审计行 %d 条", len(audits))
	}
	a := audits[0]
	if a.GetSeq() != 77 || a.GetOperator() != "ops_alice" || a.GetPeerAddr() != "10.0.0.5:51234" || a.GetReason() != req.GetReason() ||
		a.GetDomain() != uint32(inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_SCENE_GM_SANCTION) ||
		a.GetAction() != uint32(inboxpb.PlayerInboxAuditAction_PLAYER_INBOX_AUDIT_SANCTION) || a.GetCreatedMs() != 1_800_000_000_000 {
		t.Fatalf("审计行 = %+v", a)
	}
}

// TestGmSanctionPayloadIsDeterministic:同一请求两次得到同一份载荷字节(内容摘要包含载荷,重试必须得到 DUPLICATE)。
func TestGmSanctionPayloadIsDeterministic(t *testing.T) {
	logtest.Discard(t)
	store := &fakeSanctionStore{result: inbox.EnqueueResult{Seq: 1}}
	var audits []*inboxpb.PlayerInboxAudit
	deps := newDeps(t, store, &audits)
	GmSanctionPlayer(context.Background(), deps, validRequest(), "p")
	GmSanctionPlayer(context.Background(), deps, validRequest(), "p")
	if string(store.params[0].Payload) != string(store.params[1].Payload) {
		t.Fatal("两次载荷字节不同")
	}
}

func TestGmSanctionActionsMapToTypes(t *testing.T) {
	logtest.Discard(t)
	cases := map[inboxpb.PlayerInboxMessageType]func(r *inboxpb.GmSanctionPlayerRequest){
		inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN: func(r *inboxpb.GmSanctionPlayerRequest) {
			r.Action = &inboxpb.GmSanctionPlayerRequest_BlockGain{BlockGain: &inboxpb.GmCurrencyGainPayload{CurrencyType: 2}}
		},
		inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN: func(r *inboxpb.GmSanctionPlayerRequest) {
			r.Action = &inboxpb.GmSanctionPlayerRequest_UnblockGain{UnblockGain: &inboxpb.GmCurrencyGainPayload{CurrencyType: 2}}
		},
		inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_BLOCK_ITEM_GAIN: func(r *inboxpb.GmSanctionPlayerRequest) {
			r.Action = &inboxpb.GmSanctionPlayerRequest_BlockItemGain{BlockItemGain: &inboxpb.GmItemGainPayload{ItemConfigId: 10}}
		},
		inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_UNBLOCK_ITEM_GAIN: func(r *inboxpb.GmSanctionPlayerRequest) {
			r.Action = &inboxpb.GmSanctionPlayerRequest_UnblockItemGain{UnblockItemGain: &inboxpb.GmItemGainPayload{ItemConfigId: 10}}
		},
	}
	for want, set := range cases {
		store := &fakeSanctionStore{result: inbox.EnqueueResult{Seq: 5, Created: true}}
		var audits []*inboxpb.PlayerInboxAudit
		req := validRequest()
		set(req)
		GmSanctionPlayer(context.Background(), newDeps(t, store, &audits), req, "p")
		if len(store.params) != 1 || store.params[0].Type != want {
			t.Errorf("want type %v, got %+v", want, store.params)
		}
	}
}

// TestGmSanctionItemGainEnqueuesConfigIDWithAuditAndStableBytes:单玩家物品获取封禁(设计 §2.2)走与货币封锁同一条入箱路径:
// 同一个 GM 生产方与 (producer_kind, request_id) 幂等键、同事务审计(域 = SCENE_GM_SANCTION)、确定性载荷字节
// (同一请求重试得到 DUPLICATE 而不是 CONFLICT)。载荷原样带配置号,不改写调用方的请求。
func TestGmSanctionItemGainEnqueuesConfigIDWithAuditAndStableBytes(t *testing.T) {
	logtest.Discard(t)
	for name, tc := range map[string]struct {
		set  func(r *inboxpb.GmSanctionPlayerRequest)
		want inboxpb.PlayerInboxMessageType
	}{
		"封锁": {func(r *inboxpb.GmSanctionPlayerRequest) {
			r.Action = &inboxpb.GmSanctionPlayerRequest_BlockItemGain{BlockItemGain: &inboxpb.GmItemGainPayload{ItemConfigId: 30045}}
		}, inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_BLOCK_ITEM_GAIN},
		"解封": {func(r *inboxpb.GmSanctionPlayerRequest) {
			r.Action = &inboxpb.GmSanctionPlayerRequest_UnblockItemGain{UnblockItemGain: &inboxpb.GmItemGainPayload{ItemConfigId: 30045}}
		}, inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_UNBLOCK_ITEM_GAIN},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeSanctionStore{result: inbox.EnqueueResult{Seq: 88, Created: true, State: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING}}
			var audits []*inboxpb.PlayerInboxAudit
			deps := newDeps(t, store, &audits)
			req := validRequest()
			tc.set(req)
			resp := GmSanctionPlayer(context.Background(), deps, req, "10.0.0.5:51234")
			if resp.GetErrorMessage() != nil || resp.GetSeq() != 88 || !resp.GetCreated() {
				t.Fatalf("resp = %+v", resp)
			}
			if len(store.params) != 1 {
				t.Fatalf("Enqueue 调用 %d 次", len(store.params))
			}
			p := store.params[0]
			if p.Type != tc.want || p.Producer != inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_GM ||
				p.PlayerID != 42 || p.RequestID != 9001 || p.Operator != "ops_alice" || p.Note != req.GetReason() {
				t.Fatalf("EnqueueParams = %+v", p)
			}
			payload := &inboxpb.GmItemGainPayload{}
			if err := proto.Unmarshal(p.Payload, payload); err != nil || payload.GetItemConfigId() != 30045 {
				t.Fatalf("载荷 = %+v, err = %v,want item_config_id=30045", payload, err)
			}
			if len(audits) != 1 || audits[0].GetSeq() != 88 || audits[0].GetOperator() != "ops_alice" ||
				audits[0].GetPeerAddr() != "10.0.0.5:51234" ||
				audits[0].GetDomain() != uint32(inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_SCENE_GM_SANCTION) ||
				audits[0].GetAction() != uint32(inboxpb.PlayerInboxAuditAction_PLAYER_INBOX_AUDIT_SANCTION) {
				t.Fatalf("审计行 = %+v", audits)
			}
			// 原样重试:载荷字节不变(内容摘要包含载荷)。
			retry := validRequest()
			tc.set(retry)
			GmSanctionPlayer(context.Background(), deps, retry, "10.0.0.5:51234")
			if len(store.params) != 2 || string(store.params[0].Payload) != string(store.params[1].Payload) {
				t.Fatal("同一请求两次的载荷字节不同,重试会被判成幂等冲突")
			}
		})
	}
}

// TestGmSanctionRejectsBeforeEnqueue:字段不合法时不碰存储。
func TestGmSanctionRejectsBeforeEnqueue(t *testing.T) {
	logtest.Discard(t)
	cases := []struct {
		name   string
		mutate func(r *inboxpb.GmSanctionPlayerRequest)
		want   uint32
	}{
		{"player_id 为 0", func(r *inboxpb.GmSanctionPlayerRequest) { r.PlayerId = 0 }, constants.ErrInvalidParameter},
		{"request_id 为 0", func(r *inboxpb.GmSanctionPlayerRequest) { r.RequestId = 0 }, constants.ErrInvalidParameter},
		{"issued_at 为 0", func(r *inboxpb.GmSanctionPlayerRequest) { r.IssuedAtMs = 0 }, constants.ErrInvalidParameter},
		{"没有 action", func(r *inboxpb.GmSanctionPlayerRequest) { r.Action = nil }, constants.ErrInvalidParameter},
		{"action 内容为空", func(r *inboxpb.GmSanctionPlayerRequest) {
			r.Action = &inboxpb.GmSanctionPlayerRequest_BlockGain{}
		}, constants.ErrInvalidParameter},
		{"物品封锁 action 内容为空", func(r *inboxpb.GmSanctionPlayerRequest) {
			r.Action = &inboxpb.GmSanctionPlayerRequest_BlockItemGain{}
		}, constants.ErrInvalidParameter},
		{"物品解封 action 内容为空", func(r *inboxpb.GmSanctionPlayerRequest) {
			r.Action = &inboxpb.GmSanctionPlayerRequest_UnblockItemGain{}
		}, constants.ErrInvalidParameter},
		{"operator 为空", func(r *inboxpb.GmSanctionPlayerRequest) { r.Operator = "" }, constants.ErrInvalidParameter},
		{"operator 超 64B", func(r *inboxpb.GmSanctionPlayerRequest) { r.Operator = strings.Repeat("o", 65) }, constants.ErrTextTooLong},
		{"reason 为空", func(r *inboxpb.GmSanctionPlayerRequest) { r.Reason = "" }, constants.ErrInvalidParameter},
		{"reason 超 256B", func(r *inboxpb.GmSanctionPlayerRequest) { r.Reason = strings.Repeat("r", 257) }, constants.ErrTextTooLong},
	}
	for _, tc := range cases {
		store := &fakeSanctionStore{}
		var audits []*inboxpb.PlayerInboxAudit
		req := validRequest()
		tc.mutate(req)
		resp := GmSanctionPlayer(context.Background(), newDeps(t, store, &audits), req, "p")
		if resp.GetErrorMessage().GetId() != tc.want || len(store.params) != 0 {
			t.Errorf("%s: code=%d enqueue_calls=%d, want code %d 且不入箱", tc.name, resp.GetErrorMessage().GetId(), len(store.params), tc.want)
		}
	}
}

// TestGmSanctionOutcomes:业务拒绝透传 tip;存储故障 / 审计失败折成 kPlayerInboxStoreUnavailable(fault)。
func TestGmSanctionOutcomes(t *testing.T) {
	logtest.Discard(t)
	var audits []*inboxpb.PlayerInboxAudit

	store := &fakeSanctionStore{result: inbox.EnqueueResult{Code: constants.ErrFull}}
	if resp := GmSanctionPlayer(context.Background(), newDeps(t, store, &audits), validRequest(), "p"); resp.GetErrorMessage().GetId() != constants.ErrFull {
		t.Errorf("FULL 应透传: %+v", resp)
	}
	store = &fakeSanctionStore{err: errors.New("db down")}
	if resp := GmSanctionPlayer(context.Background(), newDeps(t, store, &audits), validRequest(), "p"); resp.GetErrorMessage().GetId() != constants.ErrStoreUnavailable {
		t.Errorf("存储故障应折成 StoreUnavailable: %+v", resp)
	}
	store = &fakeSanctionStore{result: inbox.EnqueueResult{Seq: 3, Created: true}, auditErr: errors.New("audit insert failed")}
	if resp := GmSanctionPlayer(context.Background(), newDeps(t, store, &audits), validRequest(), "p"); resp.GetErrorMessage().GetId() != constants.ErrStoreUnavailable {
		t.Errorf("审计失败必须让整次入箱失败: %+v", resp)
	}
	store = &fakeSanctionStore{result: inbox.EnqueueResult{Seq: 9, Created: false, State: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_APPLIED}}
	resp := GmSanctionPlayer(context.Background(), newDeps(t, store, &audits), validRequest(), "p")
	if resp.GetErrorMessage() != nil || resp.GetCreated() || resp.GetSeq() != 9 || resp.GetState() != inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_APPLIED {
		t.Errorf("DUPLICATE 应返回原 seq 与当前状态: %+v", resp)
	}
}

// 规格 §3.4:处罚被冻结的角色、自愈读不到目录都是确定没写,不能回故障码:排空证明会被打成 Hold。真正的存储故障照旧。
func TestGmSanctionRoleNotVerifiedIsNotAFault(t *testing.T) {
	logtest.Discard(t)
	for name, cause := range map[string]error{
		"角色冻结或换主": inbox.ErrOriginalRole,
		"目录读不到":   errors.Join(rolefence.ErrUnavailable, errors.New("data_service down")),
	} {
		t.Run(name, func(t *testing.T) {
			var audits []*inboxpb.PlayerInboxAudit
			store := &fakeSanctionStore{err: cause}
			resp := GmSanctionPlayer(context.Background(), newDeps(t, store, &audits), validRequest(), "p")
			if resp.GetErrorMessage().GetId() != constants.ErrStaleOwner {
				t.Fatalf("resp = %+v", resp)
			}
			if serverbase.TipVerdict(resp.GetErrorMessage().GetId()) == serverbase.VerdictFault {
				t.Fatal("不能是故障码")
			}
		})
	}
}
