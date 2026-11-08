package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type selectedVerifierProvider struct {
	*reconciliationVerifierProvider
	diagnostic                graphmail.ReconciliationSenderAddressDiagnostic
	err                       error
	observations, resolutions int
}

func (p *selectedVerifierProvider) ObserveLegacySenderAddress(context.Context, []graphmail.LegacyReference, int64) (graphmail.ReconciliationSenderAddressDiagnostic, error) {
	p.observations++
	return p.diagnostic, p.err
}
func (p *selectedVerifierProvider) Resolve(ctx context.Context, s string) (mailbox.Collection, error) {
	p.resolutions++
	return p.reconciliationVerifierProvider.Resolve(ctx, s)
}

func TestSelectedVerifierHasNoProofCapability(t *testing.T) {
	ctx := context.Background()
	a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
	path := filepath.Join(t.TempDir(), "history.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Upsert(ctx, "inbox", store.Record{GraphID: "target", InternetMessageID: "<target@example.org>", Sender: "sender@example.org"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	prep, err := store.PrepareLegacyBinding(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"equal", "different", "account", "number", "id", "mode", "missing-origin", "origin-mismatch", "read-error", "unsupported", "unknown", "removed", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			before := *diagnosticFixture()
			before.RequestedOriginal, before.AccountID, before.N, before.ImmutableID = true, a.ID, prep.Live[0].N, "target"
			before.InspectedSenderAddress = before.EnumeratedSenderAddress
			before.InspectedOrigin = diagnosticOrigin(before.InspectedSenderAddress)
			p := &selectedVerifierProvider{reconciliationVerifierProvider: &reconciliationVerifierProvider{account: a}, diagnostic: before}
			var provider mailbox.Provider = p
			selected := before.N
			localPrep := prep
			cause := errors.New("hostile-value\x1b\nhttps://host.example.org/private")
			switch kind {
			case "different":
				p.diagnostic.InspectedSenderAddress = "different@example.org"
				p.diagnostic.InspectedOrigin = diagnosticOrigin(p.diagnostic.InspectedSenderAddress)
			case "account":
				p.diagnostic.AccountID = "wrong"
			case "number":
				p.diagnostic.N++
			case "id":
				p.diagnostic.ImmutableID = "wrong"
			case "mode":
				p.diagnostic.RequestedOriginal = false
			case "missing-origin":
				p.diagnostic.EnumeratedOrigin = nil
			case "origin-mismatch":
				p.diagnostic.EnumeratedOrigin = diagnosticOrigin("wrong@example.org")
			case "read-error":
				p.err = cause
			case "unsupported":
				provider = p.reconciliationVerifierProvider
			case "unknown":
				selected++
			case "removed":
				localPrep.Live = nil
			case "duplicate":
				localPrep.Live = append(append([]store.Message{}, prep.Live...), prep.Live[0])
			}
			v := &reconciliationVerifier{provider: provider, senderDiagnostics: true, selectedOriginal: selected, report: store.LegacyReconciliationReceipt{Version: 5}}
			proof, err := v.ReconcileLegacy(ctx, a, localPrep)
			if err == nil || !reflect.DeepEqual(proof, store.LegacyReconciliation{}) || !reflect.DeepEqual(v.report, store.LegacyReconciliationReceipt{}) || p.collectorCalls != 0 {
				t.Fatal("selected path acquired proposal/review capability")
			}
			var failure *graphmail.ReconciliationError
			if !errors.As(err, &failure) {
				t.Fatal("missing safe diagnostic error")
			}
			if (kind == "equal" || kind == "different") != (failure.SenderAddressDiagnostic != nil) {
				t.Fatal("unverified capability/association reached writer")
			}
			if kind == "read-error" && !errors.Is(err, cause) {
				t.Fatal("read cause lost")
			}
			if strings.Contains(err.Error(), "hostile") {
				t.Fatal("untrusted cause rendered")
			}
			if kind == "unknown" || kind == "removed" || kind == "duplicate" || kind == "unsupported" {
				if p.observations != 0 || p.resolutions != 0 {
					t.Fatal("preflight had mailbox effects")
				}
			}
		})
	}
}

