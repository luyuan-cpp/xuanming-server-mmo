package battlepods

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeAPIServer 起一个只回固定回包的假 API server,记下最后一次请求(处理函数在另一个 goroutine 里,用原子指针交回)。
func fakeAPIServer(t *testing.T, status int, body string) (*Lookup, *atomic.Pointer[http.Request]) {
	t.Helper()
	last := &atomic.Pointer[http.Request]{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last.Store(r.Clone(context.Background()))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(address.Host)
	require.NoError(t, err)
	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("fixture-token\n"), 0o600))
	return &Lookup{Namespace: "mmorpg-infra", Host: host, Port: port, TokenPath: tokenPath, Client: server.Client()}, last
}

func TestGoneWhenTheApiListsNoPodForTheIP(t *testing.T) {
	lookup, last := fakeAPIServer(t, http.StatusOK, `{"kind":"PodList","metadata":{},"items":[]}`)
	gone, err := lookup.Gone(context.Background(), "10.1.2.3:20000")
	require.NoError(t, err)
	require.True(t, gone, "命名空间里没有 Pod 持有原 IP:原进程已死")
	request := last.Load()
	require.NotNil(t, request, "确实向 API server 发了请求")
	require.Equal(t, "/api/v1/namespaces/mmorpg-infra/pods", request.URL.Path)
	require.Equal(t, "status.podIP=10.1.2.3", request.URL.Query().Get("fieldSelector"))
	require.Equal(t, listLimit, request.URL.Query().Get("limit"))
	require.Equal(t, "Bearer fixture-token", request.Header.Get("Authorization"), "令牌去掉首尾空白")
}

func TestGoneDependsOnWhetherAnyPodHoldingTheIPIsAlive(t *testing.T) {
	cases := map[string]struct {
		body string
		gone bool
	}{
		"Running":                {`{"kind":"PodList","items":[{"status":{"phase":"Running"}}]}`, false},
		"Pending":                {`{"kind":"PodList","items":[{"status":{"phase":"Pending"}}]}`, false},
		"节点失联(Unknown)":          {`{"kind":"PodList","items":[{"status":{"phase":"Unknown"}}]}`, false},
		"没有 phase":               {`{"kind":"PodList","items":[{"status":{}}]}`, false},
		"Terminating 但容器还没确认停":   {`{"kind":"PodList","items":[{"metadata":{"deletionTimestamp":"2026-11-11T00:00:00Z"},"status":{"phase":"Running"}}]}`, false},
		"容器都已终止(Failed)":         {`{"kind":"PodList","items":[{"status":{"phase":"Failed"}}]}`, true},
		"容器都已终止(Succeeded)":      {`{"kind":"PodList","items":[{"status":{"phase":"Succeeded"}}]}`, true},
		"旧 Pod 已终止、IP 被新 Pod 占用": {`{"kind":"PodList","items":[{"status":{"phase":"Failed"}},{"status":{"phase":"Running"}}]}`, false},
		"两个都已终止":                 {`{"kind":"PodList","items":[{"status":{"phase":"Failed"}},{"status":{"phase":"Succeeded"}}]}`, true},
	}
	for why, c := range cases {
		lookup, _ := fakeAPIServer(t, http.StatusOK, c.body)
		gone, err := lookup.Gone(context.Background(), "10.1.2.3:20000")
		require.NoError(t, err, why)
		require.Equal(t, c.gone, gone, why)
	}
}

func TestApiFailuresAreUndetermined(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"没有 list 权限":   {http.StatusForbidden, `{"kind":"Status","reason":"Forbidden"}`},
		"回的不是 PodList": {http.StatusOK, `{"kind":"Status"}`},
		"回包解不开":        {http.StatusOK, `not-json`},
		"一页没取完":        {http.StatusOK, `{"kind":"PodList","metadata":{"continue":"next"},"items":[{"status":{"phase":"Failed"}}]}`},
	}
	for why, c := range cases {
		lookup, _ := fakeAPIServer(t, c.status, c.body)
		gone, err := lookup.Gone(context.Background(), "10.1.2.3:20000")
		require.Error(t, err, why)
		require.False(t, gone, why)
	}
}

func TestLookupWithoutConfigurationOrLiteralIPIsUndetermined(t *testing.T) {
	gone, err := (&Lookup{}).Gone(context.Background(), "10.1.2.3:20000")
	require.True(t, errors.Is(err, ErrNotConfigured))
	require.False(t, gone)
	gone, err = InCluster("").Gone(context.Background(), "10.1.2.3:20000")
	require.True(t, errors.Is(err, ErrNotConfigured), "没配命名空间 = 不查 Pod")
	require.False(t, gone)

	lookup, _ := fakeAPIServer(t, http.StatusOK, `{"kind":"PodList","items":[]}`)
	for _, endpoint := range []string{"battle-0.battle:20000", "10.1.2.3", ""} {
		gone, err = lookup.Gone(context.Background(), endpoint)
		require.Error(t, err, endpoint)
		require.False(t, gone, "地址里没有字面 Pod IP:判不了 %q", endpoint)
	}
}
