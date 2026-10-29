package inbox

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"player_inbox/internal/constants"
	"shared/currencytype"

	battlepb "proto/battle"
	inboxpb "proto/player_inbox"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// 类型注册表(设计 §3.2)。
//
// 唯一事实源是 proto/player_inbox/player_inbox.proto 枚举值上的选项(OptionPlayerInboxDomain = 420001、
// OptionPlayerInboxType = 420002)。启动时用 protoreflect 读成注册表并自检,任何一条不通过就拒绝启动:
//   - 每个类型都有所属的域,且该域登记了域规格(max_pending > 0);
//   - 载荷消息名能在全局类型表里解析;
//   - 载荷上限在 (0, 16KiB];
//   - 分级只能是 DURABLE;
//   - 每个类型在本文件的 payloadRules 里都有校验函数,且校验函数认的消息名与规格一致
//     (新增类型漏写校验函数 = 拒启,而不是「不校验就入箱」)。
//
// 入箱开关不在注册表里:只由服务配置 PlayerInbox.EnqueueTypes 决定(设计 §3.3)。

// TypeEntry 是一个已登记类型解析之后的形态:原始规格 + 解析出的载荷类型 + 校验函数。
type TypeEntry struct {
	Type    inboxpb.PlayerInboxMessageType
	Spec    *inboxpb.PlayerInboxTypeSpec
	Payload protoreflect.MessageType
	rule    payloadRule
}

// Registry 是只读的类型注册表,构造后可并发使用。
type Registry struct {
	domains map[inboxpb.PlayerInboxDomain]*inboxpb.PlayerInboxDomainSpec
	types   map[inboxpb.PlayerInboxMessageType]TypeEntry
}

// payloadRule 是一个类型的 Go 侧载荷校验:message 必须等于规格的 payload_message。
type payloadRule struct {
	message  protoreflect.FullName
	validate func(proto.Message) error
}

// errPayloadRule 是校验函数的统一错误前缀:调用方一律折成 kPlayerInboxPayloadInvalid。
var errPayloadRule = errors.New("inbox: payload rejected")

// maxDebtExpiresAtSeconds 是欠款过期时刻(Unix 秒)的上限 9999-12-31T23:59:59Z。
// 不依赖当前时间(判重先于一切依赖时间的校验,设计 §6.2),只挡住「把毫秒当秒」这一类量级错误:
// 毫秒时间戳(约 1.8e12)必然超过它。
const maxDebtExpiresAtSeconds = 253402300799

// payloadRules 是按类型平铺的校验函数表(AGENTS §11.2:不用宏生成;NewRegistry 双向核对,漏登记即拒启)。
var payloadRules = map[inboxpb.PlayerInboxMessageType]payloadRule{
	inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT: {
		message: "BattleSettlementData", validate: validateBattlePayload,
	},
	inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN: {
		message: "player_inbox.GmCurrencyGainPayload", validate: validateGainPayload,
	},
	inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN: {
		message: "player_inbox.GmCurrencyGainPayload", validate: validateGainPayload,
	},
	inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_ATTACH_CURRENCY_DEBT: {
		message: "player_inbox.GmCurrencyDebtPayload", validate: validateDebtPayload,
	},
}

// 只核原件形状，不在Inbox重算战斗奖励；业务效果由Scene同一持久账本裁决。
func validateBattlePayload(m proto.Message) error {
	p, ok := m.(*battlepb.BattleSettlementData)
	if !ok || p.GetPlayerId() == 0 || p.GetBattleId() == 0 || p.GetOutcome() < battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN || p.GetOutcome() > battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW || battlePayloadHasUnknown(p.ProtoReflect()) {
		return fmt.Errorf("%w: Battle原件身份或终局无效", errPayloadRule)
	}
	return nil
}

func battlePayloadHasUnknown(m protoreflect.Message) bool {
	if len(m.GetUnknown()) != 0 {
		return true
	}
	unknown := false
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case f.IsMap() && f.MapValue().Message() != nil:
			v.Map().Range(func(_ protoreflect.MapKey, e protoreflect.Value) bool {
				unknown = battlePayloadHasUnknown(e.Message())
				return !unknown
			})
		case f.IsList() && f.Message() != nil:
			for i := 0; i < v.List().Len(); i++ {
				if battlePayloadHasUnknown(v.List().Get(i).Message()) {
					unknown = true
					break
				}
			}
		case f.Message() != nil && !f.IsMap() && !f.IsList():
			unknown = battlePayloadHasUnknown(v.Message())
		}
		return !unknown
	})
	return unknown
}

