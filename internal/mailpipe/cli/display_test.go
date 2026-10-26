package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/store"
)

// pacific pins the local zone, so --local-time output is the same everywhere.
func pacific(t *testing.T) {
	t.Helper()
	old := time.Local
	time.Local = time.FixedZone("PDT", -7*3600)
	t.Cleanup(func() { time.Local = old })
}

func TestLocalTimeShowsReceivedTimesInTheLocalZone(t *testing.T) {
	pacific(t)
	e := digestEnv(t)
	const utc, local = "2026-10-20T10:00:00Z", "2026-10-20T03:00:00-07:00"
	checks := []struct {
		args []string
		want string
	}{
		{[]string{"ls", "--from", "campaign", "--local-time"}, "\t" + local + "\t"},
		{[]string{"digest", "--from", "campaign", "--local-time"}, "\t" + local + "\t"},
		{[]string{"digest", "--from", "campaign", "--local-time", "--json"}, `"received":"` + local + `"`},
		{[]string{"show", "1", "--local-time"}, "received: " + local + "\n"},
		{[]string{"show", "1", "--local-time", "--json"}, `"received":"` + local + `"`},
		{[]string{"ls", "--from", "campaign"}, "\t" + utc + "\t"}, // UTC stays the default
	}
	for _, c := range checks {
		code, out, errOut := e.run(c.args...)
		if code != 0 || !strings.Contains(out, c.want) {
			t.Fatalf("%v: code %d out %q err %q, want %q", c.args, code, out, errOut, c.want)
		}
	}
}

func TestLocalTimeLeavesAMissingTimeBlank(t *testing.T) {
	pacific(t)
	e := newEnv(t)
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(context.Background(), "inbox", store.Record{GraphID: "undated", Sender: "a@example.org"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	code, out, _ := e.run("ls", "--local-time")
	if code != 0 || out != "n\treceived\tfolder\tsender\tsubject\n1\t\tinbox\ta@example.org\t\n" {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestListingsSayNIsNotInTimeOrder(t *testing.T) {
	for _, cmd := range []string{"ls", "digest"} {
		_, help, _ := newEnv(t).run(cmd, "--help")
		if !strings.Contains(help, "n numbers messages in the order they were first synced, not by time") {
			t.Fatalf("%s help: %s", cmd, help)
		}
	}
}
