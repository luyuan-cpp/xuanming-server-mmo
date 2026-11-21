package svc

import (
	"context"
	"errors"
	"fmt"

	"shared/authoritysql"

	"github.com/zeromicro/go-zero/core/breaker"
)

// logicalDBBreakerName 是逻辑库 logicalDB 在 go-zero 具名熔断表里的名字。
// 同一逻辑库下的多个连接池(账号目录、名字、路由、流水、快照、回收等)共用一把熔断,同进退。
func logicalDBBreakerName(logicalDB string) string {
	return "logical_db/" + logicalDB
}

// newLogicalDBConnectGate 返回按逻辑库隔离的建连熔断钩子(docs/design/data-service-breaker-per-logical-db.md §5.1)。
//
// 熔断挂在 authoritysql 建物理连接处:一个物理集群失联时,只有落在该集群逻辑库上的建连会打开熔断、
// 立即失败,同一 RPC 方法上指向其它健康逻辑库的调用不受影响。闸门冻结(搬库窗口)与业务错误都发生在
// 已建好的连接上,天然不计入;调用方主动取消(context.Canceled)也不计入。
func newLogicalDBConnectGate() authoritysql.ConnectGate {
	return func(ctx context.Context, logicalDB string) (func(error), error) {
		promise, err := breaker.GetBreaker(logicalDBBreakerName(logicalDB)).Allow()
		if err != nil {
			return nil, fmt.Errorf("logical database %s: %w", logicalDB, err)
		}
		return func(outcome error) {
			if outcome == nil || errors.Is(ctx.Err(), context.Canceled) {
				promise.Accept()
				return
			}
			promise.Reject("connect failed")
		}, nil
	}
}
