package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func initReceipt(t *testing.T) (string, string) {
	t.Helper()
	root := temp(t)
	candidate := strings.Repeat("a", 40)
	b, e := runTool(t, "receipt-init", "--private-root", root, "--candidate", candidate)
	if e != nil {
		t.Fatalf("init %v %s", e, b)
	}
	return root, candidate
}
func TestAcceptancePublicSummaryPrivacy(t *testing.T) {
	root, c := initReceipt(t)
	p := filepath.Join(root, "receipt.json")
	raw, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	var receipt map[string]any
	json.Unmarshal(raw, &receipt)
	receipt["aggregate_counts"] = map[string]any{"initial": 137, "repeat": 90210, "incremental": 271}
	raw, _ = json.Marshal(receipt)
	os.WriteFile(p, raw, 0600)
	b, e := runTool(t, "summary-proposal", "--private-root", root, "--candidate", c)
	if e != nil {
		t.Fatalf("summary %v %s", e, b)
	}
	if len(b) != 0 {
		t.Fatal("private proposal emitted stdout")
	}
	out, _ := os.ReadFile(filepath.Join(root, "summary-proposal.json"))
	for _, canary := range []string{"137", "90210", "271", "consent_reference", "aggregate_counts", "private_root"} {
		if bytes.Contains(out, []byte(canary)) {
			t.Fatal("private projection leak")
		}
	}
	var public map[string]any
	json.Unmarshal(out, &public)
	if len(public) != 6 || public["candidate_commit"] != c || public["pilot"] != "pending" {
		t.Fatalf("unexpected public schema %s", out)
	}
	for _, key := range []string{"candidate_commit", "provider", "platform", "consent_reference", "checks", "aggregate_counts", "stage1_technical", "unknown"} {
		for _, value := range []any{"body login endpoint token folder policy classifier alex@example.org", 137, map[string]any{"nested": "canary@example.net"}} {
			t.Run(key+"-injection", func(t *testing.T) {
				bad := map[string]any{}
				json.Unmarshal(raw, &bad)
				bad[key] = value
				r, _ := json.Marshal(bad)
				os.WriteFile(p, r, 0600)
				os.Remove(filepath.Join(root, "summary-proposal.json"))
				b, e := runTool(t, "summary-proposal", "--private-root", root, "--candidate", c)
				if e == nil {
					t.Fatal("private value admitted")
				}
				if bytes.Contains(b, []byte("example")) || bytes.Contains(b, []byte("137")) {
					t.Fatal("error echoed private input")
				}
			})
		}
	}
}
func TestReceiptStrictSchema(t *testing.T) {
	root, c := initReceipt(t)
	p := filepath.Join(root, "receipt.json")
	raw, _ := os.ReadFile(p)
	for _, bad := range [][]byte{append([]byte(`{"version":1,`), raw[1:]...), []byte(strings.Repeat("x", 65537)), bytes.Replace(raw, []byte(`"pending"`), []byte(`"pass with token@example.org"`), 1)} {
		os.WriteFile(p, bad, 0600)
		if b, e := runTool(t, "receipt-validate", "--private-root", root, "--candidate", c); e == nil || bytes.Contains(b, []byte("example.org")) {
			t.Fatalf("invalid receipt accepted/leaked: %v %s", e, b)
		}
	}
}

func TestOwnerReviewedProposalRequiresCompletePrivateConsent(t *testing.T) {
	root, c := initReceipt(t)
	if _, e := runTool(t, "summary-proposal", "--private-root", root, "--candidate", c, "--owner-reviewed"); e == nil {
		t.Fatal("pending pilot approved")
	}
	r := receipt{Version: 1, Candidate: c, Provider: "yahoo", Platform: "macos", Consent: strings.Repeat("b", 32), Checks: checks{"pass", "pass", "pass", "pass", "pass", "pass", "pass", "pass", "pass", "pass", "pass", "pass", "pass", "pass"}, Counts: aggregates{137, 90210, 271}, Technical: technical{true, true, true, true, true, true, true}}
	b, _ := json.Marshal(r)
	os.WriteFile(filepath.Join(root, "receipt.json"), b, 0600)
	if _, e := runTool(t, "summary-proposal", "--private-root", root, "--candidate", c, "--owner-reviewed"); e == nil {
		t.Fatal("missing actual consent scope accepted")
	}
	s := consent{1, strings.Repeat("b", 32), true, true, true, true, true, "2026-01-01"}
	b, _ = json.Marshal(s)
	os.WriteFile(filepath.Join(root, "consent.json"), b, 0600)
	if b, e := runTool(t, "summary-proposal", "--private-root", root, "--candidate", c, "--owner-reviewed"); e != nil {
		t.Fatalf("invented complete positive control refused %v %s", e, b)
	}
	b, _ = os.ReadFile(filepath.Join(root, "summary-proposal.json"))
	if !bytes.Contains(b, []byte(`"privately_verified"`)) {
		t.Fatal("missing fixed result")
	}
	for _, value := range []string{"137", "90210", "271", strings.Repeat("b", 32), "2026-01-01"} {
		if bytes.Contains(b, []byte(value)) {
			t.Fatal("private value leaked")
		}
	}
}

func TestReceiptInitializationFailurePreservesArtifacts(t *testing.T) {
	for _, kind := range []string{"own-pending", "replacement", "symlink", "same-inode-edit"} {
		t.Run(kind, func(t *testing.T) {
			root := temp(t)
			existing := []byte("invented existing consent example.org\n")
			os.WriteFile(filepath.Join(root, "consent.json"), existing, 0600)
			replacement := []byte("invented replacement example.net\n")
			outside := filepath.Join(temp(t), "outside")
			os.WriteFile(outside, replacement, 0600)
			calls := 0
			old := privateTestHook
			t.Cleanup(func() { privateTestHook = old })
			privateTestHook = func() {
				calls++
				if calls != 2 {
					return
				}
				switch kind {
				case "replacement":
					tmp := filepath.Join(root, "replacement")
					os.WriteFile(tmp, replacement, 0600)
					if e := os.Rename(tmp, filepath.Join(root, "receipt.json")); e != nil {
						t.Fatal(e)
					}
				case "symlink":
					os.Remove(filepath.Join(root, "receipt.json"))
					if e := os.Symlink(outside, filepath.Join(root, "receipt.json")); e != nil {
						t.Fatal(e)
					}
				case "same-inode-edit":
					os.WriteFile(filepath.Join(root, "receipt.json"), replacement, 0600)
				}
			}
			e := receiptCommand("receipt-init", root, strings.Repeat("a", 40), false)
			if e == nil || !errors.Is(e, errInvalid) {
				t.Fatal("original creation failure lost")
			}
			actual, e := os.ReadFile(filepath.Join(root, "receipt.json"))
			if e != nil {
				t.Fatal("partial/replacement artifact deleted")
			}
			if kind != "own-pending" && !bytes.Equal(actual, replacement) {
				t.Fatal("replacement changed")
			}
			if b, _ := os.ReadFile(filepath.Join(root, "consent.json")); !bytes.Equal(b, existing) {
				t.Fatal("existing consent changed")
			}
			if b, _ := os.ReadFile(outside); !bytes.Equal(b, replacement) {
				t.Fatal("outside file changed")
			}
		})
	}
}