// validateGainPayload:封锁 / 解封只有 currency_type,按 proto 同源取值表(shared/currencytype)拒绝写错的值。
// scene 遇到不认识的值只可能是更新版本加的币种,判 kVersionSkew(停用本域,实体重建后重拉,设计 §7.6)。
func validateGainPayload(m proto.Message) error {
	p, ok := m.(*inboxpb.GmCurrencyGainPayload)
	if !ok {
		return fmt.Errorf("%w: want GmCurrencyGainPayload, got %T", errPayloadRule, m)
	}
	if !currencytype.IsKnown(p.GetCurrencyType()) {
		return fmt.Errorf("%w: currency_type=%d not in currency_type.proto", errPayloadRule, p.GetCurrencyType())
	}
	return nil
}

// validateDebtPayload:owe_amount ∈ (0, INT64_MAX](AttachDebt 的参数是 int64_t);reason ≤ 256B、
// gm_operator 非空且 ≤ 64B(由服务端用 operator 覆盖,随 CurrencyDebtEntry 永久进玩家存档,设计 §3.4)。
func validateDebtPayload(m proto.Message) error {
	p, ok := m.(*inboxpb.GmCurrencyDebtPayload)
	if !ok {
		return fmt.Errorf("%w: want GmCurrencyDebtPayload, got %T", errPayloadRule, m)
	}
	if !currencytype.IsKnown(p.GetCurrencyType()) {
		return fmt.Errorf("%w: currency_type=%d not in currency_type.proto", errPayloadRule, p.GetCurrencyType())
	}
	if p.GetOweAmount() == 0 || p.GetOweAmount() > math.MaxInt64 {
		return fmt.Errorf("%w: owe_amount=%d not in (0, INT64_MAX]", errPayloadRule, p.GetOweAmount())
	}
	if code := ValidateText(p.GetReason(), constants.MaxReasonBytes, false); code != 0 {
		return fmt.Errorf("%w: reason length=%d invalid", errPayloadRule, len(p.GetReason()))
	}
	if code := ValidateText(p.GetGmOperator(), constants.MaxOperatorBytes, true); code != 0 {
		return fmt.Errorf("%w: gm_operator length=%d invalid", errPayloadRule, len(p.GetGmOperator()))
	}
	if p.GetExpiresAt() > maxDebtExpiresAtSeconds {
		return fmt.Errorf("%w: expires_at=%d exceeds Unix seconds upper bound (milliseconds passed by mistake?)",
			errPayloadRule, p.GetExpiresAt())
	}
	return nil
}

// NewRegistry 从生成的描述符读出全部规格并自检。返回错误 = 启动致命。
func NewRegistry() (*Registry, error) {
	return newRegistry(
		inboxpb.PlayerInboxDomain(0).Descriptor(),
		inboxpb.PlayerInboxMessageType(0).Descriptor(),
		payloadRules,
		protoregistry.GlobalTypes,
	)
}

