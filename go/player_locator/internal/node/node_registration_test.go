package node

import (
	"testing"
)

// 标记键如何展开、与 NodeInfo 同一笔 Txn 同一个租约,由 shared/noderegistry 的 lease_bound_markers_test.go 钉住;
// 这里只核对适配层把前缀按传入顺序收集齐、原样交给 shared。

// WithLeaseBoundMarkers 可叠加,NewNode 按传入顺序收集前缀。
func TestWithLeaseBoundMarkersAccumulates(t *testing.T) {
	var o nodeOptions
	for _, opt := range []Option{WithLeaseBoundMarkers("/a/"), WithLeaseBoundMarkers("/b/", "/c/")} {
		opt(&o)
	}
	want := []string{"/a/", "/b/", "/c/"}
	if len(o.markerPrefixes) != len(want) {
		t.Fatalf("markerPrefixes = %v, want %v", o.markerPrefixes, want)
	}
	for i := range want {
		if o.markerPrefixes[i] != want[i] {
			t.Fatalf("markerPrefixes = %v, want %v", o.markerPrefixes, want)
		}
	}
}
