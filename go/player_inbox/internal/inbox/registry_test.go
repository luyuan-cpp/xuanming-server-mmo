package inbox

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"player_inbox/internal/constants"

	battlepb "proto/battle"
	base "proto/common/base"
	dspb "proto/data_service"
	inboxpb "proto/player_inbox"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

const (
	gmBlock   = inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN
	gmUnblock = inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN
	gmDebt    = inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_ATTACH_CURRENCY_DEBT
	gmDomain  = inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_SCENE_GM_SANCTION
)

func TestBattleOriginalPermanentErrorsPrecedeStorage(t *testing.T) {
	valid := &inboxpb.BattleSettlementInboxRequest{PlayerId: 11, BattleId: 21, ExpectedOwnerAccount: "owner", ExpectedOwnerRevision: 1, IssuedAtMs: 5}
	valid.SettlementPayload = marshal(t, &battlepb.BattleSettlementData{PlayerId: 11, BattleId: 21, Outcome: battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW})
	for _, edit := range []func(*inboxpb.BattleSettlementInboxRequest){
		func(r *inboxpb.BattleSettlementInboxRequest) { r.PlayerId = 0 },
		func(r *inboxpb.BattleSettlementInboxRequest) { r.BattleId++ },
		func(r *inboxpb.BattleSettlementInboxRequest) { r.SettlementPayload = []byte{0xff} },
		func(r *inboxpb.BattleSettlementInboxRequest) {
			r.SettlementPayload = append(r.SettlementPayload, 0xf8, 0x07, 0x01)
		},
	} {
		req := proto.Clone(valid).(*inboxpb.BattleSettlementInboxRequest)
		edit(req)
		// 没有DB的同一个正式入口仍能先拒纯原件错误，不触P/流或资产。
		store := &Store{}
		if _, err := store.EnqueueBattleSettlement(context.Background(), req); !errors.Is(err, ErrBattleSettlementOriginal) {
			t.Fatalf("Enqueue原件错误应明确分类: %v", err)
		}
		if _, err := store.LookupBattleSettlement(context.Background(), req); !errors.Is(err, ErrBattleSettlementOriginal) {
			t.Fatalf("Lookup原件错误应明确分类: %v", err)
		}
	}
}

func TestBattleSettlementRegistryAndImmutableOriginal(t *testing.T) {
	r := mustRegistry(t)
	typ := inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT
	e, ok := r.Type(typ)
	if !ok || e.Spec.GetMaxPayloadBytes() != 16384 || e.Spec.GetPayloadMessage() != "BattleSettlementData" {
		t.Fatal("Battle类型规格不完整")
	}
	d, ok := r.Domain(e.Spec.Domain)
	if !ok || d.MaxPending != 1 || d.RequiresOutOfBattle {
		t.Fatal("Battle专域必须单槽且不等待战斗退出")
	}
	p := &battlepb.BattleSettlementData{BattleId: 21, PlayerId: 11, Outcome: battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW, ItemsGained: []*battlepb.BattleItemEntry{{ItemTableId: 3, Count: 1}}}
	raw, err := proto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	req := &inboxpb.BattleSettlementInboxRequest{PlayerId: 11, BattleId: 21, ExpectedOwnerAccount: "original-owner", ExpectedOwnerRevision: 7, IssuedAtMs: testNowMs, SettlementPayload: raw}
	if err = validateBattleOriginal(req); err != nil {
		t.Fatal(err)
	}
	role := &dspb.AccountRoleOwnership{Account: "original-owner", OwnerRevision: 7, Role: &base.AccountSimplePlayer{PlayerId: 11}}
	if !battleOriginalMatches(req, role) {
		t.Fatal("完整原归属应匹配")
	}
	role.OwnerRevision++
	if battleOriginalMatches(req, role) {
		t.Fatal("不得用新目录修订补旧源权限")
	}
	p.ItemsGained[0].ProtoReflect().SetUnknown([]byte{0xF8, 0x07, 0x01})
	raw, err = proto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	req.SettlementPayload = raw
	if validateBattleOriginal(req) == nil {
		t.Fatal("嵌套未知字段必须在执行前拒绝")
	}
	if _, code := r.ValidatePayload(typ, allTypesAllowed(), raw); code != constants.ErrPayloadInvalid {
		t.Fatalf("未知字段不是有效执行原件: %d", code)
	}
	req.SettlementPayload = make([]byte, 16385)
	if validateBattleOriginal(req) == nil {
		t.Fatal("大于16KiB必须拒绝")
	}
}

