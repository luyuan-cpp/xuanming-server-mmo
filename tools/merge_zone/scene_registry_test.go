package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// zoneDownRegistry 是「注册表里该 zone 已一条注册都没有」的替身,供需要越过 P2 / 热状态门禁的用例使用。
type zoneDownRegistry struct{}

func (zoneDownRegistry) SceneNodesInZone(context.Context, uint32) (int, error) { return 0, nil }

// fixedRegistry 返回固定结果。
type fixedRegistry struct {
	n   int
	err error
}

func (r fixedRegistry) SceneNodesInZone(context.Context, uint32) (int, error) { return r.n, r.err }

// fakeEtcdGateway 模拟 etcd v3 JSON 网关的 /v3/kv/range:按前缀返回 values(原文 JSON),并核对请求形状。
func fakeEtcdGateway(t *testing.T, values []string, more bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v3/kv/range" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Key      string `json:"key"`
			RangeEnd string `json:"range_end"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		key, _ := base64.StdEncoding.DecodeString(req.Key)
		end, _ := base64.StdEncoding.DecodeString(req.RangeEnd)
		if string(key) != sceneNodeRegistryPrefix || string(end) != "SceneNodeService.rpc0" {
			http.Error(w, "unexpected range "+string(key)+" .. "+string(end), http.StatusBadRequest)
			return
		}
		type kv struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		resp := struct {
			Kvs  []kv `json:"kvs"`
			More bool `json:"more"`
		}{More: more}
		for i, v := range values {
			resp.Kvs = append(resp.Kvs, kv{
				Key:   base64.StdEncoding.EncodeToString([]byte(sceneNodeRegistryPrefix + string(rune('a'+i)))),
				Value: base64.StdEncoding.EncodeToString([]byte(v)),
			})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestEtcdGatewayRegistryCountsOnlyTheZonesNodes(t *testing.T) {
	srv := fakeEtcdGateway(t, []string{
		`{"nodeId":10,"zoneId":1,"nodeUuid":"u-10"}`,
		`{"nodeId":11,"zoneId":1}`,
		`{"nodeId":20,"zoneId":2}`,
		`{"nodeId":30}`, // zoneId 为 0 时 proto3 JSON 省略该字段
	}, false)
	defer srv.Close()
	reg, err := newEtcdGatewayRegistry(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for zone, want := range map[uint32]int{1: 2, 2: 1, 3: 0} {
		got, err := reg.SceneNodesInZone(context.Background(), zone)
		if err != nil || got != want {
			t.Errorf("zone %d: got %d, %v; want %d", zone, got, err, want)
		}
	}
}

// 解析不了的注册值可能正是该 zone 的节点:不能当成不存在。
func TestEtcdGatewayRegistryFailsClosedOnUnreadableValue(t *testing.T) {
	srv := fakeEtcdGateway(t, []string{`{"nodeId":10,"zoneId":2}`, `not json`}, false)
	defer srv.Close()
	reg, err := newEtcdGatewayRegistry(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := reg.SceneNodesInZone(context.Background(), 1); err == nil {
		t.Fatalf("unreadable registration accepted, count=%d", n)
	}
}

// more=true 说明没读全,不能拿半张表当全集。
func TestEtcdGatewayRegistryRefusesPartialRange(t *testing.T) {
	srv := fakeEtcdGateway(t, nil, true)
	defer srv.Close()
	reg, err := newEtcdGatewayRegistry(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.SceneNodesInZone(context.Background(), 1); err == nil {
		t.Fatal("partial range accepted")
	}
}

func TestEtcdGatewayRegistryFailsOverAndFailsClosed(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "etcdserver: leader changed", http.StatusServiceUnavailable)
	}))
	defer broken.Close()
	good := fakeEtcdGateway(t, []string{`{"nodeId":10,"zoneId":1}`}, false)
	defer good.Close()

	reg, err := newEtcdGatewayRegistry(broken.URL + "," + good.URL)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := reg.SceneNodesInZone(context.Background(), 1); err != nil || n != 1 {
		t.Fatalf("failover: got %d, %v; want 1 from the second endpoint", n, err)
	}

	onlyBroken, err := newEtcdGatewayRegistry(broken.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := onlyBroken.SceneNodesInZone(context.Background(), 1); err == nil {
		t.Fatal("all endpoints failed but no error returned")
	}
}

func TestNewEtcdGatewayRegistryParsesEndpoints(t *testing.T) {
	reg, err := newEtcdGatewayRegistry(" http://a:2379/ , https://b:2379,")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(reg.endpoints, ",") != "http://a:2379,https://b:2379" {
		t.Fatalf("endpoints = %v", reg.endpoints)
	}
	for _, bad := range []string{"", " , ", "etcd:2379", "ftp://a:2379", "http://"} {
		if _, err := newEtcdGatewayRegistry(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestEtcdPrefixEnd(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SceneNodeService.rpc/", "SceneNodeService.rpc0"},
		{"a\xff", "b"},
		{"\xff\xff", "\x00"},
	}
	for _, c := range cases {
		if got := string(etcdPrefixEnd([]byte(c.in))); got != c.want {
			t.Errorf("etcdPrefixEnd(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// assertZoneDown 先要注册表的正面证据;没有注册表、读失败、仍有注册都拒绝,而且这些情形都不去碰负载集
// (负载集为空不能单独证明下线,cross-zone-scene-travel.md §13.10)。rdb 传 nil:走到 ZCARD 就会 panic,
// 正好证明拒绝发生在它之前。
func TestAssertZoneDownRequiresRegistryEvidence(t *testing.T) {
	cases := []struct {
		name string
		reg  sceneRegistry
	}{
		{name: "没有注册表", reg: nil},
		{name: "注册表读失败", reg: fixedRegistry{err: errors.New("etcd unavailable")}},
		{name: "仍有注册", reg: fixedRegistry{n: 1}},
	}
	for _, c := range cases {
		err := assertZoneDown(context.Background(), nil, c.reg, 7)
		if !errors.Is(err, errZoneStillLive) {
			t.Errorf("%s: err = %v, want errZoneStillLive", c.name, err)
		}
	}
}
