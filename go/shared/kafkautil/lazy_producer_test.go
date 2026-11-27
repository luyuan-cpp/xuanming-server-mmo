package kafkautil

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/IBM/sarama"
)

type stubSyncProducer struct {
	sent   atomic.Int32
	closed atomic.Bool
}

func (s *stubSyncProducer) SendMessage(*sarama.ProducerMessage) (int32, int64, error) {
	return 0, int64(s.sent.Add(1)), nil
}
func (s *stubSyncProducer) SendMessages([]*sarama.ProducerMessage) error { return nil }
func (s *stubSyncProducer) Close() error                                 { s.closed.Store(true); return nil }
func (s *stubSyncProducer) TxnStatus() sarama.ProducerTxnStatusFlag      { return 0 }
func (s *stubSyncProducer) IsTransactional() bool                        { return false }
func (s *stubSyncProducer) BeginTxn() error                              { return nil }
func (s *stubSyncProducer) CommitTxn() error                             { return nil }
func (s *stubSyncProducer) AbortTxn() error                              { return nil }
func (s *stubSyncProducer) AddOffsetsToTxn(map[string][]*sarama.PartitionOffsetMetadata, string) error {
	return nil
}
func (s *stubSyncProducer) AddMessageToTxn(*sarama.ConsumerMessage, string, *string) error {
	return nil
}

func TestLazySyncProducerRecoversAfterInitialDialFailure(t *testing.T) {
	var attempts atomic.Int32
	stub := &stubSyncProducer{}
	lp, err := NewLazySyncProducer(func() (sarama.SyncProducer, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("broker temporarily unreachable")
		}
		return stub, nil
	})
	if err != nil {
		t.Fatalf("NewLazySyncProducer: %v", err)
	}
	if attempts.Load() != 0 {
		t.Fatal("constructor must not perform network dial")
	}

	if _, _, err := lp.SendMessage(&sarama.ProducerMessage{Topic: "t"}); err == nil {
		t.Fatal("expected first send to fail on broker outage")
	}

	// Second send retries dial and heals automatically; concurrent sends share one dial.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, serr := lp.SendMessage(&sarama.ProducerMessage{Topic: "t"}); serr != nil {
				t.Errorf("SendMessage after recovery failed: %v", serr)
			}
		}()
	}
	wg.Wait()

	if attempts.Load() != 2 {
		t.Fatalf("dial attempts = %d, want 2", attempts.Load())
	}
	if stub.sent.Load() != 8 {
		t.Fatalf("sent = %d, want 8", stub.sent.Load())
	}
	if err := lp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !stub.closed.Load() {
		t.Fatal("underlying producer was not closed")
	}
	if _, _, err := lp.SendMessage(&sarama.ProducerMessage{Topic: "t"}); !errors.Is(err, ErrLazyProducerClosed) {
		t.Fatalf("send after Close err = %v, want ErrLazyProducerClosed", err)
	}
}

func TestLazySyncProducerCloseDuringDialClosesLeakedProducer(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	stub := &stubSyncProducer{}

	lp, err := NewLazySyncProducer(func() (sarama.SyncProducer, error) {
		close(started)
		<-release
		return stub, nil
	})
	if err != nil {
		t.Fatalf("NewLazySyncProducer: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, _, serr := lp.SendMessage(&sarama.ProducerMessage{Topic: "t"})
		errCh <- serr
	}()
	<-started
	if err := lp.Close(); err != nil {
		t.Fatalf("Close during dial: %v", err)
	}
	close(release)

	if err := <-errCh; !errors.Is(err, ErrLazyProducerClosed) {
		t.Fatalf("send racing with Close err = %v, want ErrLazyProducerClosed", err)
	}
	if !stub.closed.Load() {
		t.Fatal("producer constructed concurrently with Close must be closed immediately")
	}
}
