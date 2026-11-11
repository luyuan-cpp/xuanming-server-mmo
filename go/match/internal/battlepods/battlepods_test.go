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
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeAPIServer 起一个只回 PodList 的假 API server,记下最后一次请求。
func fakeAPIServer(t *testing.T, status int, body string) (*Lookup, *http.Request) {
	t.Helper()
	var last http.Request
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = *r.Clone(context.Background())
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
	return &Lookup{Namespace: "mmorpg-infra", Host: host, Port: port, TokenPath: tokenPath, Client: server.Client()}, &last
}

func TestGoneOnlyWhenTheApiListsNoPodForTheIP(t *testing.T) {
	lookup, last := fakeAPIServer(t, http.StatusOK, `{"kind":"PodList","items":[]}`)
	gone, err := lookup.Gone(context.Background(), "10.1.2.3:20000")
	require.NoError(t, err)
	require.True(t, gone, "命名空间里没有 Pod 持有原 IP:原进程已死")
	require.Equal(t, "/api/v1/namespaces/mmorpg-infra/pods", last.URL.Path)
	require.Equal(t, "status.podIP=10.1.2.3", last.URL.Query().Get("fieldSelector"))
	require.Equal(t, "1", last.URL.Query().Get("limit"))
	require.Equal(t, "Bearer fixture-token", last.Header.Get("Authorization"), "令牌去掉首尾空白")
}

func TestPodStillHoldingTheIPIsNotGone(t *testing.T) {
	lookup, _ := fakeAPIServer(t, http.StatusOK, `{"kind":"PodList","items":[{"metadata":{"name":"battle-x","deletionTimestamp":"2026-11-11T00:00:00Z"}}]}`)
	gone, err := lookup.Gone(context.Background(), "10.1.2.3:20000")
	require.NoError(t, err)
	require.False(t, gone, "Pod 对象还在(哪怕在 Terminating):kubelet 还没确认容器已停")
}

func TestApiFailuresAreUndetermined(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"没有 list 权限":  {http.StatusForbidden, `{"kind":"Status","reason":"Forbidden"}`},
		"回的不是 PodList": {http.StatusOK, `{"kind":"Status"}`},
		"回包解不开":      {http.StatusOK, `not-json`},
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