func TestBattleProducerCannotUseGenericEnqueueOrVoid(t *testing.T) {
	s := &Store{registry: mustRegistry(t)}
	r, err := s.Enqueue(context.Background(), EnqueueParams{PlayerID: 11, RequestID: 21, IssuedAtMs: testNowMs, Producer: inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_MATCH, Type: inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT}, nil)
	if err != nil || r.Code != constants.ErrInvalidParameter {
		t.Fatalf("普通入口不可伪造Match来源: %+v %v", r, err)
	}
	for _, mode := range []inboxpb.PlayerInboxVoidMode{inboxpb.PlayerInboxVoidMode_PLAYER_INBOX_VOID_MODE_NORMAL, inboxpb.PlayerInboxVoidMode_PLAYER_INBOX_VOID_MODE_FORCE_MISMATCH} {
		resp, err := s.Void(context.Background(), VoidParams{PlayerID: 11, Domain: inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT, Seq: 1, Mode: mode}, nil)
		if err != nil || resp.GetErrorMessage().GetId() != constants.ErrItemStateConflict {
			t.Fatalf("Battle不得通用VOID: %+v %v", resp, err)
		}
	}
}

func allTypesAllowed() map[inboxpb.PlayerInboxMessageType]struct{} {
	return map[inboxpb.PlayerInboxMessageType]struct{}{gmBlock: {}, gmUnblock: {}, gmDebt: {}, inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT: {}}
}

func mustRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

// TestNewRegistryFromGeneratedDescriptors 钉住 player_inbox.proto 枚举选项上的 v1 规格(设计 §2.2、§3.4、D14)。
func TestNewRegistryFromGeneratedDescriptors(t *testing.T) {
	r := mustRegistry(t)
	spec, ok := r.Domain(gmDomain)
	if !ok {
		t.Fatal("SCENE_GM_SANCTION 没有域规格")
	}
	if spec.GetMaxPending() != 64 || !spec.GetRequiresOutOfBattle() {
		t.Errorf("GM 域规格 = %+v, want max_pending=64 requires_out_of_battle=true", spec)
	}
	want := map[inboxpb.PlayerInboxMessageType]struct {
		payload  protoreflect.FullName
		maxBytes uint32
	}{
		gmBlock:   {"player_inbox.GmCurrencyGainPayload", 64},
		gmUnblock: {"player_inbox.GmCurrencyGainPayload", 64},
		gmDebt:    {"player_inbox.GmCurrencyDebtPayload", 1024},
	}
	for typ, w := range want {
		e, ok := r.Type(typ)
		if !ok {
			t.Fatalf("类型 %v 未登记", typ)
		}
		if e.Spec.GetDomain() != gmDomain || e.Spec.GetKlass() != inboxpb.PlayerInboxClass_PLAYER_INBOX_CLASS_DURABLE {
			t.Errorf("%v 规格 = %+v", typ, e.Spec)
		}
		if e.Payload.Descriptor().FullName() != w.payload || e.Spec.GetMaxPayloadBytes() != w.maxBytes {
			t.Errorf("%v 载荷 = %s/%d, want %s/%d", typ, e.Payload.Descriptor().FullName(), e.Spec.GetMaxPayloadBytes(), w.payload, w.maxBytes)
		}
	}
	// proto 里每个非 0 类型都必须登记(新增类型漏写规格或校验函数会在 NewRegistry 拒启,这里再从枚举侧核一遍)。
	for v := range inboxpb.PlayerInboxMessageType_name {
		if v == 0 {
			continue
		}
		if _, ok := r.Type(inboxpb.PlayerInboxMessageType(v)); !ok {
			t.Errorf("枚举值 %d 没有进注册表", v)
		}
	}
}

// ── 自检失败拒绝启动:用合成描述符喂入残缺规格 ──────────────────────────

type synthValue struct {
	name   string
	number int32
	domain *inboxpb.PlayerInboxDomainSpec
	typ    *inboxpb.PlayerInboxTypeSpec
}

