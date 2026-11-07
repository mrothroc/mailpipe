package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type reconciliationFailWriter struct{ err error }

func (w reconciliationFailWriter) Write(p []byte) (int, error) {
	if w.err == nil {
		return len(p) - 1, nil
	}
	return 0, w.err
}

func TestReconciliationFailurePreservesCausesWithoutRenderingThem(t *testing.T) {
	NewRootCommand(Deps{})
	for _, short := range []bool{false, true} {
		t.Run(fmt.Sprint(short), func(t *testing.T) {
			providerCause := errors.New("untrusted-provider-secret\n\x1b")
			outputCause := errors.New("unregistered-writer-secret\n\x1b")
			writer := reconciliationFailWriter{err: outputCause}
			if short {
				writer.err = nil
				outputCause = io.ErrShortWrite
			}
			original := &graphmail.ReconciliationError{Structural: true, Reason: "identity conflict requires owner review", Cause: providerCause,
				Conflict: &graphmail.ReconciliationConflict{Originals: []graphmail.ReconciliationConflictOriginal{{N: 1}}, MatchingOriginals: 1}}
			var stderr bytes.Buffer
			a := agentio.New("account reconcile", agentio.WithJSON(true), agentio.WithStdout(writer), agentio.WithStderr(&stderr), agentio.WithOverflowDir(t.TempDir()))
			result := reconciliationFailureOutput(a, fmt.Errorf("private outer context: %w", original))
			var recovered *graphmail.ReconciliationError
			var coded *agentio.CodedError
			if agentio.ExitCode(result) != agentio.ExitStale || !errors.Is(result, providerCause) || !errors.Is(result, outputCause) || !errors.As(result, &recovered) || recovered != original || !errors.As(result, &coded) {
				t.Fatalf("failure identity lost: %v", result)
			}
			a.Fail(result)
			if !strings.Contains(stderr.String(), "diagnostic output incomplete") || strings.Contains(stderr.String(), "secret") || strings.Contains(stderr.String(), "private outer") {
				t.Fatalf("unsafe failure rendering: %s", stderr.String())
			}
		})
	}
	var out, stderr bytes.Buffer
	cause := &graphmail.ReconciliationError{Reason: "incomplete primary-mailbox evidence", Cause: context.Canceled}
	a := agentio.New("account reconcile", agentio.WithStdout(&out), agentio.WithStderr(&stderr), agentio.WithOverflowDir(t.TempDir()))
	result := reconciliationFailureOutput(a, cause)
	if agentio.ExitCode(result) != agentio.ExitRuntime || !errors.Is(result, context.Canceled) || out.Len() != 0 {
		t.Fatalf("ordinary incomplete failure changed: %v %s", result, out.String())
	}
}

type reconciliationVerifierProvider struct {
	mailbox.Provider
	account                  mailbox.Account
	evidence                 graphmail.LegacyEvidence
	resolveError, collectErr error
	collectorCalls           int
}

func (p *reconciliationVerifierProvider) Account() mailbox.Account { return p.account }
func (p *reconciliationVerifierProvider) Resolve(context.Context, string) (mailbox.Collection, error) {
	return mailbox.Collection{Ref: mailbox.CollectionRef{AccountID: p.account.ID, CollectionID: "inbox"}}, p.resolveError
}
func (p *reconciliationVerifierProvider) ReconcileLegacyReferences(context.Context, []graphmail.LegacyReference) (graphmail.LegacyEvidence, error) {
	p.collectorCalls++
	return p.evidence, p.collectErr
}

type unsupportedReconciliationProvider struct{ mailbox.Provider }

// The embedded base supplies Account, but this method set has no collector.
func TestReconciliationVerifierFailuresReturnZeroProofAndReport(t *testing.T) {
	ctx := context.Background()
	a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
	path := filepath.Join(t.TempDir(), "history.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Upsert(ctx, "inbox", store.Record{GraphID: "original", InternetMessageID: "<original@example.org>", Sender: "sender@example.org"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	prep, err := store.PrepareLegacyBinding(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"account", "unsupported", "resolve-404", "resolve-read", "collector", "diagnostic-unsupported", "review"} {
		t.Run(kind, func(t *testing.T) {
			cause := errors.New("invented-read-failure")
			p := &reconciliationVerifierProvider{account: a}
			var provider mailbox.Provider = p
			switch kind {
			case "account":
				p.account.ID = "different-account"
			case "unsupported":
				provider = unsupportedReconciliationProvider{Provider: p}
			case "resolve-404":
				p.resolveError = &graph.APIError{Status: 404, Code: "ErrorItemNotFound"}
			case "resolve-read":
				p.resolveError = cause
			case "collector":
				p.collectErr = &graphmail.ReconciliationError{Reason: "incomplete evidence", Cause: cause}
			}
			v := &reconciliationVerifier{provider: provider, senderDiagnostics: kind == "diagnostic-unsupported", report: store.LegacyReconciliationReceipt{Digest: "previous-proposal"}}
			proof, err := v.ReconcileLegacy(ctx, a, prep)
			if err == nil || !reflect.DeepEqual(proof, store.LegacyReconciliation{}) || !reflect.DeepEqual(v.report, store.LegacyReconciliationReceipt{}) {
				t.Fatalf("failed verifier leaked proof/report: %+v %+v %v", proof, v.report, err)
			}
			if (kind == "resolve-read" || kind == "collector") && !errors.Is(err, cause) {
				t.Fatal("read cause lost")
			}
			if kind != "collector" && kind != "review" && p.collectorCalls != 0 {
				t.Fatal("invalid precondition reached collector")
			}
		})
	}
}

