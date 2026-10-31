package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func epochFixture(t *testing.T) (*AccountStore, mailbox.Collection, mailbox.LocalRef) {
	t.Helper()
	ctx := context.Background()
	a, e := OpenAccount(ctx, inventedPath(t), fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = a.Close() })
	c := fixtureCollection("epoch")
	r := fixtureRecord("epoch-occurrence")
	r.IMAP = &IMAPLocator{CollectionID: c.Ref.CollectionID, MailboxID: "mailbox-a", UIDValidity: 10, UID: 4, EpochGeneration: 1}
	n, e := a.UpsertMessage(ctx, c, r)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.SetBody(ctx, n, "invented preserved content"); e != nil {
		t.Fatal(e)
	}
	if e = a.AddVerdict(ctx, n, Verdict{Stage: "category", Labels: []string{"record"}}); e != nil {
		t.Fatal(e)
	}
	return a, c, n
}
func epochDigest(t *testing.T, a *AccountStore) string {
	t.Helper()
	d, e := logicalDigest(context.Background(), a.store.db)
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func TestConditionalIMAPEpochReconciliation(t *testing.T) {
	ctx := context.Background()
	t.Run("stale newer publication and same target", func(t *testing.T) {
		a, c, n := epochFixture(t)
		b, e := OpenAccount(ctx, a.store.life.entry.path, fixtureAccount)
		if e != nil {
			t.Fatal(e)
		}
		defer b.Close()
		old, e := a.ReadIMAPEpochState(ctx, c.Ref)
		if e != nil {
			t.Fatal(e)
		}
		fresh, e := b.ReconcileIMAPEpoch(ctx, c.Ref, old, "mailbox-a", 11)
		if e != nil {
			t.Fatal(e)
		}
		r := fixtureRecord("epoch-new")
		r.IMAP = &IMAPLocator{CollectionID: c.Ref.CollectionID, MailboxID: "mailbox-a", UIDValidity: 11, UID: 4, EpochGeneration: fresh.EpochGeneration}
		p, e := b.BeginSync(ctx, c, mailbox.Cursor{}, &IMAPEpoch{MailboxID: "mailbox-a", UIDValidity: 11, Generation: fresh.EpochGeneration})
		if e != nil {
			t.Fatal(e)
		}
		cp := mailbox.Cursor{AccountID: fixtureAccount.ID, CollectionID: c.Ref.CollectionID, Token: "new"}
		if e = b.StagePage(ctx, p, mailbox.Cursor{}, mailbox.SyncPage{Mode: mailbox.Snapshot, Messages: []mailbox.Message{r.Message}, Complete: true, Checkpoint: cp}, []MessageRecord{r}); e != nil {
			t.Fatal(e)
		}
		if e = b.CommitSync(ctx, p); e != nil {
			t.Fatal(e)
		}
		before := epochDigest(t, a)
		for _, v := range []uint32{11, 12} {
			out, e := a.ReconcileIMAPEpoch(ctx, c.Ref, old, "mailbox-a", v)
			if !errors.Is(e, mailbox.ErrStaleIdentity) || out != (IMAPEpochState{}) {
				t.Fatalf("stale accepted: %+v %v", out, e)
			}
			if epochDigest(t, a) != before {
				t.Fatal("stale reset changed state")
			}
		}
		m, e := a.GetMessage(ctx, n)
		if e != nil || m.Valid || m.Body == nil || *m.Body != "invented preserved content" {
			t.Fatalf("history lost %+v %v", m, e)
		}
	})
	t.Run("same epoch publication and token ABA", func(t *testing.T) {
		a, c, _ := epochFixture(t)
		before, e := a.ReadIMAPEpochState(ctx, c.Ref)
		if e != nil {
			t.Fatal(e)
		}
		p, e := a.BeginSync(ctx, c, mailbox.Cursor{}, nil)
		if e != nil {
			t.Fatal(e)
		}
		if e = a.StagePage(ctx, p, mailbox.Cursor{}, mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true}, nil); e != nil {
			t.Fatal(e)
		}
		if e = a.CommitSync(ctx, p); e != nil {
			t.Fatal(e)
		}
		digest := epochDigest(t, a)
		if _, e = a.ReconcileIMAPEpoch(ctx, c.Ref, before, "mailbox-a", 11); !errors.Is(e, mailbox.ErrStaleIdentity) {
			t.Fatal(e)
		}
		if epochDigest(t, a) != digest {
			t.Fatal("changed newer publication")
		}
	})
	t.Run("two handles race", func(t *testing.T) {
		a, c, _ := epochFixture(t)
		b, e := OpenAccount(ctx, a.store.life.entry.path, fixtureAccount)
		if e != nil {
			t.Fatal(e)
		}
		defer b.Close()
		old, e := a.ReadIMAPEpochState(ctx, c.Ref)
		if e != nil {
			t.Fatal(e)
		}
		start := make(chan struct{})
		out := make(chan error, 2)
		var wg sync.WaitGroup
		for i, h := range []*AccountStore{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, e := h.ReconcileIMAPEpoch(ctx, c.Ref, old, "mailbox-a", uint32(11+i))
				out <- e
			}()
		}
		close(start)
		wg.Wait()
		close(out)
		success := 0
		for e := range out {
			if e == nil {
				success++
			}
		}
		if success != 1 {
			t.Fatalf("%d winners", success)
		}
		now, e := a.ReadIMAPEpochState(ctx, c.Ref)
		if e != nil || now.EpochGeneration != old.EpochGeneration+1 {
			t.Fatalf("%+v %v", now, e)
		}
	})
	t.Run("rollback and controls", func(t *testing.T) {
		a, c, _ := epochFixture(t)
		old, e := a.ReadIMAPEpochState(ctx, c.Ref)
		if e != nil {
			t.Fatal(e)
		}
		digest := epochDigest(t, a)
		for _, mutate := range []func(*IMAPEpochState){func(s *IMAPEpochState) { s.EpochGeneration++ }, func(s *IMAPEpochState) { s.BindingGeneration++ }, func(s *IMAPEpochState) { s.SyncPresent = true }, func(s *IMAPEpochState) { s.Account.ID = "other" }, func(s *IMAPEpochState) { s.Scope.CollectionID = "other" }} {
			bad := old
			mutate(&bad)
			if _, e = a.ReconcileIMAPEpoch(ctx, c.Ref, bad, "mailbox-a", 11); e == nil {
				t.Fatal("bad precondition accepted")
			}
			if epochDigest(t, a) != digest {
				t.Fatal("bad precondition wrote")
			}
		}
		if _, e = a.store.db.ExecContext(ctx, `create trigger invented_epoch_failure before delete on memberships begin select raise(abort,'invented rollback'); end`); e != nil {
			t.Fatal(e)
		}
		digest = epochDigest(t, a)
		if out, e := a.ReconcileIMAPEpoch(ctx, c.Ref, old, "mailbox-a", 11); e == nil || out != (IMAPEpochState{}) {
			t.Fatal("injected failure hidden")
		}
		if epochDigest(t, a) != digest {
			t.Fatal("partial retirement")
		}
		if _, e = a.store.db.ExecContext(ctx, "drop trigger invented_epoch_failure"); e != nil {
			t.Fatal(e)
		}
		if _, e = a.ReconcileIMAPEpoch(ctx, c.Ref, old, "mailbox-a", 11); e != nil {
			t.Fatal(e)
		}
	})
}

