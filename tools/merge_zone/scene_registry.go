package main

// 源 zone 是否已下线的**正面证据**:etcd 场景节点注册表里该 zone 一条注册都没有。
//
// 以前 -clear-source-hot-state 只凭 scene_nodes:zone:{S}:load 为空判下线。负载键丢失 / 被淘汰时 ZCARD 读作 0,
// 活着的 zone 会被当成已下线,随后删掉活玩家的 location;玩家从别的 gate 进来会落新节点、铸新 epoch,持有节点的
// 存盘全被 owner CAS 拒绝,未落盘的进度回档(docs/design/cross-zone-scene-travel.md §13.9 末、§13.10)。
// scene_manager 的 EnterScene 一侧早已改为要 etcd 注册表的正面证据(isZoneGoneFromRegistry),这里用同一口径。
//
// 走 etcd 官方的 v3 JSON 网关(POST /v3/kv/range,etcd ≥ 3.4 默认开启),不引 etcd / gRPC 客户端:本工具刻意不带
// 那一串依赖(placement_codec.go 文件头)。本仓 etcd 不开认证(deploy/docker-compose.yml、
// deploy/k8s/manifests/infra/etcd.yaml 的 ALLOW_NONE_AUTHENTICATION);开了认证要先在这里补 token 再用。
//
// 读是线性一致的(网关默认 serializable=false),读不到就报错,调用方按「不能证明已下线」拒绝(fail-closed)。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// sceneNodeRegistryPrefix 是 C++ scene 节点在 etcd 注册的键前缀,与 go/scene_manager/internal/logic/load_reporter.go 的
// sceneNodePrefix 相同;值是 NodeInfo 的 protobuf JSON(zoneId 为 0 时按 proto3 JSON 规则省略)。
const sceneNodeRegistryPrefix = "SceneNodeService.rpc/"

// sceneRegistryReadTimeout 是单个 etcd 端点的读超时。
const sceneRegistryReadTimeout = 5 * time.Second

// sceneRegistry 报告某 zone 在场景节点注册表里还有几条注册。
type sceneRegistry interface {
	SceneNodesInZone(ctx context.Context, zone uint32) (int, error)
}

// etcdGatewayRegistry 经 etcd v3 JSON 网关读注册表。端点按顺序尝试,第一个成功的为准。
type etcdGatewayRegistry struct {
	endpoints []string
	client    *http.Client
}

// newEtcdGatewayRegistry 解析 -etcd-endpoints(逗号分隔,带 http:// 或 https://)。
func newEtcdGatewayRegistry(raw string) (*etcdGatewayRegistry, error) {
	var endpoints []string
	for _, part := range strings.Split(raw, ",") {
		ep := strings.TrimRight(strings.TrimSpace(part), "/")
		if ep == "" {
			continue
		}
		u, err := url.Parse(ep)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("-etcd-endpoints: %q 不是 http(s)://host:port 形式", part)
		}
		endpoints = append(endpoints, ep)
	}
	if len(endpoints) == 0 {
		return nil, errors.New("-etcd-endpoints 为空")
	}
	return &etcdGatewayRegistry{endpoints: endpoints, client: &http.Client{Timeout: sceneRegistryReadTimeout}}, nil
}

// etcdRangeResponse 是 /v3/kv/range 应答里本工具用到的部分(键与值都是 base64)。
type etcdRangeResponse struct {
	Kvs []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"kvs"`
	More bool `json:"more"`
}

// SceneNodesInZone 读整个前缀,数 zoneId 等于 zone 的注册。任何一条值解析不了都报错:它可能正是该 zone 的节点。
func (r *etcdGatewayRegistry) SceneNodesInZone(ctx context.Context, zone uint32) (int, error) {
	body, err := json.Marshal(map[string]string{
		"key":       base64.StdEncoding.EncodeToString([]byte(sceneNodeRegistryPrefix)),
		"range_end": base64.StdEncoding.EncodeToString(etcdPrefixEnd([]byte(sceneNodeRegistryPrefix))),
	})
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, ep := range r.endpoints {
		resp, err := r.rangeAt(ctx, ep, body)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ep, err))
			continue
		}
		return countSceneNodesInZone(resp, zone)
	}
	return 0, fmt.Errorf("etcd 场景节点注册表读取失败: %w", errors.Join(errs...))
}

func (r *etcdGatewayRegistry) rangeAt(ctx context.Context, endpoint string, body []byte) (etcdRangeResponse, error) {
	var out etcdRangeResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v3/kv/range", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return out, err
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return out, fmt.Errorf("应答不是 range JSON: %w", err)
	}
	if out.More {
		// 没带 limit 的 range 不应分页;出现就说明网关语义变了,不能拿半张表当全集。
		return out, errors.New("range 应答 more=true,注册表没有读全")
	}
	return out, nil
}

// countSceneNodesInZone 数应答里 zoneId == zone 的注册。
func countSceneNodesInZone(resp etcdRangeResponse, zone uint32) (int, error) {
	n := 0
	for _, kv := range resp.Kvs {
		raw, err := base64.StdEncoding.DecodeString(kv.Value)
		if err != nil {
			return 0, fmt.Errorf("注册值不是 base64(key=%q): %w", kv.Key, err)
		}
		var node struct {
			ZoneID uint32 `json:"zoneId"`
		}
		if err := json.Unmarshal(raw, &node); err != nil {
			return 0, fmt.Errorf("注册值不是 NodeInfo JSON(key=%q): %w", kv.Key, err)
		}
		if node.ZoneID == zone {
			n++
		}
	}
	return n, nil
}

// etcdPrefixEnd 是 etcd 前缀查询的 range_end(clientv3.GetPrefixRangeEnd 同一算法):最后一个不是 0xff 的字节加一并截断。
func etcdPrefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	// 全是 0xff:按 etcd 约定用 "\x00" 表示「直到键空间末尾」。
	return []byte{0}
}
