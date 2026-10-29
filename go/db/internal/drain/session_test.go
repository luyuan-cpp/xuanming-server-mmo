package drain

import (
	"context"
	"db/internal/config"
	"testing"
)

func TestRunRejectsMissingPhysicalOwnersBeforeStarting(t *testing.T) {
	if err := Run(context.Background(), nil, config.Config{}, nil, nil, nil); err == nil {
		t.Fatal("缺真实owner却开始排空")
	}
}
