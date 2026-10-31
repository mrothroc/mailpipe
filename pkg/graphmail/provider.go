// Package graphmail adapts Microsoft Graph mailbox data and reviewed operations
// to the provider-neutral mailbox contract. HTTP mechanics remain in graph.
package graphmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/rules"
)

// Transport is Graph's authenticated wire surface. It is never exposed to
// generic synchronization or action consumers.
type Transport interface {
	Get(context.Context, string, url.Values, any) error
	Do(context.Context, string, string, url.Values, string, any, any) error
	Batch(context.Context, []graph.BatchRequest) ([]graph.BatchResponse, error)
	WriteRaw(context.Context, string, io.Writer, int64) error
}

type Provider struct {
	g       Transport
	account mailbox.Account
}

var _ mailbox.Provider = (*Provider)(nil)
var _ mailbox.TrashCapability = (*Provider)(nil)

func New(g Transport, a mailbox.Account) (*Provider, error) {
	if g == nil {
		return nil, errors.New("graph mailbox: transport required")
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	if a.ProviderID != "graph" {
		return nil, &mailbox.UnsupportedError{Operation: "Graph mailbox provider"}
	}
	return &Provider{g: g, account: a}, nil
}
func (p *Provider) Account() mailbox.Account { return p.account }
func (p *Provider) ref(id string) mailbox.MessageRef {
	return mailbox.MessageRef{AccountID: p.account.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)}
}
func (p *Provider) check(ref mailbox.MessageRef) error {
	if err := ref.Validate(p.account.ID); err != nil {
		return err
	}
	if string(ref.MessageID) != string(ref.Locator) {
		return stale("message locator changed")
	}
	return nil
}
func stale(reason string) error {
	return &mailbox.StaleIdentityError{Operation: "Graph mailbox", Reason: reason}
}
func remoteError(err error) error {
	var e *graph.APIError
	if errors.As(err, &e) && (e.Status == 404 || e.Status == 410) {
		return errors.Join(stale("message or cursor is no longer available"), err)
	}
	return err
}

const selectFields = "id,internetMessageId,subject,from,sender,toRecipients,ccRecipients,receivedDateTime,hasAttachments,isRead,bodyPreview,parentFolderId,changeKey"

type address struct {
	EmailAddress mailbox.Address `json:"emailAddress"`
}
type wireMessage struct {
	present           map[string]bool
	ID                string          `json:"id"`
	InternetMessageID string          `json:"internetMessageId"`
	Subject           string          `json:"subject"`
	From              *address        `json:"from"`
	Sender            *address        `json:"sender"`
	To                []address       `json:"toRecipients"`
	Cc                []address       `json:"ccRecipients"`
	Received          string          `json:"receivedDateTime"`
	Attachments       bool            `json:"hasAttachments"`
	Read              bool            `json:"isRead"`
	Preview           string          `json:"bodyPreview"`
	Parent            string          `json:"parentFolderId"`
	Revision          string          `json:"changeKey"`
	Removed           json.RawMessage `json:"@removed"`
}

func (w *wireMessage) UnmarshalJSON(raw []byte) error {
	type plain wireMessage
	if err := json.Unmarshal(raw, (*plain)(w)); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	w.present = make(map[string]bool, len(fields))
	for key := range fields {
		w.present[key] = true
	}
	return nil
}
func (w wireMessage) completeMetadata() bool {
	for _, key := range []string{"id", "internetMessageId", "subject", "toRecipients", "ccRecipients", "receivedDateTime", "hasAttachments", "isRead", "bodyPreview"} {
		if !w.present[key] {
			return false
		}
	}
	return true
}