// Pause the actual production conditional helper at its first guarded write.
// WAL lets the other real handle publish while this read snapshot is retained.
type epochWriteBarrier struct {
	sqlAccess
	entered chan<- struct{}
	release <-chan struct{}
}

func (b epochWriteBarrier) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if strings.HasPrefix(q, "update collections set mailbox_id=") {
		b.entered <- struct{}{}
		select {
		case <-b.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return b.sqlAccess.ExecContext(ctx, q, args...)
}
func TestConditionalIMAPEpochStaleSnapshotUpgrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, c, _ := epochFixture(t)
	if _, e := a.store.db.ExecContext(ctx, "pragma journal_mode=WAL"); e != nil {
		t.Fatal(e)
	}
	b, e := OpenAccount(ctx, a.store.life.entry.path, fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	old, e := a.ReadIMAPEpochState(ctx, c.Ref)
	if e != nil {
		t.Fatal(e)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		var out IMAPEpochState
		done <- a.transaction(ctx, func(q sqlAccess) error {
			return a.reconcileIMAPEpoch(ctx, epochWriteBarrier{q, entered, release}, c.Ref, old, "mailbox-a", 12, &out)
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	fresh, e := b.ReconcileIMAPEpoch(ctx, c.Ref, old, "mailbox-a", 11)
	if e != nil {
		t.Fatal(e)
	}
	before := epochDigest(t, b)
	unblock()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("stale read snapshot retired newer epoch")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	after, e := b.ReadIMAPEpochState(ctx, c.Ref)
	if e != nil || after != fresh || epochDigest(t, b) != before {
		t.Fatalf("newer state changed %+v %v", after, e)
	}
}