// synthEnums 用 protodesc 合成两个枚举(域、类型),值上挂给定的扩展选项。
func synthEnums(t *testing.T, domains, types []synthValue) (protoreflect.EnumDescriptor, protoreflect.EnumDescriptor) {
	t.Helper()
	toProto := func(enumName string, values []synthValue) *descriptorpb.EnumDescriptorProto {
		ed := &descriptorpb.EnumDescriptorProto{Name: proto.String(enumName)}
		ed.Value = append(ed.Value, &descriptorpb.EnumValueDescriptorProto{
			Name: proto.String(strings.ToUpper(enumName) + "_UNSPECIFIED"), Number: proto.Int32(0),
		})
		for _, v := range values {
			opts := &descriptorpb.EnumValueOptions{}
			if v.domain != nil {
				proto.SetExtension(opts, inboxpb.E_OptionPlayerInboxDomain, v.domain)
			}
			if v.typ != nil {
				proto.SetExtension(opts, inboxpb.E_OptionPlayerInboxType, v.typ)
			}
			ed.Value = append(ed.Value, &descriptorpb.EnumValueDescriptorProto{
				Name: proto.String(v.name), Number: proto.Int32(v.number), Options: opts,
			})
		}
		return ed
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name:     proto.String("inboxtest/" + strings.ReplaceAll(t.Name(), "/", "_") + ".proto"),
		Package:  proto.String("inboxtest"),
		Syntax:   proto.String("proto3"),
		EnumType: []*descriptorpb.EnumDescriptorProto{toProto("TestDomain", domains), toProto("TestType", types)},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("合成描述符失败: %v", err)
	}
	return fd.Enums().ByName("TestDomain"), fd.Enums().ByName("TestType")
}

func goodDomain() synthValue {
	return synthValue{name: "D_GM", number: 1, domain: &inboxpb.PlayerInboxDomainSpec{MaxPending: 64, RequiresOutOfBattle: true}}
}

func goodType(payload string, maxBytes uint32) *inboxpb.PlayerInboxTypeSpec {
	return &inboxpb.PlayerInboxTypeSpec{
		Domain:          gmDomain,
		Klass:           inboxpb.PlayerInboxClass_PLAYER_INBOX_CLASS_DURABLE,
		PayloadMessage:  payload,
		MaxPayloadBytes: maxBytes,
	}
}

func goodTypes() []synthValue {
	return []synthValue{
		{name: "T_BLOCK", number: 1, typ: goodType("player_inbox.GmCurrencyGainPayload", 64)},
		{name: "T_UNBLOCK", number: 2, typ: goodType("player_inbox.GmCurrencyGainPayload", 64)},
		{name: "T_DEBT", number: 3, typ: goodType("player_inbox.GmCurrencyDebtPayload", 1024)},
	}
}

func copyRules() map[inboxpb.PlayerInboxMessageType]payloadRule {
	out := make(map[inboxpb.PlayerInboxMessageType]payloadRule, len(payloadRules))
	// 合成描述符明确只建原GM三类型，不能被生产新增Battle规则遮住各原负例的实际错因。
	for _, k := range []inboxpb.PlayerInboxMessageType{gmBlock, gmUnblock, gmDebt} {
		out[k] = payloadRules[k]
	}
	return out
}

func TestNewRegistrySyntheticBaselinePasses(t *testing.T) {
	d, ty := synthEnums(t, []synthValue{goodDomain()}, goodTypes())
	if _, err := newRegistry(d, ty, copyRules(), protoregistry.GlobalTypes); err != nil {
		t.Fatalf("合成基线应通过自检: %v", err)
	}
}