func (w wireMessage) metadata() (mailbox.Metadata, error) {
	m := mailbox.Metadata{InternetMessageID: w.InternetMessageID, Subject: w.Subject, Preview: w.Preview, HasAttachments: w.Attachments, IsRead: w.Read}
	if w.Received != "" {
		t, err := time.Parse(time.RFC3339, w.Received)
		if err != nil {
			return m, errors.New("Graph mailbox: malformed received time")
		}
		m.Received = t
	}
	from := w.From
	if from == nil {
		from = w.Sender
	}
	if from != nil {
		m.Sender = from.EmailAddress
	}
	for _, r := range append(append([]address{}, w.To...), w.Cc...) {
		m.Recipients = append(m.Recipients, r.EmailAddress)
	}
	return m, nil
}
func (p *Provider) state(w wireMessage) (mailbox.State, error) {
	if w.ID == "" || w.Parent == "" || !w.completeMetadata() {
		return mailbox.State{}, errors.New("Graph mailbox: incomplete message identity or membership")
	}
	m, err := w.metadata()
	if err != nil {
		return mailbox.State{}, err
	}
	return mailbox.State{Ref: p.ref(w.ID), Metadata: m, Memberships: []mailbox.Membership{{Collection: mailbox.CollectionRef{AccountID: p.account.ID, CollectionID: mailbox.CollectionID(w.Parent)}}}, Revision: w.Revision}, nil
}
func (p *Provider) Resolve(ctx context.Context, name string) (mailbox.Collection, error) {
	var f struct {
		ID   string `json:"id"`
		Name string `json:"displayName"`
	}
	if strings.TrimSpace(name) == "" {
		return mailbox.Collection{}, errors.New("Graph mailbox: folder required")
	}
	if err := p.g.Get(ctx, "/me/mailFolders/"+url.PathEscape(name), url.Values{"$select": {"id,displayName"}}, &f); err != nil {
		return mailbox.Collection{}, err
	}
	if f.ID == "" {
		return mailbox.Collection{}, errors.New("Graph mailbox: folder has no identity")
	}
	return mailbox.Collection{Ref: mailbox.CollectionRef{AccountID: p.account.ID, CollectionID: mailbox.CollectionID(f.ID)}, Name: f.Name}, nil
}
func (p *Provider) Total(ctx context.Context, scope mailbox.CollectionRef) (int, error) {
	if err := scope.Validate(p.account.ID); err != nil {
		return 0, err
	}
	var f struct {
		Total int `json:"totalItemCount"`
	}
	err := p.g.Get(ctx, "/me/mailFolders/"+url.PathEscape(string(scope.CollectionID)), url.Values{"$select": {"totalItemCount"}}, &f)
	return f.Total, err
}
func (p *Provider) Collections(ctx context.Context) ([]mailbox.Collection, error) {
	var out []mailbox.Collection
	queue := []string{"/me/mailFolders"}
	seen := map[string]bool{}
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		if seen[path] {
			return nil, errors.New("Graph mailbox: folder pagination cycle")
		}
		seen[path] = true
		var page struct {
			Value []struct {
				ID       string `json:"id"`
				Name     string `json:"displayName"`
				Children int    `json:"childFolderCount"`
			} `json:"value"`
			Next string `json:"@odata.nextLink"`
		}
		if err := p.g.Get(ctx, path, nil, &page); err != nil {
			return nil, err
		}
		for _, f := range page.Value {
			if f.ID == "" || len(out) >= 10000 {
				return nil, errors.New("Graph mailbox: invalid or oversized folder inventory")
			}
			out = append(out, mailbox.Collection{Ref: mailbox.CollectionRef{AccountID: p.account.ID, CollectionID: mailbox.CollectionID(f.ID)}, Name: f.Name})
			if f.Children > 0 {
				queue = append(queue, "/me/mailFolders/"+url.PathEscape(f.ID)+"/childFolders")
			}
		}
		if page.Next != "" {
			queue = append(queue, page.Next)
		}
	}
	return out, nil
}
func (p *Provider) Inspect(ctx context.Context, ref mailbox.MessageRef) (mailbox.State, error) {
	if err := p.check(ref); err != nil {
		return mailbox.State{}, err
	}
	var w wireMessage
	if err := p.g.Get(ctx, "/me/messages/"+url.PathEscape(string(ref.Locator)), url.Values{"$select": {selectFields}}, &w); err != nil {
		return mailbox.State{}, remoteError(err)
	}
	s, err := p.state(w)
	if err == nil && s.Ref != ref {
		return mailbox.State{}, stale("returned message identity differs")
	}
	return s, err
}
func (p *Provider) Metadata(ctx context.Context, ref mailbox.MessageRef) (mailbox.Metadata, error) {
	s, err := p.Inspect(ctx, ref)
	return s.Metadata, err
}
func (p *Provider) Text(ctx context.Context, ref mailbox.MessageRef) (string, error) {
	if err := p.check(ref); err != nil {
		return "", err
	}
	var out struct {
		Body *struct {
			Content *string `json:"content"`
			Type    string  `json:"contentType"`
		} `json:"body"`
	}
	if err := p.g.Get(ctx, "/me/messages/"+url.PathEscape(string(ref.Locator)), url.Values{"$select": {"body"}}, &out); err != nil {
		var limit *graph.ResponseLimitError
		if errors.As(err, &limit) {
			return "", &textLimitError{cause: err, limit: limit.Limit}
		}
		return "", remoteError(err)
	}
	if out.Body == nil || out.Body.Content == nil || (out.Body.Type != "" && !strings.EqualFold(out.Body.Type, "text")) {
		return "", errors.New("Graph mailbox: complete text body unavailable")
	}
	return *out.Body.Content, nil
}

