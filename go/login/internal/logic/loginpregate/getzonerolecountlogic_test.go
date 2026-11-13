package loginpregatelogic

import (
	"context"
	"errors"
	"testing"

	"login/internal/config"
	"login/internal/svc"
	dspb "proto/data_service"
	loginpb "proto/login"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testZoneRoleCountZone uint32 = 7

// fakeZoneRoleCounter 只实现 CountZoneRoles,记下每次问的区;其余方法沿用嵌入的 nil 接口(调用即 panic,用例不碰)。
type fakeZoneRoleCounter struct {
	dspb.DataServiceClient
	roles uint64
	err   error
	asked []uint32
}

func (f *fakeZoneRoleCounter) CountZoneRoles(_ context.Context, in *dspb.CountZoneRolesRequest, _ ...grpc.CallOption) (*dspb.CountZoneRolesResponse, error) {
	f.asked = append(f.asked, in.GetZoneId())
	if f.err != nil {
		return nil, f.err
	}
	return &dspb.CountZoneRolesResponse{Roles: f.roles}, nil
}

// withZone 把本 login 的区号临时设成 zone,用例结束还原(config.AppConfig 是进程级全局)。
func withZone(t *testing.T, zone uint32) {
	t.Helper()
	saved := config.AppConfig.Node.ZoneId
	config.AppConfig.Node.ZoneId = zone
	t.Cleanup(func() { config.AppConfig.Node.ZoneId = saved })
}

func newZoneRoleCountLogic(ctx context.Context, client dspb.DataServiceClient) *GetZoneRoleCountLogic {
	return NewGetZoneRoleCountLogic(ctx, &svc.ServiceContext{DataServiceClient: client})
}

// 本区:原样转问 data_service 本区的数并带回。
func TestGetZoneRoleCount_AnswersOwnZoneFromDataService(t *testing.T) {
	withZone(t, testZoneRoleCountZone)
	fake := &fakeZoneRoleCounter{roles: 30012}

	resp, err := newZoneRoleCountLogic(context.Background(), fake).GetZoneRoleCount(&loginpb.ZoneRoleCountRequest{ZoneId: testZoneRoleCountZone})
	if err != nil {
		t.Fatalf("GetZoneRoleCount: %v", err)
	}
	if resp.GetRoles() != 30012 {
		t.Fatalf("roles = %d, want 30012", resp.GetRoles())
	}
	if len(fake.asked) != 1 || fake.asked[0] != testZoneRoleCountZone {
		t.Fatalf("data_service asked for zones %v, want [%d]", fake.asked, testZoneRoleCountZone)
	}
}

// 问到别区的 login:InvalidArgument,而且不转问 data_service(不能拿别区的数开关本区)。
func TestGetZoneRoleCount_RejectsOtherZone(t *testing.T) {
	withZone(t, testZoneRoleCountZone)
	fake := &fakeZoneRoleCounter{roles: 1}

	_, err := newZoneRoleCountLogic(context.Background(), fake).GetZoneRoleCount(&loginpb.ZoneRoleCountRequest{ZoneId: testZoneRoleCountZone + 1})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
	if len(fake.asked) != 0 {
		t.Fatalf("data_service was asked %v for another zone", fake.asked)
	}
}

// data_service 没配或读不到:Unavailable,绝不回 0;网关已放弃时回它的取消原因。
func TestGetZoneRoleCount_FailsClosed(t *testing.T) {
	withZone(t, testZoneRoleCountZone)
	req := &loginpb.ZoneRoleCountRequest{ZoneId: testZoneRoleCountZone}

	resp, err := newZoneRoleCountLogic(context.Background(), nil).GetZoneRoleCount(req)
	if status.Code(err) != codes.Unavailable || resp != nil {
		t.Fatalf("no data_service client: (%v, %v), want (nil, Unavailable)", resp, err)
	}

	down := &fakeZoneRoleCounter{err: status.Error(codes.Unavailable, "error_code=30: tidb down")}
	resp, err = newZoneRoleCountLogic(context.Background(), down).GetZoneRoleCount(req)
	if status.Code(err) != codes.Unavailable || resp != nil {
		t.Fatalf("data_service down: (%v, %v), want (nil, Unavailable)", resp, err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	gaveUp := &fakeZoneRoleCounter{err: errors.New("context canceled")}
	_, err = newZoneRoleCountLogic(canceled, gaveUp).GetZoneRoleCount(req)
	if status.Code(err) != codes.Canceled {
		t.Fatalf("gateway gave up: err = %v, want Canceled", err)
	}
}
