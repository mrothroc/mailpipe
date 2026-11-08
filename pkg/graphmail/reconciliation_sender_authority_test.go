package graphmail_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func TestReconciliationInspectionSenderMatchesHistoryHTTP(t *testing.T) {
	for _, combined := range []bool{false, true} {
		t.Run(map[bool]string{false: "sender-only", true: "combined"}[combined], func(t *testing.T) {
			before := []mailbox.Address{{Address: "reader@example.org", Name: "Reader"}}
			after := []mailbox.Address{{Address: "reader@example.org", Name: "Reader"}}
			if combined {
				before[0].Address = "Reader@Example.org"
			}
			proof, err := collectObservations(t, "sender@example.org", before, after, func(m map[string]any) {
				m["from"] = map[string]any{"emailAddress": map[string]string{"address": "enumerated@example.org", "name": "Before"}}
			}, func(m map[string]any) {
				name := "Before"
				if combined {
					name = "After"
				}
				m["from"] = map[string]any{"emailAddress": map[string]string{"address": "sender@example.org", "name": name}}
			})
			if err != nil {
				t.Fatal("history-matching exact inspection still blocked", err)
			}
			if proof.States[1].Metadata.Sender.Address != "sender@example.org" || proof.EnumeratedStates[1].Metadata.Sender.Address != "enumerated@example.org" {
				t.Fatal("both raw observations not preserved")
			}
		})
	}
}

func TestReconciliationSenderAuthorityInvalidWireEncodingHTTP(t *testing.T) {
	for _, phase := range []string{"enumeration", "inspection"} {
		t.Run(phase, func(t *testing.T) {
			box := &serverBox{folders: map[string]string{"one": "inbox"}, sender: "sender@example.org"}
			pages, inspections := 0, 0
			srv := httptest.NewServer(readOnlyBatchHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				m := box.wire("one")
				m["toRecipients"], m["ccRecipients"] = []any{}, []any{}
				enumeration := r.URL.Path == "/me/messages"
				address := "sender@example.org"
				if enumeration {
					address = "enumerated@example.org"
					pages++
				} else {
					inspections++
					if pages != 1 {
						t.Error("inspection preceded enumeration")
					}
				}
				m["from"] = map[string]any{"emailAddress": map[string]string{"address": address}}
				var body any = m
				if enumeration {
					body = map[string]any{"value": []any{m}}
				}
				raw, err := json.Marshal(body)
				if err != nil {
					t.Error(err)
					return
				}
				if enumeration == (phase == "enumeration") {
					raw = bytes.ReplaceAll(raw, []byte(address), []byte("invalid\xff@example.org"))
				}
				w.Write(raw)
			})))
			defer srv.Close()
			g := graph.New(func(context.Context) (string, error) { return "invented-token", nil })
			g.Base, g.HTTP = srv.URL, srv.Client()
			p, err := graphmail.New(g, account)
			if err != nil {
				t.Fatal(err)
			}
			refs := []graphmail.LegacyReference{{N: 1, Ref: mailbox.MessageRef{AccountID: account.ID, MessageID: "one", Locator: "one"}, Collection: mailbox.CollectionRef{AccountID: account.ID, CollectionID: "inbox"}, InternetMessageID: "<one@example.org>", Sender: "sender@example.org"}}
			proof, err := p.ReconcileLegacyReferences(context.Background(), refs)
			if err == nil || !reflect.DeepEqual(proof, graphmail.LegacyEvidence{}) || strings.Contains(err.Error(), "@example.org") {
				t.Fatal("invalid raw provenance admitted or leaked")
			}
			if pages != 1 || inspections != 1 {
				t.Fatal("invalid provenance added reads")
			}
		})
	}
}
