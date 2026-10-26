package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/store"
)

const promotionVerdict = `{"internet_message_id":"<g1@example.org>","sender":"info@campaign.example.org","stage":"category","labels":["promotion"],"provenance":"p","at":"2026-10-21T12:00:00Z"}` + "\n"

// A deletion that only means the mail expired never blocks its sender.
func TestAShelfLifeDeletionNeverBlocksTheSender(t *testing.T) {
	e := newEnv(t)
	e.stagesConfigWithPolicy("http://unused", `{"rules":[{"stage":"category","label":"promotion","action":"delete","older_than":"1d","no_block":true}]}`)
	e.run("sync")
	e.run("import-verdicts", writeFile(t, promotionVerdict))
	code, out, errOut := e.run("plan", "create")
	if code != 0 || !strings.Contains(out, "\tChip in\tdelete\tcategory:promotion>1d\n") {
		t.Fatalf("plan: code %d out %q err %q", code, out, errOut)
	}
	if _, js, _ := e.run("plan", "show", "1", "--json"); !strings.Contains(js, `"no_block":true`) {
		t.Fatalf("plan show --json lacks no_block: %q", js)
	}
	if code, _, errOut := e.run("apply", "1", "--execute"); code != 0 {
		t.Fatalf("apply: code %d err %q", code, errOut)
	}
	if code, _, errOut := e.run("rules", "senders"); code != agentio.ExitNoData {
		t.Fatalf("a shelf-life deletion reached the blocklist: code %d err %q", code, errOut)
	}
}

func TestABlockSenderIsDeletedAndBlocked(t *testing.T) {
	e := newEnv(t)
	e.stagesConfigWithPolicy("http://unused", `{"block_senders":["Info@Campaign.example.org"]}`)
	e.run("sync")
	if code, out, errOut := e.run("plan", "create"); code != 0 || !strings.Contains(out, "\tChip in\tdelete\tpolicy:block_sender\n") {
		t.Fatalf("plan: code %d out %q err %q", code, out, errOut)
	}
	e.run("apply", "1", "--execute")
	if code, out, _ := e.run("rules", "senders"); code != 0 || !strings.Contains(out, "info@campaign.example.org\t1\t0\t0\tblock\t") {
		t.Fatalf("senders: code %d out %q", code, out)
	}
}

// A sender whose wanted mail merely expired is not blocked by an earlier
// blocking deletion of their other mail.
func TestExpiredMailSparesASenderFromTheBlocklist(t *testing.T) {
	e := applied(t) // plan 1 (blocking) moved g1 from info@campaign.example.org
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Upsert(ctx, "inbox", store.Record{GraphID: "g5", Sender: "info@campaign.example.org", Subject: "Weekly news"}); err != nil {
		t.Fatal(err)
	}
	ms, _ := s.Messages(ctx, store.Filter{Folder: "inbox", SenderLike: "campaign"})
	plan, err := s.CreatePlan(ctx, "folder=inbox", []store.PlanItem{{N: ms[0].N, Action: "delete", NoBlock: true}})
	if err == nil {
		err = s.LogActions(ctx, []store.Action{{PlanID: plan, N: ms[0].N, GraphID: "g5", Action: "delete", Destination: "deleteditems", Status: store.ActionMoved}})
	}
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := e.run("rules", "senders")
	if code != 0 || !strings.Contains(out, "info@campaign.example.org\t1\t0\t1\texclude\t1 expired under a shelf life\n") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}
