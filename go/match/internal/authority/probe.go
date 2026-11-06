// Package authority 持有全球匹配的唯一 SQL 状态和机房探测验签。
package authority

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"strconv"

	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	pb "proto/match"
	"shared/roleidentity"
)

const GrantDomain = "xuanming.match.probe.grant.v1\x00"
const ReceiptDomain = "xuanming.match.probe.receipt.v1\x00"

func Sign(secret []byte, domain string, payload []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(domain))
	h.Write(payload)
	return h.Sum(nil)
}

func Signed(secret []byte, domain string, value proto.Message) (*pb.SignedNetworkProbe, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &pb.SignedNetworkProbe{Payload: b, Signature: Sign(secret, domain, b)}, nil
}

func Verify(secret []byte, domain string, signed *pb.SignedNetworkProbe, value proto.Message) error {
	if len(secret) < 32 || signed == nil || len(signed.Payload) == 0 || len(signed.Payload) > 8192 || !hmac.Equal(Sign(secret, domain, signed.Payload), signed.Signature) {
		return errors.New("地域探测签名无效")
	}
	return proto.Unmarshal(signed.Payload, value)
}

func SessionBinding(ctx context.Context, s *base.SessionDetails) string {
	original, present := roleidentity.Original(ctx)
	if s == nil || s.PlayerId == 0 || s.GateInstanceId == "" || s.SessionIncarnation == 0 || s.OwnerRevision == 0 || !present || original.Role.GetPlayerId() != s.PlayerId || original.OwnerRevision != s.OwnerRevision {
		return ""
	}
	// 长度前缀避免账号或实例字段里的分隔符改变绑定边界。
	data := binary.BigEndian.AppendUint64(nil, uint64(len(s.GateInstanceId)))
	data = append(data, []byte(s.GateInstanceId)...)
	data = binary.BigEndian.AppendUint64(data, s.SessionIncarnation)
	data = binary.BigEndian.AppendUint64(data, s.PlayerId)
	data = binary.BigEndian.AppendUint64(data, uint64(len(original.Account)))
	data = append(data, []byte(original.Account)...)
	data = binary.BigEndian.AppendUint64(data, original.OwnerRevision)
	b := sha256.Sum256(data)
	return hex.EncodeToString(b[:])
}

// MaxProbeRegions 是一份探测最多覆盖的机房数，公开探测入口（network_probe.go）与按池撮合共用。
const MaxProbeRegions = 16

// PreferredRegion 由完整签名探测决定，不随某个副本的节点发现抖动或硬阈值变化：取最低 RTT 的机房，
// 同 RTT 取机房号小者。最低 RTT 都超过硬阈值时不存在合格机房，所以保存全部测量可直接适应阈值调整。
// 撮合（poolUnitInput）与提交点（claimTx）各自现算，不持久化。
func PreferredRegion(profile *pb.MatchNetworkProfile, now uint64) uint32 {
	var preferred, best uint32
	for region, rtt := range Latencies(profile, now, math.MaxUint32) {
		if preferred == 0 || rtt < best || (rtt == best && region < preferred) {
			preferred, best = region, rtt
		}
	}
	return preferred
}

// Latencies 仅接受签名且逐玩家、逐会话、逐计划匹配的回执。超时/缺测保持不可用。
func Latencies(profile *pb.MatchNetworkProfile, now uint64, hard uint32) map[uint32]uint32 {
	out := map[uint32]uint32{}
	for _, r := range profile.GetReceipts() {
		g := r.GetGrant()
		if g == nil || g.PhysicalRegion == 0 || r.ExpiresAtMs <= now || g.ExpiresAtMs < r.MeasuredAtMs || r.MeasuredAtMs > now || r.SampleCount < 3 || r.RttMs == 0 || r.RttMs > hard {
			continue
		}
		out[g.PhysicalRegion] = r.RttMs
	}
	return out
}

func DecodeProfile(payload []byte) (*pb.MatchNetworkProfile, error) {
	p := &pb.MatchNetworkProfile{}
	if err := proto.Unmarshal(payload, p); err != nil {
		return nil, err
	}
	if p.PlanId == "" || p.SessionBinding == "" {
		return nil, errors.New("探测计划缺失")
	}
	return p, nil
}

func Lane(t *pb.MatchAuthorityTicketRecord) string {
	return strconv.FormatUint(uint64(t.Mode), 10) + ":" + strconv.FormatUint(uint64(t.ConfigId), 10) + ":" + strconv.FormatUint(uint64(t.TeamSize), 10)
}
