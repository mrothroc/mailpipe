package document

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrIncomplete means no decision may be made from the discarded content.
var ErrIncomplete = errors.New("incomplete content; retained for review")

// ErrCleanup prevents further admissions after uncertain owned-resource cleanup.
var ErrCleanup = errors.New("content cleanup failed")

type contentError struct {
	code  string
	cause error
}

func (e *contentError) Error() string           { return "content: " + e.code + "; retained for review" }
func (e *contentError) Unwrap() error           { return e.cause }
func (e *contentError) Is(target error) bool    { return target == ErrIncomplete }
func incomplete(code string, cause error) error { return &contentError{code, cause} }

// Limits bounds each representation independently. All-zero selects defaults;
// otherwise every field must be positive. No field accepts an unlimited sentinel.
type Limits struct {
	RawBytes, DecodedBytes, PartBytes, TextBytes, PageBytes, PDFBytes int64
	SmallBytes, CompatibilityBytes, MemoryBytes, DiskBytes            int64
	HeaderBytes, TotalHeaderBytes                                     int64
	HeaderFields, Depth, Parts, HTMLBuffer, HTMLTokens, HTMLDepth     int
	Pipelines, Files, Reservations                                    int
	Timeout                                                           time.Duration
}

func DefaultLimits() Limits {
	return Limits{64 << 20, 128 << 20, 32 << 20, 8 << 20, 16 << 20, 64 << 20, 1 << 20, 64 << 20, 64 << 20, 256 << 20, 64 << 10, 1 << 20, 256, 20, 256, 64 << 10, 1000000, 128, 4, 1024, 1024, 2 * time.Minute}
}
func (l Limits) Resolve() (Limits, error) {
	if l == (Limits{}) {
		l = DefaultLimits()
	}
	values := []struct{ n, max int64 }{{l.RawBytes, 64 << 20}, {l.DecodedBytes, 256 << 20}, {l.PartBytes, 64 << 20}, {l.TextBytes, 32 << 20}, {l.PageBytes, 32 << 20}, {l.PDFBytes, 64 << 20}, {l.SmallBytes, 4 << 20}, {l.CompatibilityBytes, 64 << 20}, {l.MemoryBytes, 256 << 20}, {l.DiskBytes, 1 << 30}, {l.HeaderBytes, 256 << 10}, {l.TotalHeaderBytes, 8 << 20}, {int64(l.HeaderFields), 1024}, {int64(l.Depth), 32}, {int64(l.Parts), 1024}, {int64(l.HTMLBuffer), 256 << 10}, {int64(l.HTMLTokens), 4000000}, {int64(l.HTMLDepth), 256}, {int64(l.Pipelines), 16}, {int64(l.Files), 4096}, {int64(l.Reservations), 4096}, {int64(l.Timeout), int64(10 * time.Minute)}}
	for _, v := range values {
		if v.n <= 0 || v.n > v.max {
			return Limits{}, fmt.Errorf("content: invalid finite resource limit")
		}
	}
	// Header/map/descriptor estimates and bounded decoder stacks must fit one
	// admitted pipeline before any mail-derived allocation is attempted.
	minimum := parserReservation(l)
	if l.MemoryBytes < minimum || l.TotalHeaderBytes < l.HeaderBytes {
		return Limits{}, fmt.Errorf("content: inconsistent memory/header limits")
	}
	return l, nil
}
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

type scopeKey struct{}

// WithScope shares one operation's accounting across content consumers.
func WithScope(ctx context.Context, s *Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}
func ScopeFrom(ctx context.Context) *Scope { s, _ := ctx.Value(scopeKey{}).(*Scope); return s }

// parserReservation includes simultaneously live parent header/parameter maps,
// retained descriptor strings, decoder lookahead and derived token capacities.
// This conservative logical charge is distinct from allocator/RSS measurement.
func parserReservation(l Limits) int64 {
	return int64(l.Depth+1)*(32768+4096+l.HeaderBytes*4+int64(l.HeaderFields)*256) + l.TotalHeaderBytes*4 + int64(l.Parts)*1024 + int64(l.HTMLBuffer)*4
}
