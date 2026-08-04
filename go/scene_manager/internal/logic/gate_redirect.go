package logic

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	commonpb "proto/common/base"
	"proto/scene_manager"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	redirectTokenTTLSeconds = 300
	gateEtcdPrefix          = "GateNodeService.rpc"
)

// AssignGateForZone picks the least-loaded Gate in the target zone and signs
// a redirect token. Returns nil if no gate is available.
func AssignGateForZone(ctx context.Context, svcCtx *svc.ServiceContext, targetZoneId uint32) (*scene_manager.RedirectToGateInfo, error) {
	if svcCtx.Config.GateTokenSecret == "" {
		return nil, fmt.Errorf("GateTokenSecret not configured, cannot sign redirect token")
	}

	gates, err := fetchGateNodes(ctx, svcCtx, targetZoneId)
	if err != nil {
		return nil, fmt.Errorf("fetch gate nodes for zone %d: %w", targetZoneId, err)
	}
	if len(gates) == 0 {
		return nil, fmt.Errorf("no gate nodes available for zone %d", targetZoneId)
	}

	// Pick least-loaded gate.
	sort.Slice(gates, func(i, j int) bool {
		return gates[i].PlayerCount < gates[j].PlayerCount
	})
	best := gates[0]

	if best.Endpoint == nil {
		return nil, fmt.Errorf("best gate node %d has no endpoint", best.NodeId)
	}

	// Build and sign the token (same format as Login service).
	expireTS := time.Now().Unix() + redirectTokenTTLSeconds
	payload := &commonpb.GateTokenPayload{
		GateNodeId:      best.NodeId,
		ZoneId:          best.ZoneId,
		ExpireTimestamp: expireTS,
	}
	payloadBytes, err := proto.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal token payload: %w", err)
	}

	signature := signHMAC(svcCtx.Config.GateTokenSecret, payloadBytes)

	return &scene_manager.RedirectToGateInfo{
		TargetGateIp:   best.Endpoint.Ip,
		TargetGatePort: best.Endpoint.Port,
		TokenPayload:   payloadBytes,
		TokenSignature: signature,
		TokenDeadline:  expireTS,
	}, nil
}

// fetchGateNodes queries etcd for Gate nodes in the given zone.
func fetchGateNodes(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32) ([]*commonpb.NodeInfo, error) {
	gateNodeType := uint32(commonpb.ENodeType_GateNodeService)
	prefix := fmt.Sprintf("%s/zone/%d/node_type/%d/", gateEtcdPrefix, zoneId, gateNodeType)

	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := svcCtx.Etcd.Get(fetchCtx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}

	nodes := make([]*commonpb.NodeInfo, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		info := &commonpb.NodeInfo{}
		if err := protojson.Unmarshal(kv.Value, info); err != nil {
			logx.Errorf("fetchGateNodes: invalid NodeInfo at key=%s: %v", string(kv.Key), err)
			continue
		}
		if info.Endpoint != nil {
			nodes = append(nodes, info)
		}
	}
	return nodes, nil
}

// signHMAC computes HMAC-SHA256 and returns hex-encoded bytes (matches Login and Java Gateway).
func signHMAC(secret string, data []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data)
	return []byte(hex.EncodeToString(mac.Sum(nil)))
}
