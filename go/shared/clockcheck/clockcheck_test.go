package clockcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// 偏差按回应的 Date 头算,回应状态码不论:匿名访问被拒的回应也带 Date。Date 读不懂时报错,不拿本机时钟顶替。
// 假 API server 把查询参数 date 原样放进 Date 头。
func TestMeasure_ComparesWithTheDateHeader(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", r.URL.Query().Get("date"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	reference := time.Date(2026, time.November, 19, 8, 0, 0, 0, time.UTC)
	local := reference.Add(-365 * 24 * time.Hour) // 本机被调回一年前
	target := srv.URL + "/version?date=" + url.QueryEscape(reference.Format(http.TimeFormat))
	offset, err := measure(context.Background(), srv.Client(), target, func() time.Time { return local })
	if err != nil {
		t.Fatal(err)
	}
	if want := -365*24*time.Hour - 500*time.Millisecond; offset != want {
		t.Fatalf("offset=%v, want %v", offset, want)
	}

	if got, err := measure(context.Background(), srv.Client(), srv.URL+"/version?date=not-a-date", time.Now); err == nil {
		t.Fatalf("offset=%v, want an error for an unreadable Date header", got)
	}
}

// 偏差在 MaxOffset 以内照常启动;超过就拒启,只有 warn 放行(大小写也要对)。
func TestJudge_RefusesBeyondMaxOffsetUnlessWarn(t *testing.T) {
	const oneYear = 365 * 24 * time.Hour
	for _, tc := range []struct {
		offset time.Duration
		mode   string
		refuse bool
	}{
		{MaxOffset, "", false},
		{-MaxOffset, "", false},
		{MaxOffset + time.Millisecond, "", true},
		{-MaxOffset - time.Millisecond, "enforce", true},
		{-oneYear, "warn", false},
		{-oneYear, "WARN", true},
	} {
		if err := judge(tc.offset, tc.mode); (err != nil) != tc.refuse {
			t.Errorf("judge(%v, %q) = %v, want refuse=%v", tc.offset, tc.mode, err, tc.refuse)
		}
	}
}
