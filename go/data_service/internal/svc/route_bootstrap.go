package svc

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// routeRows 答路由表是不是空的。*store.RouteStore 满足它。
type routeRows interface {
	HasAnyRoute(context.Context) (bool, error)
}

// playerHistory 答目录和存档库里有没有角色历史。*store.AccountDirectoryStore 满足它。
type playerHistory interface {
	HasHistoricalPlayers(context.Context, []uint32) (bool, error)
}

// routeBootstrapTimeout 是空环境判据的预算。K=64 时每片一条点查,存档库还要逐个打开。
const routeBootstrapTimeout = 30 * time.Second

// checkEmptyRouteBootstrap 是首次启动的空环境判据:player_route 是路由的唯一真源,表为空而目录或玩家库里已有角色历史,
// 说明路由表被清空或回档过,这时放行会把老玩家当成「没有归属」,所以拒启。非空路由表不做检查。
func checkEmptyRouteBootstrap(ctx context.Context, routes routeRows, history playerHistory, storageIDs []uint32) error {
	bounded, cancel := context.WithTimeout(ctx, routeBootstrapTimeout)
	defer cancel()
	hasRows, err := routes.HasAnyRoute(bounded)
	if err != nil {
		return fmt.Errorf("读取正规路由SQL失败: %w", err)
	}
	if hasRows {
		return nil
	}
	hasHistory, err := history.HasHistoricalPlayers(bounded, storageIDs)
	if err != nil {
		return fmt.Errorf("空路由SQL无法证明目录/已配置玩家库为空；先核正规schema: %w", err)
	}
	if hasHistory {
		return errors.New("空路由SQL仍有权威角色/玩家历史；路由表被清空或回档过，先恢复路由表，不能按新空环境启动")
	}
	return nil
}
