package recognize

import (
	"context"
	"testing"
)

func TestIsPriority(t *testing.T) {
	if IsPriority(context.Background()) {
		t.Error("一般 ctx 不應是優先")
	}
	if !IsPriority(WithPriority(context.Background())) {
		t.Error("WithPriority 標記後應是優先")
	}
}