// TestNewRegistrySelfCheckRejects:每一条自检不通过都拒绝启动(设计 §3.2)。
func TestNewRegistrySelfCheckRejects(t *testing.T) {
	cases := []struct {
		name    string
		domains func() []synthValue
		types   func() []synthValue
		rules   func() map[inboxpb.PlayerInboxMessageType]payloadRule
	}{
		{
			name:    "域没有规格",
			domains: func() []synthValue { return []synthValue{{name: "D_GM", number: 1}} },
		},
		{
			name: "域 max_pending 为 0",
			domains: func() []synthValue {
				return []synthValue{{name: "D_GM", number: 1, domain: &inboxpb.PlayerInboxDomainSpec{}}}
			},
		},
		{
			name: "类型没有规格",
			types: func() []synthValue {
				ts := goodTypes()
				ts[0].typ = nil
				return ts
			},
		},
		{
			name: "类型没有所属域",
			types: func() []synthValue {
				ts := goodTypes()
				ts[0].typ.Domain = inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_UNSPECIFIED
				return ts
			},
		},
		{
			name: "类型的域没有域规格",
			types: func() []synthValue {
				ts := goodTypes()
				ts[0].typ.Domain = inboxpb.PlayerInboxDomain(7)
				return ts
			},
		},
		{
			name: "分级不是 DURABLE",
			types: func() []synthValue {
				ts := goodTypes()
				ts[1].typ.Klass = inboxpb.PlayerInboxClass_PLAYER_INBOX_CLASS_UNSPECIFIED
				return ts
			},
		},
		{
			name: "载荷上限为 0",
			types: func() []synthValue {
				ts := goodTypes()
				ts[0].typ.MaxPayloadBytes = 0
				return ts
			},
		},
		{
			name: "载荷上限超过 16KiB",
			types: func() []synthValue {
				ts := goodTypes()
				ts[2].typ.MaxPayloadBytes = constants.MaxPayloadBytesHardCap + 1
				return ts
			},
		},
		{
			name: "载荷消息名解析不了",
			types: func() []synthValue {
				ts := goodTypes()
				ts[2].typ.PayloadMessage = "player_inbox.NoSuchPayload"
				return ts
			},
		},
		{
			name: "类型缺 Go 校验函数",
			rules: func() map[inboxpb.PlayerInboxMessageType]payloadRule {
				rs := copyRules()
				delete(rs, gmDebt)
				return rs
			},
		},
		{
			name: "校验函数认的消息与规格不一致",
			rules: func() map[inboxpb.PlayerInboxMessageType]payloadRule {
				rs := copyRules()
				rs[gmDebt] = payloadRules[gmBlock]
				return rs
			},
		},
		{
			name: "校验函数表多出未登记的类型",
			rules: func() map[inboxpb.PlayerInboxMessageType]payloadRule {
				rs := copyRules()
				rs[inboxpb.PlayerInboxMessageType(99)] = payloadRules[gmBlock]
				return rs
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			domains := []synthValue{goodDomain()}
			if tc.domains != nil {
				domains = tc.domains()
			}
			types := goodTypes()
			if tc.types != nil {
				types = tc.types()
			}
			rules := copyRules()
			if tc.rules != nil {
				rules = tc.rules()
			}
			d, ty := synthEnums(t, domains, types)
			if _, err := newRegistry(d, ty, rules, protoregistry.GlobalTypes); err == nil {
				t.Fatal("自检应拒绝,实际通过")
			}
		})
	}
}

// ── 入箱第 1 步:注册表校验 ─────────────────────────────────────────────

func marshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func validDebt() *inboxpb.GmCurrencyDebtPayload {
	return &inboxpb.GmCurrencyDebtPayload{CurrencyType: 1, OweAmount: 500, Reason: "补缴 #123", GmOperator: "ops_alice", ExpiresAt: 1_900_000_000}
}

func TestValidatePayload(t *testing.T) {
	r := mustRegistry(t)
	all := allTypesAllowed()
	onlyBlock := map[inboxpb.PlayerInboxMessageType]struct{}{gmBlock: {}}

	longestDebt := validDebt()
	longestDebt.Reason = strings.Repeat("a", 256)
	longestDebt.GmOperator = strings.Repeat("b", 64)

	cases := []struct {
		name    string
		typ     inboxpb.PlayerInboxMessageType
		allowed map[inboxpb.PlayerInboxMessageType]struct{}
		payload []byte
		want    uint32
	}{
		{"封锁:空载荷(金币 = 0)合法", gmBlock, all, nil, 0},
		{"封锁:钻石", gmBlock, all, marshal(t, &inboxpb.GmCurrencyGainPayload{CurrencyType: 1}), 0},
		{"挂欠款:合法", gmDebt, all, marshal(t, validDebt()), 0},
		{"未知类型", inboxpb.PlayerInboxMessageType(99), all, nil, constants.ErrUnknownType},
		{"UNSPECIFIED", inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_UNSPECIFIED, all, nil, constants.ErrUnknownType},
		{"不在白名单", gmDebt, onlyBlock, marshal(t, validDebt()), constants.ErrDisabled},
		{"白名单为空 = 全部停用", gmBlock, nil, nil, constants.ErrDisabled},
		{"超过类型上限", gmBlock, all, bytes.Repeat([]byte{0x08, 0x01}, 33), constants.ErrPayloadTooLarge},
		{"解码失败", gmBlock, all, []byte{0xff}, constants.ErrPayloadInvalid},
		{"带未知字段", gmBlock, all, []byte{0x48, 0x01}, constants.ErrPayloadInvalid},
		{"欠款额为 0", gmDebt, all, marshal(t, withDebt(func(p *inboxpb.GmCurrencyDebtPayload) { p.OweAmount = 0 })), constants.ErrPayloadInvalid},
		{"欠款额超过 INT64_MAX", gmDebt, all, marshal(t, withDebt(func(p *inboxpb.GmCurrencyDebtPayload) { p.OweAmount = math.MaxInt64 + 1 })), constants.ErrPayloadInvalid},
		{"欠款额恰为 INT64_MAX", gmDebt, all, marshal(t, withDebt(func(p *inboxpb.GmCurrencyDebtPayload) { p.OweAmount = math.MaxInt64 })), 0},
		{"gm_operator 为空", gmDebt, all, marshal(t, withDebt(func(p *inboxpb.GmCurrencyDebtPayload) { p.GmOperator = "" })), constants.ErrPayloadInvalid},
		{"gm_operator 超 64B", gmDebt, all, marshal(t, withDebt(func(p *inboxpb.GmCurrencyDebtPayload) { p.GmOperator = strings.Repeat("x", 65) })), constants.ErrPayloadInvalid},
		{"欠款 reason 超 256B", gmDebt, all, marshal(t, withDebt(func(p *inboxpb.GmCurrencyDebtPayload) { p.Reason = strings.Repeat("r", 257) })), constants.ErrPayloadInvalid},
		{"expires_at 误传毫秒", gmDebt, all, marshal(t, withDebt(func(p *inboxpb.GmCurrencyDebtPayload) { p.ExpiresAt = 1_900_000_000_000 })), constants.ErrPayloadInvalid},
		{"expires_at 为 0 = 永不过期", gmDebt, all, marshal(t, withDebt(func(p *inboxpb.GmCurrencyDebtPayload) { p.ExpiresAt = 0 })), 0},
		{"上限内的最长文本", gmDebt, all, marshal(t, longestDebt), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := r.ValidatePayload(tc.typ, tc.allowed, tc.payload)
			if got != tc.want {
				t.Fatalf("ValidatePayload = %d, want %d", got, tc.want)
			}
		})
	}
}

