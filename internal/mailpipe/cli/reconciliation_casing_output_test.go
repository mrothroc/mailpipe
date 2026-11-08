package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
)

// Renderer compatibility/privacy only: collection and attribution are exercised
// by the separate real command-tree and store tests.
func TestReconciliationCasingRendererRetainsLongPairs(t *testing.T) {
	NewRootCommand(Deps{})
	const secret = "invented-recipient-secret"
	agentio.RegisterSecret(secret)
	for _, jsonMode := range []bool{false, true} {
		t.Run(fmt.Sprint(jsonMode), func(t *testing.T) {
			var out, stderr bytes.Buffer
			dir := t.TempDir()
			agent := agentio.New("account reconcile", agentio.WithJSON(jsonMode), agentio.WithStdout(&out), agentio.WithStderr(&stderr), agentio.WithOverflowDir(dir), agentio.WithLimit(1))
			current, enumerated := mailbox.State{Metadata: mailbox.Metadata{Sender: mailbox.Address{Address: "SENDER@Example.org"}}}, mailbox.State{Metadata: mailbox.Metadata{Sender: mailbox.Address{Address: "sender@example.org"}}}
			current.Metadata.Sender.Name = "Current-" + strings.Repeat("名", 4096) + secret + "\x1b\u202e"
			enumerated.Metadata.Sender.Name = "Original-" + strings.Repeat("名", 4096) + secret + "\x1b\u202e"
			for i := 0; i < 256; i++ {
				current.Metadata.Recipients = append(current.Metadata.Recipients, mailbox.Address{Address: fmt.Sprintf("reader%d@example.org", i), Name: secret + "\x1b\u202e"})
				enumerated.Metadata.Recipients = append(enumerated.Metadata.Recipients, mailbox.Address{Address: fmt.Sprintf("Reader%d@Example.org", i), Name: secret + "\x1b\u202e"})
			}
			r := store.LegacyReconciliationReceipt{Version: 6, Items: []store.LegacyReconciliationItem{{N: 9007199254740993, Disposition: "verified", State: &current, EnumeratedState: &enumerated, RecipientCasingChanged: true, SenderCasingChanged: true, SenderNameChanged: true}}}
			if err := reconciliationOutput(agent, "review", r); err != nil {
				t.Fatal(err)
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 {
				t.Fatal("missing full private pair", err)
			}
			path := filepath.Join(dir, files[0].Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("overflow is not private", err)
			}
			for _, text := range []string{out.String(), stderr.String(), string(raw)} {
				if strings.Contains(text, secret) {
					t.Fatal("pair bypassed recursive redaction")
				}
			}
			if strings.ContainsAny(out.String()+stderr.String(), "\x1b\u202e") {
				t.Fatal("terminal controls rendered")
			}
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			var left, right map[string]any
			if jsonMode {
				var row map[string]any
				if err = json.Unmarshal([]byte(lines[len(lines)-1]), &row); err != nil {
					t.Fatal(err)
				}
				if row["recipient_casing_changed"] != true || row["sender_casing_changed"] != true || row["sender_name_changed"] != true {
					t.Fatal("missing review flag")
				}
				left = row["enumerated_state"].(map[string]any)
				right = row["state"].(map[string]any)
			} else {
				header, cells := strings.Split(lines[0], "\t"), strings.Split(lines[len(lines)-1], "\t")
				if len(header) != len(cells) {
					t.Fatal("unreadable TSV cell layout")
				}
				for i, key := range header {
					switch key {
					case "sender_name_changed", "sender_casing_changed", "recipient_casing_changed":
						if cells[i] != "true" {
							t.Fatal("TSV omitted review flag")
						}
					case "enumerated_state":
						err = json.Unmarshal([]byte(cells[i]), &left)
					case "state":
						err = json.Unmarshal([]byte(cells[i]), &right)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, state := range []map[string]any{left, right} {
				if len(state["Metadata"].(map[string]any)["Recipients"].([]any)) != 256 {
					t.Fatal("recipient pair truncated")
				}
			}
			for _, want := range []string{"Reader0@Example.org", "Reader255@Example.org", "reader0@example.org", "reader255@example.org", "9007199254740993", "SENDER@Example.org", "sender@example.org", "Current-" + strings.Repeat("名", 4096), "Original-" + strings.Repeat("名", 4096)} {
				if !strings.Contains(string(raw), want) {
					t.Fatal("raw reviewed pair or integer lost")
				}
			}
		})
	}
}

func TestReconciliationSenderDisagreementRendererRetainsLongStates(t *testing.T) {
	NewRootCommand(Deps{})
	const secret = "invented-sender-report-secret"
	agentio.RegisterSecret(secret)
	for _, jsonMode := range []bool{false, true} {
		t.Run(fmt.Sprint(jsonMode), func(t *testing.T) {
			var out, stderr bytes.Buffer
			dir := t.TempDir()
			agent := agentio.New("account reconcile", agentio.WithJSON(jsonMode), agentio.WithStdout(&out), agentio.WithStderr(&stderr), agentio.WithOverflowDir(dir), agentio.WithLimit(1))
			before := mailbox.State{Metadata: mailbox.Metadata{Sender: mailbox.Address{Address: "enumerated-" + strings.Repeat("b", 4096) + secret + "@example.org", Name: "Before\x1b\u202e"}}}
			after := mailbox.State{Metadata: mailbox.Metadata{Sender: mailbox.Address{Address: "current-" + strings.Repeat("a", 4096) + secret + "@example.org", Name: "After\x1b\u202e"}}}
			r := store.LegacyReconciliationReceipt{Version: 6, Items: []store.LegacyReconciliationItem{{N: 9007199254740993, Disposition: "verified", State: &after, EnumeratedState: &before, SenderAddressDisagrees: true, SenderNameChanged: true, RecipientCasingChanged: true}}}
			if err := reconciliationOutput(agent, "review", r); err != nil {
				t.Fatal(err)
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 {
				t.Fatal("missing complete private output", err)
			}
			path := filepath.Join(dir, files[0].Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("unsafe overflow", err)
			}
			for _, text := range []string{out.String(), stderr.String(), string(raw)} {
				if strings.Contains(text, secret) {
					t.Fatal("new sender pair escaped redaction")
				}
			}
			if strings.ContainsAny(out.String()+stderr.String(), "\x1b\u202e") {
				t.Fatal("terminal controls rendered")
			}
			for _, text := range []string{strings.Repeat("a", 4096), strings.Repeat("b", 4096), "9007199254740993"} {
				if !strings.Contains(string(raw), text) {
					t.Fatal("raw sender pair or exact number truncated")
				}
			}
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if jsonMode {
				var row map[string]any
				if err = json.Unmarshal([]byte(lines[len(lines)-1]), &row); err != nil {
					t.Fatal(err)
				}
				if row["sender_address_disagrees"] != true || row["sender_casing_changed"] != false || row["sender_name_changed"] != true || row["recipient_casing_changed"] != true {
					t.Fatal("independent flags lost")
				}
			} else {
				header, cells := strings.Split(lines[0], "\t"), strings.Split(lines[len(lines)-1], "\t")
				if len(header) != len(cells) {
					t.Fatal("invalid TSV")
				}
				seenDisagreement, seenCasing := false, false
				for i, key := range header {
					seenDisagreement = seenDisagreement || key == "sender_address_disagrees"
					seenCasing = seenCasing || key == "sender_casing_changed"
					if key == "sender_address_disagrees" && cells[i] != "true" || key == "sender_casing_changed" && cells[i] != "false" {
						t.Fatal("TSV sender flags conflated")
					}
				}
				if !seenDisagreement || !seenCasing {
					t.Fatal("TSV omitted a sender flag")
				}
			}
		})
	}
}
