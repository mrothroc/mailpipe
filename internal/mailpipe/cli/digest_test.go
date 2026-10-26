package cli_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/graph"
)

// digestEnv syncs two Inbox messages with previews and read flags, judges
// them with the allowlist, and imports a delete verdict for the campaign one.
func digestEnv(t *testing.T) *env {
	t.Helper()
	return digestEnvAt(t, "2026-10-20T10:00:00Z")
}

// digestEnvAt is digestEnv with the campaign message received at campaignAt
// (the friend's message is received at 2026-10-21T09:30:00Z).
func digestEnvAt(t *testing.T, campaignAt string) *env {
	t.Helper()
	e := newEnv(t)
	with := func(raw json.RawMessage, extra map[string]any) json.RawMessage {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		for k, v := range extra {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return b
	}
	e.graph.pages["/me/mailFolders/inbox/messages/delta"] = []graph.Page{{Value: []json.RawMessage{
		with(msg("g1", "info@campaign.example.org", "Chip in", campaignAt),
			map[string]any{"isRead": false, "bodyPreview": "Give͏ ‌ today!\r\n\r\nPlease­"}),
		with(msg("g2", "friend@example.org", "Dinner\tTuesday?", "2026-10-21T09:30:00Z"),
			map[string]any{"isRead": true, "hasAttachments": true, "bodyPreview": strings.Repeat("é", 150)}),
	}, DeltaLink: "d"}}
	e.stagesConfigWithPolicy("http://unused", `{"rules":[{"stage":"imported","label":"appeal","action":"delete"}]}`)
	e.run("sync")
	e.run("run", "known")
	if code, _, errOut := e.run("import-verdicts", writeFile(t,
		`{"internet_message_id":"<g1@example.org>","sender":"info@campaign.example.org","stage":"imported","labels":["appeal"],"provenance":"p","at":"2026-10-21T12:00:00Z"}`+"\n")); code != 0 {
		t.Fatalf("import: %q", errOut)
	}
	return e
}

var longPreview = strings.Repeat("é", 139) + "…"

func TestDigestShowsOneLinePerMessageWithDecisionLabelsAndPreview(t *testing.T) {
	e := digestEnv(t)
	code, out, errOut := e.run("digest")
	want := "n\treceived\tsender\tsubject\tdecision\tlabels\tpreview\n" +
		"2\t2026-10-21T09:30:00Z\tfriend@example.org\tDinner Tuesday?\tkeep\tknown:known_contact\t" + longPreview + "\n" +
		"1\t2026-10-20T10:00:00Z\tinfo@campaign.example.org\tChip in\tdelete\timported:appeal\tGive today! Please\n"
	if code != 0 || out != want {
		t.Fatalf("code %d\nout  %q\nwant %q\nerr %q", code, out, want, errOut)
	}
	if !strings.Contains(errOut, "[hint] 2 messages: 1 delete, 1 keep, 0 with no policy rule. Try: mailpipe show 2\n") {
		t.Fatalf("hint %q", errOut)
	}
}

func TestDigestJSONCarriesFlagsAndReasons(t *testing.T) {
	e := digestEnv(t)
	code, out, errOut := e.run("digest", "--json", "--from", "friend")
	want := `{"attachments":true,"decision":"keep","folder":"inbox","labels":["known:known_contact"],"n":2,` +
		`"preview":"` + longPreview + `","read":true,"reasons":["known:action=keep"],"received":"2026-10-21T09:30:00Z",` +
		`"sender":"friend@example.org","sender_name":"A Sender","subject":"Dinner\tTuesday?"}` + "\n"
	if code != 0 || out != want {
		t.Fatalf("code %d\nout  %s\nwant %s\nerr %q", code, out, want, errOut)
	}
}

func TestDigestFilters(t *testing.T) {
	e := digestEnv(t)
	for name, c := range map[string]struct {
		args []string
		want string // the n of each row
	}{
		"unread": {[]string{"--unread"}, "1"},
		"since":  {[]string{"--since", "2026-10-21T00:00:00Z"}, "2"},
		"from":   {[]string{"--from", "CAMPAIGN"}, "1"},
		"folder": {[]string{"--folder", "sentitems"}, "3"},
	} {
		code, out, errOut := e.run(append([]string{"digest"}, c.args...)...)
		var ns []string
		for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
			ns = append(ns, strings.Split(line, "\t")[0])
		}
		if code != 0 || strings.Join(ns, ",") != c.want {
			t.Fatalf("%s: code %d rows %v want %s err %q", name, code, ns, c.want, errOut)
		}
	}
}

func TestDigestWithNothingToShowSaysWhatToDo(t *testing.T) {
	e := digestEnv(t)
	code, _, errOut := e.run("digest", "--since", "2027-01-01")
	if code != agentio.ExitNoData || !strings.Contains(errOut, "no messages match. Try: mailpipe digest --help") {
		t.Fatalf("code %d err %q", code, errOut)
	}
	empty := newEnv(t)
	empty.stagesConfig("http://unused")
	if code, _, errOut := empty.run("digest"); code != agentio.ExitNoData || !strings.Contains(errOut, "Try: mailpipe sync") {
		t.Fatalf("empty store: code %d err %q", code, errOut)
	}
}

func TestDigestWorksWithoutStageConfiguration(t *testing.T) {
	e := digestEnv(t)
	t.Setenv("MAILPIPE_STAGES", filepath.Join(t.TempDir(), "absent.json"))
	code, out, errOut := e.run("digest")
	if code != 0 || !strings.Contains(out, "\tChip in\t\timported:appeal\t") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "[hint] no stage configuration, so decisions are blank. Try: mailpipe stages") {
		t.Fatalf("err %q", errOut)
	}
}

func TestDigestRejectsABadSince(t *testing.T) {
	e := digestEnv(t)
	if code, _, errOut := e.run("digest", "--since", "soon"); code != agentio.ExitUsage || !strings.Contains(errOut, "Try: mailpipe digest --since 24h") {
		t.Fatalf("code %d err %q", code, errOut)
	}
}

func TestDigestHintPointsAtTheNewestMessageNotBeingRemoved(t *testing.T) {
	e := digestEnvAt(t, "2026-10-22T10:00:00Z") // the campaign message (delete) is now the newest
	_, out, errOut := e.run("digest")
	if !strings.HasPrefix(strings.Split(out, "\n")[1], "1\t") || !strings.Contains(errOut, "Try: mailpipe show 2\n") {
		t.Fatalf("out %q err %q", out, errOut)
	}
	if _, _, errOut := e.run("digest", "--unread"); !strings.Contains(errOut, "Try: mailpipe show 1\n") {
		t.Fatalf("with only deletions the hint falls back to the newest: %q", errOut)
	}
}

func TestDigestHelpExplainsTheDecisionColumn(t *testing.T) {
	_, help, _ := newEnv(t).run("digest", "--help")
	for _, want := range []string{"blank when no policy rule applies", "mailpipe verdicts <n>"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help lacks %q: %s", want, help)
		}
	}
}
