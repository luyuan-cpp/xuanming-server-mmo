package svc

import (
	"fmt"
	"strings"
	"sync"

	"github.com/zeromicro/go-zero/zrpc"

	smpb "proto/scene_manager"
)

// 托管归位(docs/design/player-connection-lost-hold.md §4.5)要把 EnterScene 发给角色所在 zone 的 scene_manager。租约监视
// 按 slot 分片、不按 zone 分(slot_shard.go),本副本认领到的离线托管角色可能在别的 zone;scene_manager 只放本 zone 的落点
// (control-plane-zone-scoping-20261105.md §4.3),发给本 zone 的那一个会被当成跨 zone 拒掉。
//
// 发现键按 zone 加后缀 `<base>.z<zone>`(同文 §1 第 6 条:K8s 渲染与本机多 zone 脚本同一条规则),本副本配的是本 zone 的那一个;
// 别的 zone 的键由它换后缀得到。配的键不带后缀(单 zone 部署)时全服只有一个 zone,别的 zone 寻址不到,按错误返回。
// 离场通知(LeaveScene)不经这里:那条只动玩家域,任何 zone 的 scene_manager 都能处理。

// SceneManagerKeyForZone 由本 zone 的发现键推出 zone 的发现键。zone == ownZone 时原样返回。
func SceneManagerKeyForZone(ownKey string, ownZone, zone uint32) (string, error) {
	if zone == 0 {
		return "", fmt.Errorf("zone 0 is not addressable")
	}
	if zone == ownZone {
		return ownKey, nil
	}
	suffix := fmt.Sprintf(".z%d", ownZone)
	base, ok := strings.CutSuffix(ownKey, suffix)
	if ownZone == 0 || !ok || base == "" {
		return "", fmt.Errorf("scene_manager discovery key %q does not end with %q; zone %d is not addressable from this deployment",
			ownKey, suffix, zone)
	}
	return fmt.Sprintf("%s.z%d", base, zone), nil
}

// zoneSceneManagers 是别的 zone 的 scene_manager 客户端,按需建、建好缓存(zone 数有界)。线程安全。
type zoneSceneManagers struct {
	conf    zrpc.RpcClientConf
	ownZone uint32
	dial    func(zrpc.RpcClientConf) (smpb.SceneManagerClient, error)

	mu      sync.Mutex
	clients map[uint32]smpb.SceneManagerClient
}

func newZoneSceneManagers(conf zrpc.RpcClientConf, ownZone uint32) *zoneSceneManagers {
	return &zoneSceneManagers{conf: conf, ownZone: ownZone, dial: dialSceneManager, clients: map[uint32]smpb.SceneManagerClient{}}
}

// dialSceneManager 建一个 zrpc 客户端。强制 NonBlock:在租约监视的协程里建,不能因为那个 zone 暂时没有 scene_manager 就阻塞
// (go-zero 缺省阻塞拨号,拨不上直接返回错误)。连不上的请求由调用方当作这一次没发成,下一次续租再来。
func dialSceneManager(conf zrpc.RpcClientConf) (smpb.SceneManagerClient, error) {
	conf.NonBlock = true
	cli, err := zrpc.NewClient(conf)
	if err != nil {
		return nil, err
	}
	return smpb.NewSceneManagerClient(cli.Conn()), nil
}

func (z *zoneSceneManagers) forZone(zone uint32) (smpb.SceneManagerClient, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if client, ok := z.clients[zone]; ok {
		return client, nil
	}
	key, err := SceneManagerKeyForZone(z.conf.Etcd.Key, z.ownZone, zone)
	if err != nil {
		return nil, err
	}
	conf := z.conf
	conf.Etcd.Key = key
	client, err := z.dial(conf)
	if err != nil {
		return nil, fmt.Errorf("build scene_manager client for zone %d (key %q): %w", zone, key, err)
	}
	z.clients[zone] = client
	return client, nil
}

// SceneManagerForZone 返回 zone 的 scene_manager 客户端:本 zone 就是 SceneManagerClient,别的 zone 按发现键后缀按需建。
// 只给托管归位用(见文件头)。
func (s *ServiceContext) SceneManagerForZone(zone uint32) (smpb.SceneManagerClient, error) {
	if zone != 0 && zone == s.Config.Node.ZoneId {
		if s.SceneManagerClient == nil {
			return nil, fmt.Errorf("SceneManager client is not configured")
		}
		return s.SceneManagerClient, nil
	}
	if s.zoneSceneManagers == nil {
		return nil, fmt.Errorf("scene_manager of zone %d is not addressable: no discovery configured", zone)
	}
	return s.zoneSceneManagers.forZone(zone)
}

// SetZoneSceneManagerForTest 让单测给某个 zone 装一个假的 scene_manager 客户端(不经发现)。生产从不调用。
func (s *ServiceContext) SetZoneSceneManagerForTest(zone uint32, client smpb.SceneManagerClient) {
	if s.zoneSceneManagers == nil {
		s.zoneSceneManagers = &zoneSceneManagers{clients: map[uint32]smpb.SceneManagerClient{}}
	}
	s.zoneSceneManagers.mu.Lock()
	defer s.zoneSceneManagers.mu.Unlock()
	s.zoneSceneManagers.clients[zone] = client
}
