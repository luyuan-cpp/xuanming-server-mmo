package logic

// 本包**唯一**的 smpb.SceneManagerClient 假实现(docs/design/scene-enter-chain-residuals.md §3.7、§7「组间关系」)。
// W1 / W2 的测试同包编译,各写一份会撞名、整个包编译失败;别的测试文件需要捕获 LeaveScene 时复用它,不要另建。

import (
	"context"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	common "proto/common/base"
	smpb "proto/scene_manager"
)

// fakeSceneManagerClient 加锁记录全部 LeaveScene 与 EnterScene 请求(克隆后存,不与调用方共享),可配应答与错误;
// 其余 RPC 一律回 Unimplemented —— player_locator 只调 LeaveScene 与托管归位的 EnterScene(hosted_resume.go),
// 调到别的就是用例写错了。
type fakeSceneManagerClient struct {
	mu            sync.Mutex
	leaveRequests []*smpb.LeaveSceneRequest
	leaveErr      error
	enterRequests []*smpb.EnterSceneRequest
	enterResp     *smpb.EnterSceneResponse
	enterErr      error
}

// setEnterReply 让之后的 EnterScene 回 resp / err(resp 为 nil 且 err 为 nil 时回成功的空应答)。请求照样记下。
func (f *fakeSceneManagerClient) setEnterReply(resp *smpb.EnterSceneResponse, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enterResp, f.enterErr = resp, err
}

// enterCalls 返回到目前为止收到的全部 EnterScene 请求(按到达顺序的副本)。
func (f *fakeSceneManagerClient) enterCalls() []*smpb.EnterSceneRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*smpb.EnterSceneRequest(nil), f.enterRequests...)
}

var _ smpb.SceneManagerClient = (*fakeSceneManagerClient)(nil)

// setLeaveErr 让之后的 LeaveScene 返回 err;nil 恢复成功。请求照样记下。
func (f *fakeSceneManagerClient) setLeaveErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaveErr = err
}

// leaveCalls 返回到目前为止收到的全部 LeaveScene 请求(按到达顺序的副本)。
func (f *fakeSceneManagerClient) leaveCalls() []*smpb.LeaveSceneRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*smpb.LeaveSceneRequest(nil), f.leaveRequests...)
}

func (f *fakeSceneManagerClient) LeaveScene(_ context.Context, in *smpb.LeaveSceneRequest, _ ...grpc.CallOption) (*common.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaveRequests = append(f.leaveRequests, proto.Clone(in).(*smpb.LeaveSceneRequest))
	if f.leaveErr != nil {
		return nil, f.leaveErr
	}
	return &common.Empty{}, nil
}

func (f *fakeSceneManagerClient) CreateScene(context.Context, *smpb.CreateSceneRequest, ...grpc.CallOption) (*smpb.CreateSceneResponse, error) {
	return nil, status.Error(codes.Unimplemented, "fakeSceneManagerClient: CreateScene not supported")
}

func (f *fakeSceneManagerClient) DestroyScene(context.Context, *smpb.DestroySceneRequest, ...grpc.CallOption) (*common.Empty, error) {
	return nil, status.Error(codes.Unimplemented, "fakeSceneManagerClient: DestroyScene not supported")
}

func (f *fakeSceneManagerClient) EnterScene(_ context.Context, in *smpb.EnterSceneRequest, _ ...grpc.CallOption) (*smpb.EnterSceneResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enterRequests = append(f.enterRequests, proto.Clone(in).(*smpb.EnterSceneRequest))
	if f.enterErr != nil {
		return nil, f.enterErr
	}
	if f.enterResp != nil {
		return proto.Clone(f.enterResp).(*smpb.EnterSceneResponse), nil
	}
	return &smpb.EnterSceneResponse{PlayerId: in.GetPlayerId()}, nil
}