type textLimitError struct {
	cause error
	limit int64
}

func (e *textLimitError) Error() string {
	return fmt.Sprintf("read text: message exceeds %d-byte content limit; incomplete content was discarded", e.limit)
}
func (e *textLimitError) Unwrap() error { return e.cause }

func (p *Provider) WriteMIME(ctx context.Context, ref mailbox.MessageRef, w io.Writer) error {
	if err := p.check(ref); err != nil {
		return err
	}
	return remoteError(p.g.WriteRaw(ctx, "/me/messages/"+url.PathEscape(string(ref.Locator))+"/$value", w, 64<<20))
}

type cursor struct {
	Version int              `json:"v"`
	Mode    mailbox.SyncMode `json:"mode"`
	Link    string           `json:"link"`
}

func (p *Provider) cursor(scope mailbox.CollectionRef, mode mailbox.SyncMode, link string) mailbox.Cursor {
	if link == "" {
		return mailbox.Cursor{}
	}
	b, _ := json.Marshal(cursor{1, mode, link})
	return mailbox.Cursor{AccountID: p.account.ID, CollectionID: scope.CollectionID, Token: string(b)}
}
func (p *Provider) Sync(ctx context.Context, r mailbox.SyncRequest) (mailbox.SyncPage, error) {
	page, _, err := p.SyncStates(ctx, r)
	return page, err
}
func (p *Provider) SyncStates(ctx context.Context, r mailbox.SyncRequest) (mailbox.SyncPage, []mailbox.State, error) {
	if err := r.Validate(p.account.ID); err != nil {
		return mailbox.SyncPage{}, nil, err
	}
	mode := mailbox.Snapshot
	path := "/me/mailFolders/" + url.PathEscape(string(r.Collection.CollectionID)) + "/messages/delta"
	q := url.Values{"$select": {selectFields}}
	if r.Cursor != (mailbox.Cursor{}) {
		var c cursor
		if err := json.Unmarshal([]byte(r.Cursor.Token), &c); err != nil || c.Version != 1 || c.Link == "" || (c.Mode != mailbox.Delta && c.Mode != mailbox.Snapshot) {
			return mailbox.SyncPage{}, nil, stale("invalid cursor; run an explicit full scan")
		}
		mode, path, q = c.Mode, c.Link, nil
	}
	var wire struct {
		Value     *[]json.RawMessage `json:"value"`
		NextLink  string             `json:"@odata.nextLink"`
		DeltaLink string             `json:"@odata.deltaLink"`
	}
	if err := p.g.Do(ctx, http.MethodGet, path, q, fmt.Sprintf("odata.maxpagesize=%d", graph.PageSize), nil, &wire); err != nil {
		return mailbox.SyncPage{}, nil, remoteError(err)
	}
	if wire.Value == nil || (wire.NextLink == "") == (wire.DeltaLink == "") {
		return mailbox.SyncPage{}, nil, errors.New("Graph mailbox: incomplete delta page or terminal evidence")
	}
	page := mailbox.SyncPage{Mode: mode, Complete: wire.NextLink == ""}
	var states []mailbox.State
	// Delta can repeat a message within a page. Preserve its final observation.
	records := map[string]wireMessage{}
	var order []string
	for _, raw := range *wire.Value {
		var w wireMessage
		if err := json.Unmarshal(raw, &w); err != nil {
			return page, nil, errors.New("Graph mailbox: malformed delta record")
		}
		if w.ID == "" {
			return page, nil, errors.New("Graph mailbox: delta record has no identity")
		}
		if _, ok := records[w.ID]; !ok {
			order = append(order, w.ID)
		}
		if len(w.Removed) == 0 && !w.completeMetadata() {
			id := w.ID
			if err := p.g.Get(ctx, "/me/messages/"+url.PathEscape(id), url.Values{"$select": {selectFields}}, &w); err != nil {
				var api *graph.APIError
				if errors.As(err, &api) && api.Status == 404 {
					w = wireMessage{ID: id, Removed: json.RawMessage(`{}`)}
				} else {
					return page, nil, err
				}
			}
			if w.ID != id {
				return page, nil, stale("hydration identity differs")
			}
		}
		if len(w.Removed) == 0 && !w.completeMetadata() {
			return page, nil, errors.New("Graph mailbox: incomplete hydrated metadata")
		}
		records[w.ID] = w
	}
	for _, id := range order {
		w := records[id]
		if len(w.Removed) > 0 || (w.Parent != "" && w.Parent != string(r.Collection.CollectionID)) {
			if mode == mailbox.Delta {
				page.Removed = append(page.Removed, p.ref(id))
			}
			continue
		}
		m, err := w.metadata()
		if err != nil {
			return page, nil, err
		}
		msg := mailbox.Message{Ref: p.ref(id), Metadata: m}
		page.Messages = append(page.Messages, msg)
		states = append(states, mailbox.State{Ref: msg.Ref, Metadata: m, Revision: w.Revision, Memberships: []mailbox.Membership{{Collection: r.Collection}}})
	}
	if page.Complete {
		page.Checkpoint = p.cursor(r.Collection, mailbox.Delta, wire.DeltaLink)
	} else {
		page.Next = p.cursor(r.Collection, mode, wire.NextLink)
	}
	return page, states, page.Validate(r.Collection)
}

