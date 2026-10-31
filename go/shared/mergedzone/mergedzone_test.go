package mergedzone

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	dspb "proto/data_service"
	"shared/placement"
)

// fakeClient 依次回答 answers,用完之后一直回答最后一项。
type fakeClient struct {
	answers []fakeAnswer
	calls   int
}

type fakeAnswer struct {
	merges map[uint32]uint32
	err    error
}

func (c *fakeClient) GetMergedZones(context.Context, *emptypb.Empty, ...grpc.CallOption) (*dspb.GetMergedZonesResponse, error) {
	answer := c.answers[min(c.calls, len(c.answers)-1)]
	c.calls++
	if answer.err != nil {
		return nil, answer.err
	}
	return &dspb.GetMergedZonesResponse{MergedInto: answer.merges}, nil
}

func noWait(context.Context) error { return nil }

func TestLoad_BuildsCacheFromTheRPC(t *testing.T) {
	client := &fakeClient{answers: []fakeAnswer{{merges: map[uint32]uint32{5: 6, 6: 7}}}}
	cache, err := Load(context.Background(), client, placement.MergedZoneCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	zones := cache.Snapshot()
	if got := zones.Final(5); got != 7 {
		t.Fatalf("Final(5) = %d, want 7 (the RPC answers raw edges, the chain is resolved here)", got)
	}
	if client.calls != 1 {
		t.Fatalf("calls = %d", client.calls)
	}
}

func TestLoad_EmptyAnswerMeansNoMerges(t *testing.T) {
	cache, err := Load(context.Background(), &fakeClient{answers: []fakeAnswer{{}}}, placement.MergedZoneCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	zones := cache.Snapshot()
	if zones == nil || zones.Final(5) != 5 {
		t.Fatalf("snapshot = %v", zones)
	}
}

func TestLoad_NoClientIsAnError(t *testing.T) {
	if _, err := Load(context.Background(), nil, placement.MergedZoneCacheOptions{}); err == nil {
		t.Fatal("no client must refuse: the caller cannot tell no merges from not read")
	}
}

func TestLoad_RetriesUntilDataServiceAnswers(t *testing.T) {
	down := errors.New("no endpoints")
	client := &fakeClient{answers: []fakeAnswer{{err: down}, {err: down}, {merges: map[uint32]uint32{5: 6}}}}
	waits := 0
	cache, err := load(context.Background(), reader(client), placement.MergedZoneCacheOptions{}, func(context.Context) error {
		waits++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cache.Snapshot().Final(5); got != 6 || waits != 2 || client.calls != 3 {
		t.Fatalf("Final(5)=%d waits=%d calls=%d", got, waits, client.calls)
	}
}

func TestLoad_BudgetExhaustedReturnsTheLastError(t *testing.T) {
	down := errors.New("no endpoints")
	expired := errors.New("budget used up")
	client := &fakeClient{answers: []fakeAnswer{{err: down}}}
	cache, err := load(context.Background(), reader(client), placement.MergedZoneCacheOptions{}, func(context.Context) error { return expired })
	if cache != nil || !errors.Is(err, down) || !errors.Is(err, expired) {
		t.Fatalf("cache=%v err=%v", cache, err)
	}
}

func TestLoad_BrokenMappingIsNotRetried(t *testing.T) {
	client := &fakeClient{answers: []fakeAnswer{{merges: map[uint32]uint32{5: 6, 6: 5}}}}
	_, err := load(context.Background(), reader(client), placement.MergedZoneCacheOptions{}, noWait)
	if !errors.Is(err, placement.ErrMergedZoneUnresolvable) {
		t.Fatalf("err = %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("calls = %d: a broken mapping does not heal by retrying", client.calls)
	}
}

func TestLoad_CallerCancelStopsWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &fakeClient{answers: []fakeAnswer{{err: errors.New("no endpoints")}}}
	if _, err := Load(ctx, client, placement.MergedZoneCacheOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
}

// 定时重读由调用方 Start;Load 自己只读一次,没有 Start 过的缓存 Stop 立刻返回。
func TestLoad_DoesNotStartRefreshing(t *testing.T) {
	client := &fakeClient{answers: []fakeAnswer{{}}}
	cache, err := Load(context.Background(), client, placement.MergedZoneCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cache.Stop()
	if client.calls != 1 {
		t.Fatalf("calls = %d", client.calls)
	}
}
