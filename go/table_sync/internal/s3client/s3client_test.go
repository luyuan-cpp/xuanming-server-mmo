package s3client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSigV4PutHeadGetAndSizeLimit(t *testing.T) {
	var mu sync.Mutex
	objects := map[string][]byte{}
	var seenAuth []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			objects[r.URL.Path] = body
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			if _, ok := objects[r.URL.Path]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			data, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	cli, err := New(Config{
		Endpoint:  srv.URL,
		Region:    "ap-east-1",
		Bucket:    "mmorpg-tables",
		AccessKey: "AKIA_TEST",
		SecretKey: "SECRET_TEST",
		PathStyle: true,
	})
	require.NoError(t, err)
	cli.nowFn = func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) }

	ctx := context.Background()
	key := "tables/0123456789abcdef/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.tar.gz"

	exists, err := cli.HeadObject(ctx, key)
	require.NoError(t, err)
	require.False(t, exists)

	payload := []byte("hello-table-bundle-payload")
	require.NoError(t, cli.PutObject(ctx, key, payload, "application/gzip"))

	exists, err = cli.HeadObject(ctx, key)
	require.NoError(t, err)
	require.True(t, exists)

	got, err := cli.GetObjectBytes(ctx, key, 1024)
	require.NoError(t, err)
	require.Equal(t, payload, got)

	// 超过 maxBytes 上限必须拒绝。
	var tooSmall bytes.Buffer
	_, err = cli.GetObjectToWriter(ctx, key, &tooSmall, 4)
	require.Error(t, err)

	// 每次请求都携带 AWS4-HMAC-SHA256 签名头。
	require.NotEmpty(t, seenAuth)
	for _, h := range seenAuth {
		require.Contains(t, h, "AWS4-HMAC-SHA256 Credential=AKIA_TEST/20261120/ap-east-1/s3/aws4_request")
	}
}
