package svc

// zone_policy.go — 本区「区服策略」读者的装配(zonepolicy.Reader):起服时建、Start 时起读循环、CloseSource 时停。
// 读到的策略只有两个用处:登录排队的区容量(gateWatcherCapacityProvider.ZoneCapacity)与建角拦截(CreateRoleClosed)。
// 权威来源与读不到时的口径见 zonepolicy 包注释。

import (
	"context"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"

	"login/internal/logic/pkg/zonepolicy"

	"shared/safego"
)

const (
	// zonePolicyRefreshInterval:读区服策略的周期。运维改了 zone_config 之后,本区网关至多 5 秒写到 etcd,
	// login 至多再晚这么久读到。读的是一个小键,每个副本每秒 0.5 次点读。
	zonePolicyRefreshInterval = 2 * time.Second
	// zonePolicyGetTimeout:一次点读的预算,不超过一个周期,免得读挂住时循环堆积。
	zonePolicyGetTimeout = 2 * time.Second
)

// newZonePolicyReader 建本区的区服策略读者;没有 etcd 客户端时回 nil(读者的方法对 nil 安全,口径同「键不存在」)。
func newZonePolicyReader(cli *clientv3.Client, zoneID uint32) *zonepolicy.Reader {
	if cli == nil {
		return nil
	}
	return zonepolicy.NewReader(zoneID, func(ctx context.Context, key string) ([]byte, bool, error) {
		getCtx, cancel := context.WithTimeout(ctx, zonePolicyGetTimeout)
		defer cancel()
		resp, err := cli.Get(getCtx, key)
		if err != nil {
			return nil, false, err
		}
		if len(resp.Kvs) == 0 {
			return nil, false, nil
		}
		return resp.Kvs[0].Value, true, nil
	})
}

// startZonePolicyReader 起区服策略的读循环。每个 login 副本各读一份,读的是同一个键,不需要选主。
func (s *ServiceContext) startZonePolicyReader() {
	if s.ZonePolicy == nil {
		logx.Errorf("[ZonePolicy] reader NOT started (no etcd client): the login queue uses the soft cap and " +
			"character creation is never restricted in this process")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.zonePolicyStop = cancel
	reader := s.ZonePolicy
	s.background.Add(1)
	safego.Go("login.zone_policy", func() {
		defer s.background.Done()
		reader.Run(ctx, zonePolicyRefreshInterval)
	})
}

// CreateRoleClosed 报告本区是否暂停创建新角色(区服目录的 create_role_closed)。读不到区服策略时回 false:
// 不拦建角,口径见 zonepolicy 包注释。
func (s *ServiceContext) CreateRoleClosed() bool {
	p, ok := s.ZonePolicy.Current()
	return ok && p.CreateRoleClosed
}