func TestSelectedDiagnosticFlagPreflight(t *testing.T) {
	for _, value := range []string{"", "0", "-1", "+1", "01", " 1", "1 ", "9223372036854775808", "hostile\x1b\nhttps://host.example.org/private"} {
		t.Run("invalid-"+strings.ReplaceAll(value, "/", "_"), func(t *testing.T) { runSelectedPreflight(t, value, nil) })
	}
	for _, flag := range []string{"missing-output", "--execute", "--execute=false", "--confirm-account=", "--confirm-history=", "--confirm-reconciliation="} {
		t.Run(flag, func(t *testing.T) { runSelectedPreflight(t, "1", []string{flag}) })
	}
}
func runSelectedPreflight(t *testing.T, n string, extra []string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	contextDir := t.TempDir()
	t.Setenv("MAILPIPE_CONFIG", filepath.Join(contextDir, "config.json"))
	args := []string{"--db", filepath.Join(contextDir, "history.db"), "account", "reconcile", "--sender-address-diagnostic-n", n}
	if len(extra) == 0 || extra[0] != "missing-output" {
		args = append(args, "--sender-address-diagnostic-out", filepath.Join(dir, "selected.json"))
		args = append(args, extra...)
	}
	calls := 0
	root := NewRootCommand(Deps{Auth: func() (Authenticator, error) { calls++; return nil, errors.New("unexpected auth") }, Store: func(string) (*store.Store, error) { calls++; return nil, errors.New("unexpected store") }})
	var out, stderr bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&stderr)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	if ExitCode(err) != 2 || calls != 0 || out.Len() != 0 {
		t.Fatalf("flag preflight effects: code=%d calls=%d", ExitCode(err), calls)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("preflight created output")
	}
	for _, s := range []string{dir, "hostile", "host.example.org"} {
		if strings.Contains(stderr.String(), s) {
			t.Fatal("untrusted selection/path leaked")
		}
	}
}

func TestSelectedDiagnosticFixedScopeAndFailure(t *testing.T) {
	for _, requested := range []bool{false, true} {
		d := diagnosticFixture()
		d.RequestedOriginal = requested
		raw, err := encodeSenderDiagnostic(d)
		if err != nil {
			t.Fatal(err)
		}
		var doc senderDiagnosticDocument
		if err = json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Format != 3 || doc.HistoricalValidated || doc.ReconciliationComplete || doc.AdmissionEvaluated == requested || doc.DescribesFirstFailure == requested {
			t.Fatal("scope truth changed")
		}
		want := "first_failure_sender_disagreement"
		if requested {
			want = "requested_original_observation"
		}
		if doc.Scope != want {
			t.Fatal("untrusted scope")
		}
	}
	for _, kind := range []string{"saved", "short-stderr", "writer-failure", "canceled", "oversized", "hostile"} {
		t.Run(kind, func(t *testing.T) {
			d := diagnosticFixture()
			d.RequestedOriginal = true
			output, path := privateDiagnostic(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var out, stderr bytes.Buffer
			var errorOut interface{ Write([]byte) (int, error) } = &stderr
			switch kind {
			case "short-stderr":
				errorOut = reconciliationFailWriter{}
			case "writer-failure":
				output.readBack = func(*os.File) ([]byte, error) { return nil, errors.New("hostile-writer-secret") }
			case "canceled":
				cancel()
			case "oversized":
				d.InspectedSenderAddress = strings.Repeat("x", senderDiagnosticLimit+1)
				d.InspectedOrigin = diagnosticOrigin(d.InspectedSenderAddress)
			case "hostile":
				d.InspectedSenderAddress = "secret\x1b\nhttps://host.example.org/private"
				d.InspectedOrigin = diagnosticOrigin(d.InspectedSenderAddress)
			}
			a := agentio.New("account reconcile", agentio.WithStdout(&out), agentio.WithStderr(errorOut), agentio.WithOverflowDir(t.TempDir()))
			cause := &graphmail.ReconciliationError{Reason: "selected-original diagnostic only", SenderAddressDiagnostic: d}
			result := output.failure(ctx, a, cause)
			a.Fail(result)
			if agentio.ExitCode(result) != 1 || out.Len() != 0 {
				t.Fatal("selected diagnostic returned success/output")
			}
			for _, s := range []string{"secret", "host.example.org", path, "--confirm-reconciliation", "retry a reconciliation"} {
				if strings.Contains(stderr.String(), s) {
					t.Fatal("selected output leaked or granted retry authority")
				}
			}
			if kind == "writer-failure" || kind == "canceled" || kind == "oversized" {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal("failed selected output survived")
				}
			}
		})
	}
}
