// Package inboxcontract 是玩家收件箱(go/player_inbox)与它的生产方(go/match)共用的入箱合同:
// 内容摘要的字节布局,以及按类型的序列化载荷上限。两边都改调这里,不再各自手写(2026-10-30,评审 B-3)。
//
//   - 载荷上限的唯一事实源是 proto/player_inbox/player_inbox.proto 枚举值上的
//     (OptionPlayerInboxType).max_payload_bytes;收件箱的类型注册表(internal/inbox/registry.go)与本包读的是同一个选项。
//   - 摘要布局是持久合同:收件箱条目的 content_sha256 列、Match 核对的签名回执都依赖它,改布局等于改存量数据的
//     判重语义。inboxcontract_test.go 用固定输入与固定十六进制钉住它。
package inboxcontract

import (
	"crypto/sha256"
	"encoding/binary"

	inboxpb "proto/player_inbox"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ContentSHA256 = sha256(msg_type ‖ issued_at_ms ‖ payload)(收件箱 IB-2)。
// 12 字节定长头:msg_type 按 big-endian uint32、issued_at_ms 按 big-endian uint64,之后接载荷原字节,拼接无歧义;
// nil 与空载荷结果相同。issued_at_ms 在摘要里,所以生产方刷新它重试会得到 CONFLICT(收件箱设计 §6.2 生产方契约)。
func ContentSHA256(msgType uint32, issuedAtMs uint64, payload []byte) []byte {
	var head [12]byte
	binary.BigEndian.PutUint32(head[0:4], msgType)
	binary.BigEndian.PutUint64(head[4:12], issuedAtMs)
	h := sha256.New()
	h.Write(head[:])
	h.Write(payload)
	return h.Sum(nil)
}

// MaxPayloadBytes 读类型规格上的序列化载荷上限(字节)。未登记的枚举值(含 UNSPECIFIED)、没有规格或上限为 0
// 都返回 false:调用方按「上限未知」拒绝(fail-closed),不能当成不限。调用方的判定统一写 len(payload) > max。
func MaxPayloadBytes(t inboxpb.PlayerInboxMessageType) (int, bool) {
	value := t.Descriptor().Values().ByNumber(protoreflect.EnumNumber(t))
	if value == nil {
		return 0, false
	}
	opts := value.Options()
	if opts == nil || !proto.HasExtension(opts, inboxpb.E_OptionPlayerInboxType) {
		return 0, false
	}
	spec, ok := proto.GetExtension(opts, inboxpb.E_OptionPlayerInboxType).(*inboxpb.PlayerInboxTypeSpec)
	if !ok || spec.GetMaxPayloadBytes() == 0 {
		return 0, false
	}
	return int(spec.GetMaxPayloadBytes()), true
}