func withDebt(mutate func(*inboxpb.GmCurrencyDebtPayload)) *inboxpb.GmCurrencyDebtPayload {
	p := validDebt()
	mutate(p)
	return p
}

func TestCheckAllowlist(t *testing.T) {
	r := mustRegistry(t)
	if err := r.CheckAllowlist(allTypesAllowed()); err != nil {
		t.Fatalf("全部已登记类型应通过: %v", err)
	}
	if err := r.CheckAllowlist(map[inboxpb.PlayerInboxMessageType]struct{}{inboxpb.PlayerInboxMessageType(99): {}}); err == nil {
		t.Fatal("白名单含未登记类型应拒启")
	}
}

func TestValidateText(t *testing.T) {
	cases := []struct {
		name     string
		s        string
		max      int
		required bool
		want     uint32
	}{
		{"必填为空", "", 64, true, constants.ErrInvalidParameter},
		{"选填为空", "", 64, false, 0},
		{"恰好上限", strings.Repeat("a", 64), 64, true, 0},
		{"超上限", strings.Repeat("a", 65), 64, true, constants.ErrTextTooLong},
		{"多字节按字节计", strings.Repeat("中", 22), 64, true, constants.ErrTextTooLong}, // 66 字节
		{"非法 UTF-8", "a\xffb", 64, true, constants.ErrTextTooLong},
	}
	for _, tc := range cases {
		if got := ValidateText(tc.s, tc.max, tc.required); got != tc.want {
			t.Errorf("%s: ValidateText = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestLabelsAreLowCardinality:标签只来自枚举名,未知值一律 "unknown"(AGENTS §9:不让任意整数变成新序列)。
func TestLabelsAreLowCardinality(t *testing.T) {
	if got := DomainLabel(gmDomain); got != "scene_gm_sanction" {
		t.Errorf("DomainLabel = %q", got)
	}
	if got := DomainLabel(inboxpb.PlayerInboxDomain(12345)); got != "unknown" {
		t.Errorf("未知域标签 = %q, want unknown", got)
	}
	if got := TypeLabel(gmDebt); got != "gm_attach_currency_debt" {
		t.Errorf("TypeLabel = %q", got)
	}
	if got := TypeLabel(inboxpb.PlayerInboxMessageType(12345)); got != "unknown" {
		t.Errorf("未知类型标签 = %q, want unknown", got)
	}
	if got := FenceLabel(inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_EPOCH_AHEAD); got != "epoch_ahead" {
		t.Errorf("FenceLabel = %q", got)
	}
	if got := AckResultLabel(inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_SKIP_NOT_REQUESTED); got != "skip_not_requested" {
		t.Errorf("AckResultLabel = %q", got)
	}
	if got := VoidReasonLabel(inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_QUARANTINED); got != "quarantined" {
		t.Errorf("VoidReasonLabel = %q", got)
	}
}
