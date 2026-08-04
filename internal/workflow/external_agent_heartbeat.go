package workflow

import (
	"context"

	"go.temporal.io/sdk/activity"
)

func init() {
	activityHeartbeat = func(ctx context.Context, details ...any) {
		activity.RecordHeartbeat(ctx, details...)
	}
}
