package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/netx"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/encoding/protojson"

	"db/internal/config"
	"db/internal/logic/pkg/proto_sql"
	"db/internal/playerstore"
	"db/internal/svc"

	"proto/common/base"
	"shared/generated/persistschema"
	"shared/noderegistry"
)

const (
	playerStoreNodeType = base.ENodeType_PlayerStoreNodeService
	// playerStoreLeaseTTL 是 C++ 约定注册的租约,崩溃之后 scene 最多这么久才看不到这个节点。
	playerStoreLeaseTTL int64 = 60

	playerStoreEtcdDialTimeout  = 5 * time.Second
	playerStoreRegisterTimeout  = 30 * time.Second
	playerStoreListenProbeLimit = 30 * time.Second
	playerStoreSchemaTimeout    = 30 * time.Second
)

// newPlayerStoreServer 装配存盘接口。配置不对就拒绝启动:这个节点起不来比起来却存不进要好。
func newPlayerStoreServer(c *config.Config, ctx *svc.ServiceContext, stores *proto_sql.StoreRegistry) (*playerstore.Server, error) {
	limits := playerstore.DefaultLimits()
	if v := c.ServerConfig.PlayerStore.MaxRowBytes; v > 0 {
		limits.MaxRowBytes = v
	}
	if v := c.ServerConfig.PlayerStore.MaxRequestBytes; v > 0 {
		limits.MaxRequestBytes = v
	}
	resolve, err := playerstore.NewStorageResolver(playerstore.ResolverOptions{
		Redis:             ctx.PlacementRedis,
		Zone:              c.ZoneId,
		Required:          c.Placement.Required,
		RequireRouteReady: c.Placement.RequireRouteReady,
	})
	if err != nil {
		return nil, err
	}
	open := func(openCtx context.Context, storageID uint32) (*sql.DB, error) {
		store, err := stores.Store(openCtx, storageID)
		if err != nil {
			return nil, err
		}
		return store.DB, nil
	}
	// 本 zone 的库必须已经有 player_store。别的落点库在第一次用到时才打开,缺表由启动它们的 zone 发现。
	verifyCtx, cancel := context.WithTimeout(context.Background(), playerStoreSchemaTimeout)
	defer cancel()
	home, err := open(verifyCtx, c.ZoneId)
	if err != nil {
		return nil, fmt.Errorf("open home storage %d: %w", c.ZoneId, err)
	}
	store, err := playerstore.New(home, persistschema.Gen, limits)
	if err != nil {
		return nil, err
	}
	if err := store.VerifySchema(verifyCtx); err != nil {
		return nil, err
	}
	return playerstore.NewServer(playerstore.ServerOptions{
		Secret:   os.Getenv(playerstore.SceneSecretEnv),
		Open:     open,
		Resolve:  resolve,
		KnownGen: persistschema.Gen,
		Limits:   limits,
	})
}

// registerPlayerStoreNode 写 C++ 约定的 etcd 注册:scene 靠它发现本 zone 的 go/db。
// 等端口真正 accept 之后才写 key,C++ 一看到 NodeInfo 就会拨。
// 失败返回错误,调用方拒绝继续:scene 发现不到就存不了盘。
func registerPlayerStoreNode(c *config.Config) (func(), error) {
	if len(c.Etcd.Hosts) == 0 {
		return nil, fmt.Errorf("Etcd.Hosts is empty; scene discovers go/db through etcd")
	}
	cfg := clientv3.Config{Endpoints: c.Etcd.Hosts, DialTimeout: playerStoreEtcdDialTimeout}
	if c.Etcd.HasAccount() {
		cfg.Username = c.Etcd.User
		cfg.Password = c.Etcd.Pass
	}
	cli, err := clientv3.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("connect etcd: %w", err)
	}
	host, portText, err := net.SplitHostPort(c.ListenOn)
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("parse ListenOn %q: %w", c.ListenOn, err)
	}
	port, err := strconv.ParseUint(portText, 10, 32)
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("parse ListenOn port %q: %w", portText, err)
	}
	dialHost := host
	if isUnspecifiedHost(host) {
		dialHost = "127.0.0.1"
	}
	regCtx, cancel := context.WithTimeout(context.Background(), playerStoreRegisterTimeout)
	defer cancel()
	registration, err := noderegistry.RegisterAfterListening(regCtx, cli, net.JoinHostPort(dialHost, portText), playerStoreListenProbeLimit,
		noderegistry.Spec{
			Prefix:          base.ENodeType_name[int32(playerStoreNodeType)] + ".rpc",
			NodeType:        uint32(playerStoreNodeType),
			ZoneId:          c.ZoneId,
			LeaseTTL:        playerStoreLeaseTTL,
			BuildValue:      playerStoreNodeInfo(c.ZoneId, advertisedHost(host), uint32(port), uint64(time.Now().Unix())),
			OnReclaimFailed: noderegistry.ReallocateNewID,
			OnNodeIDChanged: func(oldID, newID uint32) {
				logx.Errorf("[player_store] could not reclaim node_id=%d after etcd lease loss; continuing with node_id=%d", oldID, newID)
			},
			LogPrefix: "[player_store]",
		})
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("register node: %w", err)
	}
	registration.KeepAlive()
	return func() {
		registration.Close()
		_ = cli.Close()
	}, nil
}

// playerStoreNodeInfo 按 C++ NodeInfo 约定生成 protojson。
func playerStoreNodeInfo(zoneID uint32, host string, port uint32, launchTime uint64) func(nodeID uint32, nodeUUID string) ([]byte, error) {
	return func(nodeID uint32, nodeUUID string) ([]byte, error) {
		info := &base.NodeInfo{
			NodeId:       nodeID,
			NodeType:     uint32(playerStoreNodeType),
			ZoneId:       zoneID,
			NodeUuid:     nodeUUID,
			LaunchTime:   launchTime,
			ProtocolType: uint32(base.ENodeProtocolType_PROTOCOL_GRPC),
			Endpoint:     &base.EndpointComp{Ip: host, Port: port},
			GrpcEndpoint: &base.EndpointComp{Ip: host, Port: port},
		}
		return protojson.Marshal(info)
	}
}

// isUnspecifiedHost:""、0.0.0.0、:: 这类「监听所有网卡」的 host,不能原样写进 NodeInfo。
func isUnspecifiedHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// advertisedHost 决定写进 NodeInfo 的 IP:POD_IP 优先,其次 ListenOn 的具体 host,最后本机内网地址。
func advertisedHost(listenHost string) string {
	if podIP := os.Getenv("POD_IP"); podIP != "" {
		return podIP
	}
	if !isUnspecifiedHost(listenHost) {
		return listenHost
	}
	if ip := netx.InternalIp(); ip != "" {
		return ip
	}
	return "127.0.0.1"
}