// newRegistry 是可注入的构造体,单测用它喂入残缺的规格表验证自检。
func newRegistry(domainEnum, typeEnum protoreflect.EnumDescriptor, rules map[inboxpb.PlayerInboxMessageType]payloadRule,
	types *protoregistry.Types) (*Registry, error) {
	r := &Registry{
		domains: map[inboxpb.PlayerInboxDomain]*inboxpb.PlayerInboxDomainSpec{},
		types:   map[inboxpb.PlayerInboxMessageType]TypeEntry{},
	}

	values := domainEnum.Values()
	for i := 0; i < values.Len(); i++ {
		v := values.Get(i)
		if v.Number() == 0 {
			continue
		}
		spec, ok := enumValueExtension(v, inboxpb.E_OptionPlayerInboxDomain).(*inboxpb.PlayerInboxDomainSpec)
		if !ok || spec == nil {
			return nil, fmt.Errorf("inbox registry: domain %s has no (OptionPlayerInboxDomain) spec", v.Name())
		}
		if spec.GetMaxPending() == 0 {
			return nil, fmt.Errorf("inbox registry: domain %s max_pending must be > 0", v.Name())
		}
		r.domains[inboxpb.PlayerInboxDomain(v.Number())] = spec
	}

	values = typeEnum.Values()
	for i := 0; i < values.Len(); i++ {
		v := values.Get(i)
		if v.Number() == 0 {
			continue
		}
		t := inboxpb.PlayerInboxMessageType(v.Number())
		spec, ok := enumValueExtension(v, inboxpb.E_OptionPlayerInboxType).(*inboxpb.PlayerInboxTypeSpec)
		if !ok || spec == nil {
			return nil, fmt.Errorf("inbox registry: type %s has no (OptionPlayerInboxType) spec", v.Name())
		}
		if spec.GetDomain() == inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_UNSPECIFIED {
			return nil, fmt.Errorf("inbox registry: type %s has no owning domain", v.Name())
		}
		if _, ok := r.domains[spec.GetDomain()]; !ok {
			return nil, fmt.Errorf("inbox registry: type %s domain %v has no domain spec", v.Name(), spec.GetDomain())
		}
		if spec.GetKlass() != inboxpb.PlayerInboxClass_PLAYER_INBOX_CLASS_DURABLE {
			return nil, fmt.Errorf("inbox registry: type %s class %v is not DURABLE (v1 registers durable types only)", v.Name(), spec.GetKlass())
		}
		if spec.GetMaxPayloadBytes() == 0 || spec.GetMaxPayloadBytes() > constants.MaxPayloadBytesHardCap {
			return nil, fmt.Errorf("inbox registry: type %s max_payload_bytes=%d not in (0, %d]",
				v.Name(), spec.GetMaxPayloadBytes(), constants.MaxPayloadBytesHardCap)
		}
		mt, err := types.FindMessageByName(protoreflect.FullName(spec.GetPayloadMessage()))
		if err != nil {
			return nil, fmt.Errorf("inbox registry: type %s payload_message %q cannot be resolved: %w", v.Name(), spec.GetPayloadMessage(), err)
		}
		rule, ok := rules[t]
		if !ok || rule.validate == nil {
			return nil, fmt.Errorf("inbox registry: type %s has no Go payload validator (add it to payloadRules)", v.Name())
		}
		if rule.message != mt.Descriptor().FullName() {
			return nil, fmt.Errorf("inbox registry: type %s validator expects %s but spec payload is %s",
				v.Name(), rule.message, mt.Descriptor().FullName())
		}
		r.types[t] = TypeEntry{Type: t, Spec: spec, Payload: mt, rule: rule}
	}
	for t := range rules {
		if _, ok := r.types[t]; !ok {
			return nil, fmt.Errorf("inbox registry: payloadRules has %v but the enum has no such registered type", t)
		}
	}
	if len(r.types) == 0 {
		return nil, errors.New("inbox registry: no message types registered")
	}
	return r, nil
}

// enumValueExtension 读枚举值上的扩展;没有选项时返回 nil。
func enumValueExtension(v protoreflect.EnumValueDescriptor, ext protoreflect.ExtensionType) any {
	opts := v.Options()
	if opts == nil {
		return nil
	}
	m, ok := opts.(proto.Message)
	if !ok || !proto.HasExtension(m, ext) {
		return nil
	}
	return proto.GetExtension(m, ext)
}

// Type 返回已登记类型;未登记返回 false。
func (r *Registry) Type(t inboxpb.PlayerInboxMessageType) (TypeEntry, bool) {
	e, ok := r.types[t]
	return e, ok
}

// Domain 返回已登记域的规格;未登记返回 false。
func (r *Registry) Domain(d inboxpb.PlayerInboxDomain) (*inboxpb.PlayerInboxDomainSpec, bool) {
	s, ok := r.domains[d]
	return s, ok
}

