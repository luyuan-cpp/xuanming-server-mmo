package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"robot/config"
)

func TestTravelSmokeRejectsWhitespaceAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "travel.yaml")
	data := []byte("gateway_addr: http://127.0.0.1:58900\nmode: travel-smoke\ntravel_smoke:\n  home_zone: 990017\n  visit_zone: 990018\n  account: ' robot_assetarrival_travel_20261017'\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("传送配置必须在登录前拒绝带首尾空白的专用账号")
	}
}

func TestTravelSmokeAccountConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		account string
		omit    bool
		want    string
		invalid bool
	}{
		{name: "旧配置缺省", omit: true, want: "robot_9601"},
		{name: "显式空串兼容", want: "robot_9601"},
		{name: "专用白名单账号", account: "robot_assetarrival_travel_20261017", want: "robot_assetarrival_travel_20261017"},
		{name: "191字符边界", account: "robot_" + strings.Repeat("a", 185), want: "robot_" + strings.Repeat("a", 185)},
		{name: "UTF8字符边界", account: "robot_" + strings.Repeat("角", 185), want: "robot_" + strings.Repeat("角", 185)},
		{name: "纯空白", account: " \t", invalid: true},
		{name: "首空白", account: " robot_assetarrival_travel_20261017", invalid: true},
		{name: "尾空白", account: "robot_assetarrival_travel_20261017\n", invalid: true},
		{name: "缺少开发前缀", account: "assetarrival_travel_20261017", invalid: true},
		{name: "192字符越界", account: "robot_" + strings.Repeat("a", 186), invalid: true},
		{name: "UTF8字符越界", account: "robot_" + strings.Repeat("角", 186), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "travel.yaml")
			data := "gateway_addr: http://127.0.0.1:58900\nmode: travel-smoke\ntravel_smoke:\n  home_zone: 990017\n  visit_zone: 990018\n"
			if !tc.omit {
				data += fmt.Sprintf("  account: %q\n", tc.account)
			}
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := config.Load(path)
			if tc.invalid {
				if err == nil {
					t.Fatal("无效账号必须在任何登录副作用前被拒绝")
				}
				return
			}
			if err != nil {
				t.Fatalf("兼容配置加载失败: %v", err)
			}
			if got.TravelSmoke.Account != tc.want || got.TravelSmoke.DwellSeconds != 35 {
				t.Fatal("显式账号或原有停留缺省值在正式配置装配时丢失")
			}
		})
	}
}

// hop_timeout_seconds:缺省 60(旧配置不写也照旧);验收冻结硬上限时可调大;0 / 负数在登录前就拒绝,
// 不能让一跳预算变成"立刻超时"。
func TestTravelSmokeHopTimeoutConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		line    string
		want    int
		invalid bool
	}{
		{name: "旧配置缺省", want: 60},
		{name: "验收冻结上限", line: "  hop_timeout_seconds: 90\n", want: 90},
		{name: "零", line: "  hop_timeout_seconds: 0\n", invalid: true},
		{name: "负数", line: "  hop_timeout_seconds: -1\n", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "travel.yaml")
			data := "gateway_addr: http://127.0.0.1:58900\nmode: travel-smoke\ntravel_smoke:\n  home_zone: 990017\n  visit_zone: 990018\n" + tc.line
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := config.Load(path)
			if tc.invalid {
				if err == nil {
					t.Fatal("非正的一跳预算必须在任何登录副作用前被拒绝")
				}
				return
			}
			if err != nil {
				t.Fatalf("配置加载失败: %v", err)
			}
			if got.TravelSmoke.HopTimeoutSeconds != tc.want {
				t.Fatalf("hop_timeout_seconds = %d, want %d", got.TravelSmoke.HopTimeoutSeconds, tc.want)
			}
		})
	}
}
