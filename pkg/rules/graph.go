package rules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/mrothroc/mailpipe/pkg/graph"
)

// Graph is the part of the Graph client rules use.
type Graph interface {
	Do(ctx context.Context, method, path string, query url.Values, extraPrefer string, body, out any) error
	Batch(ctx context.Context, reqs []graph.BatchRequest) ([]graph.BatchResponse, error)
}

// GraphRules reads and writes the signed-in user's Inbox rules.
type GraphRules struct {
	g      Graph
	junkID string // the Junk folder's id, looked up once when needed
}

// junkFolder returns the Junk folder's id.
func (r *GraphRules) junkFolder(ctx context.Context) (string, error) {
	if r.junkID != "" {
		return r.junkID, nil
	}
	var f struct {
		ID string `json:"id"`
	}
	if err := r.g.Do(ctx, http.MethodGet, "/me/mailFolders/junkemail", url.Values{"$select": {"id"}}, "", nil, &f); err != nil {
		return "", fmt.Errorf("find Junk: %w", err)
	}
	if f.ID == "" {
		return "", errors.New("find Junk: no folder id")
	}
	r.junkID = f.ID
	return f.ID, nil
}

// NewGraphRules returns GraphRules over g.
func NewGraphRules(g Graph) *GraphRules { return &GraphRules{g: g} }

const rulesPath = "/me/mailFolders/inbox/messageRules"

type recipient struct {
	EmailAddress struct {
		Address string `json:"address"`
	} `json:"emailAddress"`
}

type wireRule struct {
	ID          string                     `json:"id"`
	DisplayName string                     `json:"displayName"`
	Sequence    int                        `json:"sequence"`
	IsEnabled   bool                       `json:"isEnabled"`
	Conditions  map[string]json.RawMessage `json:"conditions"`
	Exceptions  map[string]json.RawMessage `json:"exceptions"`
	Actions     map[string]json.RawMessage `json:"actions"`
}

// unset reports whether Graph's value for a rule property says "not used".
func unset(v json.RawMessage) bool {
	switch strings.TrimSpace(string(v)) {
	case "", "null", "false", "[]", "{}", `""`:
		return true
	}
	return false
}

func isTrue(v json.RawMessage) bool { return strings.TrimSpace(string(v)) == "true" }

// only reports whether every property of m other than keep is unset.
func only(m map[string]json.RawMessage, keep ...string) bool {
	for k, v := range m {
		if !unset(v) && !slices.Contains(keep, k) {
			return false
		}
	}
	return true
}

// rule reads a server rule. junkID is the Junk folder's id, needed only to
// recognise a junk rule's shape.
func (w wireRule) rule(junkID func() (string, error)) (Rule, error) {
	r := Rule{ID: w.ID, Name: w.DisplayName, Sequence: w.Sequence, Enabled: w.IsEnabled}
	if isJunk(w.DisplayName) {
		r.Junk = true
		var contains []string
		if v, ok := w.Conditions["senderContains"]; ok && !unset(v) {
			if err := json.Unmarshal(v, &contains); err != nil {
				return r, fmt.Errorf("rule %q: senderContains: %w", w.DisplayName, err)
			}
		}
		r.Senders = normalize(contains)
		var folder string
		_ = json.Unmarshal(w.Actions["moveToFolder"], &folder)
		id, err := junkID()
		if err != nil {
			return r, err
		}
		r.Exact = len(r.Senders) > 0 && only(w.Conditions, "senderContains") && only(w.Exceptions) &&
			only(w.Actions, "moveToFolder", "stopProcessingRules") && folder == id && isTrue(w.Actions["stopProcessingRules"])
		return r, nil
	}
	if from, ok := w.Conditions["fromAddresses"]; ok && !unset(from) {
		var rs []recipient
		if err := json.Unmarshal(from, &rs); err != nil {
			return r, fmt.Errorf("rule %q: fromAddresses: %w", w.DisplayName, err)
		}
		addrs := make([]string, len(rs))
		for i, x := range rs {
			addrs[i] = x.EmailAddress.Address
		}
		r.Senders = normalize(addrs)
	}
	r.Exact = len(r.Senders) > 0 && only(w.Conditions, "fromAddresses") && only(w.Exceptions) &&
		only(w.Actions, "delete", "stopProcessingRules") &&
		isTrue(w.Actions["delete"]) && isTrue(w.Actions["stopProcessingRules"])
	return r, nil
}

// List returns every Inbox rule, in the server's order.
func (r *GraphRules) List(ctx context.Context) ([]Rule, error) {
	var out []Rule
	link := rulesPath
	for link != "" {
		var page struct {
			Value    []wireRule `json:"value"`
			NextLink string     `json:"@odata.nextLink"`
		}
		if err := r.g.Do(ctx, http.MethodGet, link, nil, "", nil, &page); err != nil {
			return nil, err
		}
		for _, w := range page.Value {
			rule, err := w.rule(func() (string, error) { return r.junkFolder(ctx) })
			if err != nil {
				return nil, err
			}
			out = append(out, rule)
		}
		link = page.NextLink
	}
	return out, nil
}

