package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyRemovedIdentityProjection(t *testing.T) {
	ctx := context.Background()
	s, e := Open(filepath.Join(t.TempDir(), "history.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	body := strings.Repeat("invented-body-canary", 1<<15)
	subject := strings.Repeat("invented-metadata-canary", 1<<15)
	for i, id := range []string{"removed-one", "removed-two"} {
		_, e := s.Upsert(ctx, "inbox", Record{GraphID: id, InternetMessageID: "<" + id + "@example.org>", Sender: "sender@example.org", Subject: subject})
		if e != nil {
			t.Fatal(e)
		}
		if e = s.SetBody(ctx, int64(i+1), body); e != nil {
			t.Fatal(e)
		}
		if _, e = s.Remove(ctx, "inbox", id); e != nil {
			t.Fatal(e)
		}
	}
	if e = s.db.Close(); e != nil {
		t.Fatal(e)
	}
	capture := &inventoryCapture{}
	name := "invented-removed-projection-" + randomID()
	sql.Register(name, inventoryDriver{capture})
	db, e := sql.Open(name, sqliteURI(s.life.entry.path, "rw", false))
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	s.db = db
	identities, e := legacyRemoved(ctx, db)
	if e != nil {
		t.Fatal(e)
	}
	if len(identities) != 2 || identities[0].N != 1 || identities[0].GraphID != "removed-one" || identities[1].N != 2 || identities[1].GraphID != "removed-two" {
		t.Fatalf("removed identities lost: %+v", identities)
	}
	// Inspect values from the pinned driver before Scan can discard anything.
	// Run the full read even on a wide projection, so a red run also proves the
	// observer sees content and that stored values were not changed by the read.
	projectedColumns := append([]string(nil), capture.columns...)
	projectedBytes, projectedBody := capture.bytes, capture.body
	m, found, e := s.Get(ctx, 1)
	if e != nil || !found || m.Body == nil || *m.Body != body || m.Subject != subject || !capture.body || capture.maxValue < len(body) {
		t.Fatalf("full-content positive control failed: %v", e)
	}
	if !reflect.DeepEqual(projectedColumns, []string{"n", "graph_id", "n", "graph_id"}) || projectedBody || projectedBytes != len("removed-one")+len("removed-two") {
		t.Fatalf("removed receipt projection fetched payload: columns=%v bytes=%d body=%v (full-content positive control passed)", projectedColumns, projectedBytes, projectedBody)
	}
}
