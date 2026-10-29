package accountdirectory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	componentpb "proto/common/component"
	dspb "proto/data_service"
	"shared/savebarrier"
)

type initialCommitRPC struct {
	dspb.DataServiceClient
	create func(context.Context, *dspb.CreateAccountRoleRequest) (*dspb.AccountDirectoryResponse, error)
}

func (f initialCommitRPC) CreateAccountRole(ctx context.Context, in *dspb.CreateAccountRoleRequest, _ ...grpc.CallOption) (*dspb.AccountDirectoryResponse, error) {
	return f.create(ctx, in)
}

func TestCreateClientValidatesInitialCommitReceipt(t *testing.T) {
	role := &base.AccountSimplePlayer{PlayerId: 9101, ZoneId: 1}
	for _, kind := range []string{"new", "idempotent", "wrong-id", "zero-sequence", "business-payload", "unknown-field", "unknown-commit"} {
		t.Run(kind, func(t *testing.T) {
			client := NewClient(initialCommitRPC{create: func(ctx context.Context, in *dspb.CreateAccountRoleRequest) (*dspb.AccountDirectoryResponse, error) {
				if in.Account != "owner" || !proto.Equal(in.Role, role) {
					t.Fatal("RPC 丢失原建角身份")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("RPC 无原有界截止时间")
				}
				if kind == "unknown-commit" {
					return nil, status.Error(codes.Unavailable, "提交应答未知")
				}
				out := &dspb.AccountDirectoryResponse{Players: &base.AccountSimplePlayerList{Players: []*base.AccountSimplePlayer{proto.Clone(role).(*base.AccountSimplePlayer)}}, InitialPlayerData: savebarrier.InitialPlayerData(role.PlayerId)}
				switch kind {
				case "idempotent":
					out.InitialPlayerData = nil
				case "wrong-id":
					out.InitialPlayerData = savebarrier.InitialPlayerData(role.PlayerId + 1)
				case "zero-sequence":
					out.InitialPlayerData.LastSaveSeq = 0
				case "business-payload":
					out.InitialPlayerData.PlayerDatabaseData.LevelComponent = &componentpb.LevelComp{Level: 8}
				case "unknown-field":
					out.InitialPlayerData.ProtoReflect().SetUnknown([]byte{0xF8, 0x7F, 1})
				}
				return out, nil
			}}, strings.Repeat("c", 32))
			out, err := client.CreateWithInitialCommit(context.Background(), "owner", role)
			if kind == "new" || kind == "idempotent" {
				if err != nil || out == nil {
					t.Fatalf("有效回执被拒: %v", err)
				}
				if (out.InitialPlayerData != nil) != (kind == "new") {
					t.Fatal("幂等目录制造了初始凭据")
				}
			} else if !errors.Is(err, ErrUnavailable) || out != nil {
				t.Fatalf("坏/未知回执被放行: %v/%v", out, err)
			}
		})
	}
}

// 旧 Directory 实现仍可返回目录，能力缺失不制造新的 SQL 初始原件。
type oldDirectory struct{ Directory }

func (oldDirectory) Create(context.Context, string, *base.AccountSimplePlayer) (*base.AccountSimplePlayerList, error) {
	return &base.AccountSimplePlayerList{}, nil
}

func TestLegacyDirectoryCannotManufactureInitialCommitReceipt(t *testing.T) {
	out, err := CreateWithInitialCommit(context.Background(), oldDirectory{}, "owner", &base.AccountSimplePlayer{PlayerId: 9101})
	if err != nil || out == nil || out.InitialPlayerData != nil {
		t.Fatalf("旧目录能力被提升: %v/%v", out, err)
	}
}
