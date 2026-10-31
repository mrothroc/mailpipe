package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/apply"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type pausedPlanLog struct {
	s                *store.Store
	reached, release chan struct{}
}

func (l pausedPlanLog) Moved(ctx context.Context, id int64) (map[int64]bool, error) {
	done, e := l.s.Moved(ctx, id)
	close(l.reached)
	select {
	case <-l.release:
		return done, e
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (l pausedPlanLog) LogActions(ctx context.Context, a []store.Action) error {
	return l.s.LogActions(ctx, a)
}

type inventedMailbox struct{}

func (inventedMailbox) FolderID(_ context.Context, name string) (string, error) {
	return "opaque-" + name, nil
}
func (inventedMailbox) Lookup(_ context.Context, ids []string) ([]apply.Found, error) {
	out := make([]apply.Found, len(ids))
	for i := range out {
		out[i].Remote = apply.Remote{InternetMessageID: "<fixture@example.org>", Sender: "sender@example.org", ParentFolderID: "opaque-inbox"}
	}
	return out, nil
}
func (inventedMailbox) Move(_ context.Context, ids []string, _ string) ([]error, error) {
	return make([]error, len(ids)), nil
}
func TestLifecycleExcludesApplyAfterMoved(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "mail.db")
	s, e := store.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.Upsert(ctx, "inbox", store.Record{GraphID: "fixture-graph", InternetMessageID: "<fixture@example.org>", Sender: "sender@example.org"}); e != nil {
		t.Fatal(e)
	}
	messages, e := s.Messages(ctx, store.Filter{})
	if e != nil {
		t.Fatal(e)
	}
	id, e := s.CreatePlan(ctx, "inbox", []store.PlanItem{{N: messages[0].N, Action: "delete"}})
	if e != nil {
		t.Fatal(e)
	}
	_, items, e := s.Plan(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	log := pausedPlanLog{s: s, reached: make(chan struct{}), release: make(chan struct{})}
	finished := make(chan error, 1)
	go func() {
		_, e := apply.Run(ctx, log, inventedMailbox{}, id, items, apply.Options{Action: "delete", Destination: "deleteditems"}, func(apply.Outcome) error { return nil })
		finished <- e
	}()
	select {
	case <-log.reached:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, e = store.PrepareLegacyBinding(ctx, path); !errors.Is(e, store.ErrLifecycleBusy) {
		close(log.release)
		t.Fatalf("binding admitted after Moved before provider work: %v", e)
	}
	close(log.release)
	if e = <-finished; e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = store.PrepareLegacyBinding(ctx, path); e != nil {
		t.Fatalf("maintenance refused after session close: %v", e)
	}
}