// Domains 返回全部已登记域,按域号升序(运维对账的输出顺序因此稳定,不随 map 遍历变)。
func (r *Registry) Domains() []inboxpb.PlayerInboxDomain {
	out := make([]inboxpb.PlayerInboxDomain, 0, len(r.domains))
	for d := range r.domains {
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

// CheckAllowlist 确认白名单里的每个类型都已登记。由装配层在启动时调用,不通过即拒启。
func (r *Registry) CheckAllowlist(allowed map[inboxpb.PlayerInboxMessageType]struct{}) error {
	for t := range allowed {
		if _, ok := r.types[t]; !ok {
			return fmt.Errorf("PlayerInbox.EnqueueTypes lists %v, which has no registered spec", t)
		}
	}
	return nil
}

// ValidatePayload 做入箱第 1 步的注册表校验(设计 §6.2),返回类型条目与 tip 码(0 = 通过)。
// 顺序:类型已登记且为持久类 → 在白名单里 → 大小 → 解码 → 类型校验函数。全部与当前时间无关。
func (r *Registry) ValidatePayload(t inboxpb.PlayerInboxMessageType, allowed map[inboxpb.PlayerInboxMessageType]struct{},
	payload []byte) (TypeEntry, uint32) {
	entry, ok := r.types[t]
	if !ok || entry.Spec.GetKlass() != inboxpb.PlayerInboxClass_PLAYER_INBOX_CLASS_DURABLE {
		return TypeEntry{}, constants.ErrUnknownType
	}
	if _, ok := allowed[t]; !ok {
		return TypeEntry{}, constants.ErrDisabled
	}
	if uint64(len(payload)) > uint64(entry.Spec.GetMaxPayloadBytes()) {
		return TypeEntry{}, constants.ErrPayloadTooLarge
	}
	msg, err := entry.Decode(payload)
	if err != nil {
		return TypeEntry{}, constants.ErrPayloadInvalid
	}
	if err := entry.rule.validate(msg); err != nil {
		return TypeEntry{}, constants.ErrPayloadInvalid
	}
	return entry, 0
}

// Decode 按类型规格解码载荷。带未知字段的载荷一律拒绝:消费方看不到的字段可能承载语义(fail-closed)。
func (e TypeEntry) Decode(payload []byte) (proto.Message, error) {
	msg := e.Payload.New().Interface()
	if err := proto.Unmarshal(payload, msg); err != nil {
		return nil, fmt.Errorf("inbox: decode %s: %w", e.Payload.Descriptor().FullName(), err)
	}
	if len(msg.ProtoReflect().GetUnknown()) != 0 {
		return nil, fmt.Errorf("inbox: payload %s carries unknown fields", e.Payload.Descriptor().FullName())
	}
	return msg, nil
}

// ValidateText 校验一个自由文本字段:必须是合法 UTF-8 且不超过 maxBytes 字节;required 时不得为空。
// 返回 tip 码(0 = 通过):空 → kInvalidParameter;超长 / 非法 UTF-8 → kPlayerInboxTextTooLong。
// 库里的 string 列是 MEDIUMTEXT,长度没有任何约束,所以必须在这里显式检查(设计 §3.4)。
func ValidateText(s string, maxBytes int, required bool) uint32 {
	if s == "" {
		if required {
			return constants.ErrInvalidParameter
		}
		return 0
	}
	if len(s) > maxBytes || !utf8.ValidString(s) {
		return constants.ErrTextTooLong
	}
	return 0
}

// DomainLabel 是域的低基数指标标签(scene_gm_sanction);未登记的值一律 "unknown"。
func DomainLabel(d inboxpb.PlayerInboxDomain) string {
	name, ok := inboxpb.PlayerInboxDomain_name[int32(d)]
	if !ok || d == inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_UNSPECIFIED {
		return "unknown"
	}
	return strings.ToLower(strings.TrimPrefix(name, "PLAYER_INBOX_DOMAIN_"))
}

// TypeLabel 是类型的低基数指标标签(gm_block_currency_gain);未登记的值一律 "unknown"。
func TypeLabel(t inboxpb.PlayerInboxMessageType) string {
	name, ok := inboxpb.PlayerInboxMessageType_name[int32(t)]
	if !ok || t == inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_UNSPECIFIED {
		return "unknown"
	}
	return strings.ToLower(strings.TrimPrefix(name, "PLAYER_INBOX_MSG_"))
}

// FenceLabel 是围栏结局的指标标签(no_stream / first / match / advance / stale / epoch_required / epoch_ahead)。
func FenceLabel(v inboxpb.PlayerInboxFenceVerdict) string {
	name, ok := inboxpb.PlayerInboxFenceVerdict_name[int32(v)]
	if !ok {
		return "unknown"
	}
	return strings.ToLower(strings.TrimPrefix(name, "PLAYER_INBOX_FENCE_"))
}

// AckResultLabel 是 Ack 逐条结果的指标标签(finalized / already_same / conflict / ...)。
func AckResultLabel(v inboxpb.PlayerInboxAckResult) string {
	name, ok := inboxpb.PlayerInboxAckResult_name[int32(v)]
	if !ok {
		return "unknown"
	}
	return strings.ToLower(strings.TrimPrefix(name, "PLAYER_INBOX_ACK_"))
}

// VoidReasonLabel 是作废原因的指标标签(cancelled / quarantined)。
func VoidReasonLabel(v inboxpb.PlayerInboxVoidReason) string {
	name, ok := inboxpb.PlayerInboxVoidReason_name[int32(v)]
	if !ok {
		return "unknown"
	}
	return strings.ToLower(strings.TrimPrefix(name, "PLAYER_INBOX_VOID_"))
}
