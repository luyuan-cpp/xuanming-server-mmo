package kafkautil

import (
	"strings"
	"testing"
)

func TestPartitionContractMarkerIsStableAndUnambiguous(t *testing.T) {
	prefixA, markerA10 := partitionContractMarker("db_task_zone_1", 10)
	prefixAAgain, markerA20 := partitionContractMarker("db_task_zone_1", 20)
	prefixB, markerB10 := partitionContractMarker("db_task_zone_1_p10", 10)

	if prefixA != prefixAAgain || !strings.HasPrefix(markerA10, prefixA) || !strings.HasPrefix(markerA20, prefixA) {
		t.Fatalf("same topic must share one stable marker prefix")
	}
	if markerA10 == markerA20 {
		t.Fatalf("different partition contracts must produce different markers")
	}
	if prefixA == prefixB || markerA10 == markerB10 {
		t.Fatalf("different topic names must not share an ambiguous marker namespace")
	}
	if len(markerA10) > 249 {
		t.Fatalf("Kafka topic marker exceeds the 249-byte topic-name limit: %d", len(markerA10))
	}
}
