package matchentry

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	pb "proto/match"
)

func TestTicketOccupancyAllowsOnlyExactIdleAndTerminal(t *testing.T) {
	idle := &pb.MatchAuthorityTicketRecord{PlayerId: 9, OwnerAccount: "original", OwnerRevision: 7}
	if TicketOccupied(nil) || TicketOccupied(idle) {
		t.Fatal("缺行或仅登记原身份的空占位不应占用")
	}
	for _, field := range []protoreflect.Name{"ticket", "request_id", "formation_id", "mode", "config_id", "team_size", "enqueued_ms", "not_before_ms", "zone_id", "rating_centi", "team_id", "network_profile", "expires_ms", "selected_region"} {
		t.Run(string(field), func(t *testing.T) {
			ticket := proto.Clone(idle).(*pb.MatchAuthorityTicketRecord)
			message := ticket.ProtoReflect()
			descriptor := message.Descriptor().Fields().ByName(field)
			switch descriptor.Kind() {
			case protoreflect.StringKind:
				message.Set(descriptor, protoreflect.ValueOfString("original"))
			case protoreflect.BytesKind:
				message.Set(descriptor, protoreflect.ValueOfBytes([]byte{1}))
			case protoreflect.Uint32Kind:
				message.Set(descriptor, protoreflect.ValueOfUint32(1))
			case protoreflect.Uint64Kind:
				message.Set(descriptor, protoreflect.ValueOfUint64(1))
			case protoreflect.Int64Kind:
				message.Set(descriptor, protoreflect.ValueOfInt64(1))
			default:
				t.Fatalf("占用契约字段类型变化：%s", descriptor.Kind())
			}
			if !TicketOccupied(ticket) {
				t.Fatal("非空pending0被漏过")
			}
			ticket.State = 4
			if TicketOccupied(ticket) {
				t.Fatal("精确终态仍被阻塞")
			}
		})
	}
	for _, state := range []uint32{1, 2, 3, 5, 99} {
		if !TicketOccupied(&pb.MatchAuthorityTicketRecord{State: state}) {
			t.Fatalf("未终态/未知状态%d被放行", state)
		}
	}
}