func fromAddresses(senders []string) []recipient {
	out := make([]recipient, len(senders))
	for i, s := range senders {
		out[i].EmailAddress.Address = s
	}
	return out
}

// shape returns the conditions and actions mailpipe writes for rule.
func (r *GraphRules) shape(ctx context.Context, rule Rule) (conditions, actions map[string]any, err error) {
	if !rule.Junk {
		return map[string]any{"fromAddresses": fromAddresses(rule.Senders)}, map[string]any{"delete": true, "stopProcessingRules": true}, nil
	}
	id, err := r.junkFolder(ctx)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"senderContains": rule.Senders}, map[string]any{"moveToFolder": id, "stopProcessingRules": true}, nil
}

// Create writes a new rule with mailpipe's shape at rule.Sequence.
func (r *GraphRules) Create(ctx context.Context, rule Rule) error {
	conditions, actions, err := r.shape(ctx, rule)
	if err != nil {
		return err
	}
	body := map[string]any{"displayName": rule.Name, "sequence": rule.Sequence, "isEnabled": true,
		"conditions": conditions, "actions": actions}
	return r.g.Do(ctx, http.MethodPost, rulesPath, nil, "", body, nil)
}

// Update sets an existing rule's senders and enables it. It is only for a
// rule that already has mailpipe's shape; any other rule is replaced.
func (r *GraphRules) Update(ctx context.Context, rule Rule) error {
	if rule.ID == "" {
		return fmt.Errorf("rule %q has no id to update", rule.Name)
	}
	conditions, _, err := r.shape(ctx, rule)
	if err != nil {
		return err
	}
	body := map[string]any{"isEnabled": true, "conditions": conditions}
	return r.g.Do(ctx, http.MethodPatch, rulesPath+"/"+url.PathEscape(rule.ID), nil, "", body, nil)
}

// Delete removes a rule.
func (r *GraphRules) Delete(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("a rule id is needed to delete it")
	}
	return r.g.Do(ctx, http.MethodDelete, rulesPath+"/"+url.PathEscape(id), nil, "", nil, nil)
}

type folderPage struct {
	Value []struct {
		ParentFolderID string `json:"parentFolderId"`
	} `json:"value"`
	NextLink string `json:"@odata.nextLink"`
}

func (p folderPage) outside(folderID string) int {
	n := 0
	for _, m := range p.Value {
		if m.ParentFolderID != folderID {
			n++
		}
	}
	return n
}

// Kept counts, for each sender, their messages anywhere in the mailbox outside
// Deleted Items. It fails rather than guess: a sender whose search goes
// unanswered must not be treated as having nothing kept.
func (r *GraphRules) Kept(ctx context.Context, senders []string) (map[string]int, error) {
	if len(senders) == 0 {
		return map[string]int{}, nil
	}
	var del struct {
		ID string `json:"id"`
	}
	if err := r.g.Do(ctx, http.MethodGet, "/me/mailFolders/deleteditems", url.Values{"$select": {"id"}}, "", nil, &del); err != nil {
		return nil, fmt.Errorf("find Deleted Items: %w", err)
	}
	if del.ID == "" {
		return nil, errors.New("find Deleted Items: no folder id")
	}
	out := make(map[string]int, len(senders))
	for start := 0; start < len(senders); start += graph.MaxBatch {
		chunk := senders[start:min(start+graph.MaxBatch, len(senders))]
		reqs := make([]graph.BatchRequest, len(chunk))
		for i, s := range chunk {
			q := url.Values{"$filter": {"from/emailAddress/address eq '" + strings.ReplaceAll(s, "'", "''") + "'"},
				"$select": {"parentFolderId"}, "$top": {"1000"}}
			reqs[i] = graph.BatchRequest{ID: strconv.Itoa(i + 1), Method: http.MethodGet, URL: "/me/messages?" + q.Encode()}
		}
		resps, err := r.g.Batch(ctx, reqs)
		if err != nil {
			return nil, err
		}
		for i, resp := range resps {
			sender := chunk[i]
			if err := resp.Err(); err != nil {
				return nil, fmt.Errorf("search mail from %s: %w", sender, err)
			}
			var page folderPage
			if err := json.Unmarshal(resp.Body, &page); err != nil {
				return nil, fmt.Errorf("search mail from %s: %w", sender, err)
			}
			n := page.outside(del.ID)
			for page.NextLink != "" {
				link := page.NextLink
				page = folderPage{}
				if err := r.g.Do(ctx, http.MethodGet, link, nil, "", nil, &page); err != nil {
					return nil, fmt.Errorf("search mail from %s: %w", sender, err)
				}
				n += page.outside(del.ID)
			}
			out[sender] = n
		}
	}
	return out, nil
}