// ManagedRules retains Graph's richer junk/block rules; the narrower generic
// NativeRule subset does not authorize losing their shape or order semantics.
func (p *Provider) ManagedRules() rules.ManagedService { return rules.NewGraphRules(p.g) }

func (p *Provider) Trash(ctx context.Context, r mailbox.ReviewedAction) (mailbox.State, error) {
	states, errs, err := p.TrashBatch(ctx, []mailbox.ReviewedAction{r})
	if err != nil {
		return mailbox.State{}, err
	}
	return states[0], errs[0]
}

// TrashBatch rechecks each chunk immediately before moving only exact matches.
// Wire chunking, rejected-request retry and returned-state proof stay here.
func (p *Provider) TrashBatch(ctx context.Context, requests []mailbox.ReviewedAction) ([]mailbox.State, []error, error) {
	states := make([]mailbox.State, len(requests))
	errs := make([]error, len(requests))
	for _, r := range requests {
		if err := r.Validate(p.account.ID); err != nil {
			return states, errs, err
		}
		if err := p.check(r.Expected.Ref); err != nil {
			return states, errs, err
		}
	}
	destination, err := p.Resolve(ctx, "deleteditems")
	if err != nil {
		return states, errs, err
	}
	for start := 0; start < len(requests); start += graph.MaxBatch {
		var pending []int
		for i := start; i < min(start+graph.MaxBatch, len(requests)); i++ {
			pending = append(pending, i)
		}
		for attempt := 0; len(pending) > 0 && attempt < graph.MaxTries; attempt++ {
			look := make([]graph.BatchRequest, len(pending))
			for j, i := range pending {
				look[j] = graph.BatchRequest{ID: strconv.Itoa(j + 1), Method: http.MethodGet, URL: "/me/messages/" + url.PathEscape(string(requests[i].Expected.Ref.Locator)) + "?" + url.Values{"$select": {selectFields}}.Encode()}
			}
			found, e := p.g.Batch(ctx, look)
			if e != nil {
				return states, errs, e
			}
			if len(found) != len(look) {
				return states, errs, errors.New("Graph mailbox: incomplete inspection batch")
			}
			var ready []int
			var moves []graph.BatchRequest
			for j, i := range pending {
				if e = found[j].Err(); e == nil {
					var w wireMessage
					e = json.Unmarshal(found[j].Body, &w)
					if e == nil {
						var current mailbox.State
						current, e = p.state(w)
						if e == nil {
							e = mailbox.CheckPrecondition(p.account.ID, requests[i].Expected, current)
						}
					}
				}
				if e != nil {
					errs[i] = remoteError(e)
					continue
				}
				ready = append(ready, i)
				moves = append(moves, graph.BatchRequest{ID: strconv.Itoa(len(moves) + 1), Method: http.MethodPost, URL: "/me/messages/" + url.PathEscape(string(requests[i].Expected.Ref.Locator)) + "/move", Body: map[string]string{"destinationId": string(destination.Ref.CollectionID)}})
			}
			if len(moves) == 0 {
				break
			}
			out, e := p.g.Batch(ctx, moves)
			if e != nil {
				return states, errs, fmt.Errorf("Graph mailbox: uncertain move batch; reconcile before retry: %w", e)
			}
			if len(out) != len(moves) {
				return states, errs, errors.New("Graph mailbox: uncertain incomplete move batch; reconcile before retry")
			}
			pending = nil
			wait := time.Second
			for j, i := range ready {
				response := out[j]
				if response.Status == 429 && attempt < graph.MaxTries-1 {
					pending = append(pending, i)
					for k, v := range response.Headers {
						if strings.EqualFold(k, "Retry-After") {
							if seconds, e := strconv.Atoi(v); e == nil && seconds >= 0 && seconds <= 300 {
								wait = max(wait, time.Duration(seconds)*time.Second)
							}
						}
					}
					continue
				}
				if e = response.Err(); e != nil {
					errs[i] = e
					continue
				}
				var w wireMessage
				e = json.Unmarshal(response.Body, &w)
				var current mailbox.State
				if e == nil {
					current, e = p.state(w)
				}
				if e != nil {
					current, e = p.Inspect(ctx, requests[i].Expected.Ref)
				}
				if e == nil && (current.Ref.MessageID != requests[i].Expected.Ref.MessageID || current.Metadata.InternetMessageID != requests[i].Expected.Metadata.InternetMessageID || !strings.EqualFold(strings.TrimSpace(current.Metadata.Sender.Address), strings.TrimSpace(requests[i].Expected.Metadata.Sender.Address)) || len(current.Memberships) != 1 || current.Memberships[0].Collection != destination.Ref) {
					e = stale("move result requires reconciliation")
				}
				states[i], errs[i] = current, e
			}
			if len(pending) > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return states, errs, ctx.Err()
				case <-timer.C:
				}
			}
		}
	}
	return states, errs, nil
}

