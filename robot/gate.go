package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"

	"robot/config"
	"robot/pkg"
)

type gateAssignment struct {
	addr      string
	payload   []byte
	signature []byte
}

// resolveGateAddr calls the Gateway HTTP API to get the best gate endpoint.
func resolveGateAddr(cfg *config.Config) (host, port string, payload, signature []byte, err error) {
	zoneID := cfg.ZoneID
	if zoneID == 0 {
		zoneID, err = resolveZoneID(cfg.GatewayAddr)
		if err != nil {
			zap.L().Warn("server-list unavailable, using zone_id=0", zap.Error(err))
			zoneID = 0
		}
	}

	const maxRetries = 30
	backoff := 2 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		result, assignErr := assignGateHTTP(cfg.GatewayAddr, zoneID)
		if assignErr == nil {
			zap.L().Info("assigned gate via HTTP",
				zap.String("gate", result.addr),
				zap.Int("attempt", attempt),
			)
			h, p, splitErr := net.SplitHostPort(result.addr)
			return h, p, result.payload, result.signature, splitErr
		}

		zap.L().Warn("HTTP AssignGate failed, retrying",
			zap.String("gateway_addr", cfg.GatewayAddr),
			zap.Int("attempt", attempt),
			zap.Int("max", maxRetries),
			zap.Error(assignErr),
		)

		if attempt < maxRetries {
			time.Sleep(backoff)
			if backoff < 10*time.Second {
				backoff = backoff * 3 / 2
			}
		}
	}

	return "", "", nil, nil, fmt.Errorf("AssignGate failed after %d retries", maxRetries)
}

// assignGateHTTP calls POST /api/assign-gate on the Gateway HTTP API.
// resolveZoneID calls GET /api/server-list and picks the recommended zone.
func resolveZoneID(gatewayAddr string) (uint32, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatewayAddr+"/api/server-list", nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var result struct {
		Zones []struct {
			ZoneID      uint32 `json:"zone_id"`
			Name        string `json:"name"`
			Status      string `json:"status"`
			Recommended bool   `json:"recommended"`
		} `json:"zones"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("decode server-list: %w", err)
	}
	if len(result.Zones) == 0 {
		return 0, fmt.Errorf("server-list returned no zones")
	}

	// Pick recommended zone, or first available.
	for _, z := range result.Zones {
		if z.Recommended {
			zap.L().Info("auto-selected zone from server-list",
				zap.Uint32("zone_id", z.ZoneID),
				zap.String("name", z.Name),
			)
			return z.ZoneID, nil
		}
	}
	picked := result.Zones[0]
	zap.L().Info("auto-selected first zone from server-list",
		zap.Uint32("zone_id", picked.ZoneID),
		zap.String("name", picked.Name),
	)
	return picked.ZoneID, nil
}

func assignGateHTTP(gatewayAddr string, zoneId uint32) (*gateAssignment, error) {
	reqBody, _ := json.Marshal(map[string]uint32{"zone_id": zoneId})

	url := gatewayAddr + "/api/assign-gate"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP POST %s: %w", url, err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", httpResp.StatusCode, string(body))
	}

	var resp struct {
		GateIP         string `json:"gate_ip"`
		GatePort       uint32 `json:"gate_port"`
		GateWsURL      string `json:"gate_ws_url"`
		TokenPayload   []byte `json:"token_payload"`
		TokenSignature []byte `json:"token_signature"`
		Error          string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("AssignGate: %s", resp.Error)
	}
	if resp.GateIP == "" || resp.GatePort == 0 {
		return nil, fmt.Errorf("AssignGate returned empty address")
	}
	// 小游戏连这台 gate 的 WebSocket 地址:登记下来,连接时按 ip:port 查(pkg/transport.go)。
	pkg.AdvertiseWsURL(resp.GateIP, int(resp.GatePort), resp.GateWsURL)

	return &gateAssignment{
		addr:      fmt.Sprintf("%s:%d", resp.GateIP, resp.GatePort),
		payload:   resp.TokenPayload,
		signature: resp.TokenSignature,
	}, nil
}
