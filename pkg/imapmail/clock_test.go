package imapmail

import (
	"context"
	"time"
)

type fakeClock struct{ waits *[]time.Duration }

func (f *fakeClock) Wait(ctx context.Context, d time.Duration) error {
	*f.waits = append(*f.waits, d)
	return ctx.Err()
}

type waitFunc func(context.Context, time.Duration) error

func (f waitFunc) Wait(ctx context.Context, d time.Duration) error { return f(ctx, d) }