// InspectBatch preserves ordered per-item checks while keeping Graph's batch
// limit and wire shapes entirely inside its adapter.
func (p *Provider) InspectBatch(ctx context.Context, refs []mailbox.MessageRef) ([]mailbox.State, []error, error) {
	states := make([]mailbox.State, len(refs))
	errs := make([]error, len(refs))
	for _, ref := range refs {
		if err := p.check(ref); err != nil {
			return states, errs, err
		}
	}
	for start := 0; start < len(refs); start += graph.MaxBatch {
		end := min(start+graph.MaxBatch, len(refs))
		reqs := make([]graph.BatchRequest, end-start)
		for j, ref := range refs[start:end] {
			reqs[j] = graph.BatchRequest{ID: strconv.Itoa(j + 1), Method: http.MethodGet, URL: "/me/messages/" + url.PathEscape(string(ref.Locator)) + "?" + url.Values{"$select": {selectFields}}.Encode()}
		}
		out, err := p.g.Batch(ctx, reqs)
		if err != nil {
			return states, errs, err
		}
		if len(out) != len(reqs) {
			return states, errs, errors.New("Graph mailbox: incomplete inspection batch")
		}
		for j, response := range out {
			i := start + j
			e := response.Err()
			if e == nil {
				var w wireMessage
				e = json.Unmarshal(response.Body, &w)
				if e == nil {
					states[i], e = p.state(w)
					if e == nil && states[i].Ref != refs[i] {
						e = stale("inspection identity differs")
					}
				}
			}
			errs[i] = remoteError(e)
		}
	}
	return states, errs, nil
}
