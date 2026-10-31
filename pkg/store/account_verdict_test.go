package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func verdictAccount(t *testing.T) (*AccountStore, []mailbox.LocalRef) {
	t.Helper()
	ctx := context.Background()
	a, err := OpenAccount(ctx, filepath.Join(t.TempDir(), "mail.db"), mailbox.Account{ID: "invented-account", ProviderID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	c := mailbox.Collection{Ref: mailbox.CollectionRef{AccountID: a.Account().ID, CollectionID: "inbox"}}
	var refs []mailbox.LocalRef
	for _, id := range []string{"one", "two"} {
		r, err := a.UpsertMessage(ctx, c, MessageRecord{Message: mailbox.Message{Ref: mailbox.MessageRef{AccountID: a.Account().ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)}, Metadata: mailbox.Metadata{Sender: mailbox.Address{Address: "sender@example.org"}}}})
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, r)
	}
	return a, refs
}

func TestAccountVerdictsBatch(t *testing.T) {
	ctx := context.Background()
	t.Run("order duplicates and caller values", func(t *testing.T) {
		a, refs := verdictAccount(t)
		batch := []BoundVerdict{{Local: refs[1], Verdict: Verdict{Stage: "first"}}, {Local: refs[0], Verdict: Verdict{Stage: "middle"}}, {Local: refs[1], Verdict: Verdict{Stage: "last"}}}
		if err := a.AddVerdictsBatch(ctx, batch); err != nil {
			t.Fatal(err)
		}
		if batch[0].Verdict.N != 0 {
			t.Fatal("caller mutated")
		}
		v, err := a.Verdicts(ctx, refs[1])
		if err != nil || len(v) != 2 || v[0].Stage != "first" || v[1].Stage != "last" || v[1].ID-v[0].ID != 2 {
			t.Fatalf("order: %+v %v", v, err)
		}
	})
	for _, fault := range []string{"account", "missing", "retired", "n", "json", "sql", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			a, refs := verdictAccount(t)
			batch := []BoundVerdict{{Local: refs[0], Verdict: Verdict{Stage: "first"}}, {Local: refs[1], Verdict: Verdict{Stage: "second"}}}
			callctx := ctx
			switch fault {
			case "account":
				batch[1].Local.AccountID = "other-account"
			case "missing":
				batch[1].Local.N = 99999
			case "retired":
				if _, err := a.store.db.ExecContext(ctx, "update messages set identity_valid=0 where n=?", refs[1].N); err != nil {
					t.Fatal(err)
				}
			case "n":
				batch[1].Verdict.N = refs[0].N
			case "json":
				batch[1].Verdict.Scores = map[string]float64{"score": math.NaN()}
			case "sql":
				if _, err := a.store.db.ExecContext(ctx, "create trigger invented_verdict_failure before insert on verdicts when new.stage='second' begin select raise(abort,'invented failure'); end"); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				var cancel context.CancelFunc
				callctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err := a.AddVerdictsBatch(callctx, batch)
			if err == nil {
				t.Fatal("fault accepted")
			}
			if fault == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			vs, err := a.Verdicts(ctx, refs[0])
			if err != nil || len(vs) != 0 {
				t.Fatalf("partial commit: %+v %v", vs, err)
			}
		})
	}
	t.Run("same-message semantics and empty validation", func(t *testing.T) {
		a, refs := verdictAccount(t)
		if err := a.RemoveMembership(ctx, refs[0], mailbox.CollectionRef{AccountID: a.Account().ID, CollectionID: "inbox"}); err != nil {
			t.Fatal(err)
		}
		if err := a.AddVerdictsBatch(ctx, []BoundVerdict{{Local: refs[0], Verdict: Verdict{Stage: "detached"}}}); err != nil {
			t.Fatal(err)
		}
		if err := a.AddVerdictsBatch(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		if err := a.AddVerdictsBatch(ctx, nil); err == nil {
			t.Fatal("closed empty batch accepted")
		}
	})
}

func TestAccountVerdictsBatchConcurrentSessions(t *testing.T) {
	a, refs := verdictAccount(t)
	ctx := context.Background()
	b, err := OpenAccount(ctx, a.store.life.entry.path, a.Account())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	start := make(chan struct{})
	results := make(chan error, 2)
	for index, s := range []*AccountStore{a, b} {
		go func(index int, s *AccountStore) {
			<-start
			results <- s.AddVerdictsBatch(ctx, []BoundVerdict{{Local: refs[0], Verdict: Verdict{Stage: fmt.Sprintf("batch-%d-first", index)}}, {Local: refs[1], Verdict: Verdict{Stage: fmt.Sprintf("batch-%d-last", index)}}})
		}(index, s)
	}
	close(start)
	succeeded := 0
	for range 2 {
		err := <-results
		if err == nil {
			succeeded++
		} else {
			var driverErr interface {
				error
				Code() int
			}
			if !errors.As(err, &driverErr) || (driverErr.Code()&255 != 5 && driverErr.Code()&255 != 6) {
				t.Fatalf("unexpected contention error: %v", err)
			}
		}
	}
	// SQLite may conservatively refuse a contending transaction. Every successful
	// batch is whole; no failed batch may leave only its first message's row.
	first, err := a.Verdicts(ctx, refs[0])
	if err != nil {
		t.Fatal(err)
	}
	last, err := a.Verdicts(ctx, refs[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != succeeded || len(last) != succeeded || succeeded == 0 {
		t.Fatalf("partial concurrent batch: %d %+v %+v", succeeded, first, last)
	}
}
