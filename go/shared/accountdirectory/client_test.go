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
	dspb "proto/data_service"
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
	for _, kind := range []string{"committed", "unknown-commit"} {
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
				return &dspb.AccountDirectoryResponse{Players: &base.AccountSimplePlayerList{Players: []*base.AccountSimplePlayer{proto.Clone(role).(*base.AccountSimplePlayer)}}}, nil
			}}, strings.Repeat("c", 32))
			out, err := client.CreateWithInitialCommit(context.Background(), "owner", role)
			if kind == "committed" {
				if err != nil || out == nil {
					t.Fatalf("有效回执被拒: %v", err)
				}
			} else if !errors.Is(err, ErrUnavailable) || out != nil {
				t.Fatalf("未知回执被放行: %v/%v", out, err)
			}
		})
	}
}

