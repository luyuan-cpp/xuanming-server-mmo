package playerstore

import (
	"context"
	"errors"
	"testing"
)

type fakeRouteReader struct {
	storageID uint32
	found     bool
	err       error
	calls     int
}

func (f *fakeRouteReader) StorageOf(_ context.Context, _ uint64) (uint32, bool, error) {
	f.calls++
	return f.storageID, f.found, f.err
}

// 选库的结局。读错不能被当成没有落点:没有落点要查数据,读错只要等目录恢复。
func TestResolveOutcomes(t *testing.T) {
	readFailure := errors.New("rpc error: code = Unavailable")
	cases := []struct {
		name    string
		reader  fakeRouteReader
		want    uint32
		wantErr error
	}{
		{name: "路由读到", reader: fakeRouteReader{storageID: 7, found: true}, want: 7},
		{name: "非 zone 落点", reader: fakeRouteReader{storageID: 1000000, found: true}, want: 1000000},
		{name: "目录确定没有落点", reader: fakeRouteReader{}, wantErr: ErrPlayerStorageMissing},
		{name: "storage_id 为 0", reader: fakeRouteReader{found: true}, wantErr: ErrPlayerStorageMissing},
		{name: "目录读错", reader: fakeRouteReader{err: readFailure}, wantErr: readFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := tc.reader
			resolve, err := NewStorageResolver(&reader)
			if err != nil {
				t.Fatal(err)
			}
			got, err := resolve(context.Background(), 42)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("storage = %d, want %d", got, tc.want)
			}
			if tc.reader.err != nil && errors.Is(err, ErrPlayerStorageMissing) {
				t.Fatalf("a read failure must not look like a missing placement: %v", err)
			}
			if reader.calls != 1 {
				t.Fatalf("a claim reads the route exactly once: %d", reader.calls)
			}
		})
	}
}

func TestNewStorageResolverRejectsBadConfig(t *testing.T) {
	if _, err := NewStorageResolver(nil); err == nil {
		t.Fatal("a nil reader must be rejected")
	}
}
