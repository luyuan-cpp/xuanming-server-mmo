package saveoutbox

import (
	"context"
	"github.com/prometheus/client_golang/prometheus"
	"testing"
	"time"
)

func TestHistoricalDrainUsesOriginalGenerationAndBodies(t *testing.T) {
	rc := fixtureRedis(t)
	publisher := &recordingPublisher{}
	replay, err := New(rc, rc, publisher, nil, openRouteGate(), Config{ZoneID: 1, TopicGeneration: 2}, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	in := testIntent(t, 17, 3, 4, 1)
	in.topic = topicFor(1, 1)
	storeIntent(t, rc, 17, 1, in)
	left, err := replay.DrainHistorical(context.Background(), []string{topicFor(1, 1)})
	if err != nil || left != 1 {
		t.Fatalf("历史补投: left=%d err=%v", left, err)
	}
	if len(publisher.tasks) != 2 {
		t.Fatal("原两条任务未完整发布")
	}
	if publisher.tasks[0].topic != in.topic || publisher.tasks[0].body != in.first || publisher.tasks[1].body != in.second {
		t.Fatal("历史topic或body被改写")
	}
	if rc.ZCard(context.Background(), pendingIndexKey(1, 17%BucketCount)).Val() != 1 {
		t.Fatal("未完成pending被删除")
	}
}
func TestHistoricalDrainRestoresUnindexedOriginalIntent(t *testing.T) {
	rc := fixtureRedis(t)
	replay := testReplay(t, rc, &recordingPublisher{})
	in := testIntent(t, 99, 3, 4, 1)
	storeIntent(t, rc, 99, 1, in)
	rc.ZRem(context.Background(), pendingIndexKey(1, 99%BucketCount), "99")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if left, err := replay.DrainHistorical(ctx, []string{in.topic}); err != nil || left != 1 {
		t.Fatalf("原未索引意图未补回并投递: left=%d err=%v", left, err)
	}
	if rc.ZCard(ctx, pendingIndexKey(1, 99%BucketCount)).Val() != 1 {
		t.Fatal("未完成索引未保留")
	}
	if rc.HGet(context.Background(), "player:{99}:save_barrier", fieldFirst).Val() != in.first {
		t.Fatal("HOLD丢失原字节")
	}
}
