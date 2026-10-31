package drain

import (
	"context"
	"testing"
)

func TestRunRejectsMissingPhysicalOwnersBeforeStarting(t *testing.T) {
	if err := Run(context.Background(), nil, nil); err == nil {
		t.Fatal("缺真实owner却开始排空")
	}
}