func TestReconciliationVerifierCannotReuseSuccessfulReportAfterFailure(t *testing.T) {
	ctx := context.Background()
	a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
	path := filepath.Join(t.TempDir(), "empty.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	prep, err := store.PrepareLegacyBinding(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	p := &reconciliationVerifierProvider{account: a, evidence: graphmail.LegacyEvidence{Started: now, Finished: now, Pages: 1}}
	v := &reconciliationVerifier{provider: p}
	if _, err = v.ReconcileLegacy(ctx, a, prep); err != nil || v.report.Digest == "" {
		t.Fatalf("positive report control failed: %+v %v", v.report, err)
	}
	p.collectErr = context.Canceled
	proof, err := v.ReconcileLegacy(ctx, a, prep)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(proof, store.LegacyReconciliation{}) || !reflect.DeepEqual(v.report, store.LegacyReconciliationReceipt{}) {
		t.Fatalf("old proposal escaped after failure: %+v %+v %v", proof, v.report, err)
	}
}

func TestReconciliationMembershipOutputRedactsNestedPrimitives(t *testing.T) {
	NewRootCommand(Deps{})
	secret := "invented-nested-secret"
	agentio.RegisterSecret(secret)
	for _, jsonMode := range []bool{false, true} {
		t.Run(fmt.Sprint(jsonMode), func(t *testing.T) {
			var out, stderr bytes.Buffer
			dir := t.TempDir()
			agent := agentio.New("account reconcile", agentio.WithJSON(jsonMode), agentio.WithStdout(&out), agentio.WithStderr(&stderr), agentio.WithOverflowDir(dir), agentio.WithLimit(1))
			account := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
			old := mailbox.Membership{Collection: mailbox.CollectionRef{AccountID: account.ID, CollectionID: mailbox.CollectionID("old-" + secret)}}
			current := mailbox.Membership{Collection: mailbox.CollectionRef{AccountID: account.ID, CollectionID: mailbox.CollectionID("current-" + secret)}}
			state := mailbox.State{Ref: mailbox.MessageRef{AccountID: account.ID, MessageID: mailbox.MessageID("id-" + secret), Locator: mailbox.Locator("id-" + secret)}, Memberships: []mailbox.Membership{current}, Revision: secret, Metadata: mailbox.Metadata{Subject: secret, Sender: mailbox.Address{Address: "sender@example.org\u202e"}}}
			r := store.LegacyReconciliationReceipt{Version: 2, Account: account, Items: []store.LegacyReconciliationItem{{N: 9007199254740993, OriginalID: "id-" + secret, OriginalFolder: secret, OriginalMemberships: []mailbox.Membership{old}, MembershipChanged: true, Disposition: "verified", State: &state}}}
			if err := reconciliationOutput(agent, "review", r); err != nil {
				t.Fatal(err)
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 {
				t.Fatalf("missing private overflow: %v %v", files, err)
			}
			path := filepath.Join(dir, files[0].Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{out.String(), stderr.String(), string(raw)} {
				if strings.Contains(value, secret) {
					t.Fatal("nested typed state bypassed redaction")
				}
			}
			if strings.Contains(out.String(), "\u202e") || strings.Contains(stderr.String(), "\u202e") {
				t.Fatal("terminal control rendered")
			}
			if !strings.Contains(string(raw), "9007199254740993") {
				t.Fatal("local number lost precision during primitive conversion")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("overflow is not private", err)
			}
			if jsonMode {
				lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
				var row map[string]any
				if err = json.Unmarshal([]byte(lines[1]), &row); err != nil {
					t.Fatal(err)
				}
				if row["membership_changed"] != true || row["state"] == nil || row["original_memberships"] == nil {
					t.Fatal("redaction omitted required review evidence")
				}
			}
		})
	}
}
