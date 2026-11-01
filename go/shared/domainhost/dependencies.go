package domainhost

import (
	"context"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"
	dspb "proto/data_service"
	"shared/idsegment"
	"time"
)

// IDClients同一实际DataService连接，按领域tag独占号段。
// 启动时预取一次，只为尽早暴露问题，失败只告警（和 guild、trade、login 一样）：首次部署时发号器记录要等 -id-issuer-init，
// data_service 也可能还没起来，这些都不该挡住进程启动。没好时取号回错误，好了自动恢复，不用重启。
// Close只关闭调用方创建的资源，不关闭共享或全局客户端。
type IDClients struct {
	RPC  zrpc.Client
	Data dspb.DataServiceClient
	IDs  map[string]*idsegment.Client
}

func OpenIDs(ctx context.Context, c zrpc.RpcClientConf, tags ...string) (result *IDClients, err error) {
	rpc, err := zrpc.NewClient(c)
	if err != nil {
		return nil, err
	}
	result = &IDClients{RPC: rpc, Data: dspb.NewDataServiceClient(rpc.Conn()), IDs: map[string]*idsegment.Client{}}
	owned := result
	ready := false
	defer func() {
		if !ready {
			owned.Close()
		}
	}()
	allocate := idsegment.Adapt(result.Data.AllocateIdSegment, func(tag string, step uint32) *dspb.AllocateIdSegmentRequest {
		return &dspb.AllocateIdSegmentRequest{BizTag: tag, Step: step}
	})
	for _, tag := range tags {
		ids, e := idsegment.New(allocate, idsegment.Options{BizTag: tag, Step: 1000, MinStep: 100, MaxStep: 100000, FetchTimeout: 1500 * time.Millisecond})
		if e != nil {
			return nil, e
		}
		result.IDs[tag] = ids
		check, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, e = ids.Next(check)
		cancel()
		if e != nil {
			logx.Errorf("[domainhost] warming the id segment %s failed, id allocation will keep retrying: %v", tag, e)
		}
	}
	ready = true
	return result, nil
}
func (r *IDClients) Close() {
	if r == nil {
		return
	}
	for _, ids := range r.IDs {
		ids.Close()
	}
	if r.RPC != nil {
		_ = r.RPC.Conn().Close()
	}
}
