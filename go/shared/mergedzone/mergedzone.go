// Package mergedzone 是没有全局库连接的服务(guild、trade)读合服映射的入口:
// 把 data_service.GetMergedZones 接成 placement.MergedZoneCache。
//
// 解析规则、快照和定时重读都在 shared/placement(docs/design/player-data-tidb-scaling-plan.md §7.1),
// 这里只多一件事:给启动时的第一次读一段有界的等待。
package mergedzone

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	dspb "proto/data_service"
	"shared/placement"
)

const (
	// StartupBudget 是启动时等 data_service 回答的总时限。data_service 和调用方常常一起启动,它注册进 etcd、
	// 客户端发现它都要时间,本地脚本又没有编排器替进程重拉,所以在进程内等一段;等满还读不到就拒绝启动。
	StartupBudget = 60 * time.Second

	attemptTimeout = 3 * time.Second
	retryInterval  = time.Second
)

// Client 是 dspb.DataServiceClient 里这里用到的那一个方法。
type Client interface {
	GetMergedZones(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*dspb.GetMergedZonesResponse, error)
}

// Load 从 data_service 读一次映射并建缓存,不起定时重读(调用方 Start、Stop)。
//
// 读不到时在 StartupBudget 内重试,预算用完或 ctx 取消返回错误,调用方必须拒绝启动:启动时没有映射,
// 就分不清「没有合服」和「读不到」。映射表本身解析不通(placement.ErrMergedZoneUnresolvable)立刻返回,重试不会让它变好。
func Load(ctx context.Context, client Client, opts placement.MergedZoneCacheOptions) (*placement.MergedZoneCache, error) {
	if client == nil {
		return nil, errors.New("merged zones: no data_service client (DataServiceRpc is not configured)")
	}
	budget, cancel := context.WithTimeout(ctx, StartupBudget)
	defer cancel()
	return load(budget, reader(client), opts, func(ctx context.Context) error {
		timer := time.NewTimer(retryInterval)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	})
}

// reader 把 RPC 接成读数函数。空表解码出来是 nil map,placement 按空表处理。
func reader(client Client) placement.MergedZoneReader {
	return func(ctx context.Context) (map[uint32]uint32, error) {
		resp, err := client.GetMergedZones(ctx, &emptypb.Empty{})
		if err != nil {
			return nil, err
		}
		return resp.GetMergedInto(), nil
	}
}

// load 是重试循环。wait 由参数注入,单测不依赖真实时钟;它返回错误 = 预算用完或被取消。
func load(ctx context.Context, read placement.MergedZoneReader, opts placement.MergedZoneCacheOptions, wait func(context.Context) error) (*placement.MergedZoneCache, error) {
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		cache, err := placement.NewMergedZoneCache(attemptCtx, read, opts)
		cancel()
		if err == nil {
			return cache, nil
		}
		if errors.Is(err, placement.ErrMergedZoneUnresolvable) {
			return nil, err
		}
		if waitErr := wait(ctx); waitErr != nil {
			return nil, fmt.Errorf("merged zones: data_service did not answer GetMergedZones after %d attempt(s): %w", attempt, errors.Join(err, waitErr))
		}
	}
}
