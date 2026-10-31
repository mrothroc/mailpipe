package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
)

// commandStore preserves CLI view rows while routing every bound operation
// through AccountStore. Legacy storage is available only while unbound.
type commandStore struct {
	legacy *store.Store
	bound  *store.AccountStore
	cfg    config.Config
}

func (s *commandStore) Close() error {
	if s.bound != nil {
		return s.bound.Close()
	}
	return s.legacy.Close()
}
func (s *commandStore) local(n int64) mailbox.LocalRef {
	return mailbox.LocalRef{AccountID: s.bound.Account().ID, N: n}
}
func (s *commandStore) collection(name string) mailbox.CollectionRef {
	id := s.cfg.Collections[name]
	if id == "" {
		id = name
	}
	return mailbox.CollectionRef{AccountID: s.bound.Account().ID, CollectionID: mailbox.CollectionID(id)}
}
func (s *commandStore) alias(id string) string {
	for _, name := range []string{"inbox", "deleteditems", "sentitems", "junkemail"} {
		if s.cfg.Collections[name] == id {
			return name
		}
	}
	var names []string
	for name, value := range s.cfg.Collections {
		if value == id {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		return names[0]
	}
	return id
}

func (s *commandStore) filter(f store.Filter) store.AccountFilter {
	out := store.AccountFilter{Filter: f}
	out.Filter.Folder = ""
	if f.Folder != "" {
		c := s.collection(f.Folder)
		out.Collection = &c
	}
	return out
}
func (s *commandStore) view(m store.StoredMessage) store.Message {
	meta := m.State.Metadata
	out := store.Message{N: m.Local.N, InternetMessageID: meta.InternetMessageID, Sender: strings.ToLower(meta.Sender.Address), SenderName: meta.Sender.Name, Subject: meta.Subject, Preview: meta.Preview, HasAttachments: meta.HasAttachments, IsRead: meta.IsRead, Body: m.Body}
	if m.GraphID != nil {
		out.GraphID = *m.GraphID
	}
	if !meta.Received.IsZero() {
		out.Received = meta.Received.UTC().Format(time.RFC3339Nano)
	}
	for _, r := range meta.Recipients {
		out.Recipients = append(out.Recipients, strings.ToLower(r.Address))
	}
	if out.Recipients == nil {
		out.Recipients = []string{}
	}
	if len(m.State.Memberships) == 1 {
		out.Folder = s.alias(string(m.State.Memberships[0].Collection.CollectionID))
	}
	return out
}
func (s *commandStore) views(ms []store.StoredMessage) []store.Message {
	out := make([]store.Message, len(ms))
	for i, m := range ms {
		out[i] = s.view(m)
	}
	return out
}
func (s *commandStore) Count(ctx context.Context, f store.Filter) (int, error) {
	if s.bound == nil {
		return s.legacy.Count(ctx, f)
	}
	return s.bound.Count(ctx, s.filter(f))
}
func (s *commandStore) Messages(ctx context.Context, f store.Filter) ([]store.Message, error) {
	if s.bound == nil {
		return s.legacy.Messages(ctx, f)
	}
	ms, err := s.bound.Messages(ctx, s.filter(f))
	return s.views(ms), err
}
func (s *commandStore) Get(ctx context.Context, n int64) (store.Message, bool, error) {
	if s.bound == nil {
		return s.legacy.Get(ctx, n)
	}
	m, err := s.bound.GetMessage(ctx, s.local(n))
	if errors.Is(err, mailbox.ErrStaleIdentity) {
		return store.Message{}, false, nil
	}
	return s.view(m), err == nil, err
}
func (s *commandStore) remoteRef(ctx context.Context, n int64) (mailbox.MessageRef, error) {
	if s.bound == nil {
		return mailbox.MessageRef{}, errors.New("legacy mailbox requires explicit attribution. Try: mailpipe account bind")
	}
	m, err := s.bound.GetMessage(ctx, s.local(n))
	if err != nil {
		return mailbox.MessageRef{}, err
	}
	if !m.Valid {
		return mailbox.MessageRef{}, &mailbox.StaleIdentityError{Operation: "fetch content", Reason: "message identity retired"}
	}
	return m.State.Ref, nil
}
func (s *commandStore) SetBody(ctx context.Context, n int64, body string) error {
	if s.bound == nil {
		return s.legacy.SetBody(ctx, n, body)
	}
	return s.bound.SetBody(ctx, s.local(n), body)
}
func (s *commandStore) Pending(ctx context.Context, f store.Filter, name string) ([]store.Message, error) {
	if s.bound == nil {
		return s.legacy.Pending(ctx, f, name)
	}
	ms, err := s.bound.Pending(ctx, s.filter(f), name)
	return s.views(ms), err
}
func (s *commandStore) SentRecipients(ctx context.Context) ([]string, error) {
	if s.bound == nil {
		return s.legacy.SentRecipients(ctx)
	}
	ms, err := s.Messages(ctx, store.Filter{Folder: "sentitems"})
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, m := range ms {
		for _, r := range m.Recipients {
			set[strings.ToLower(r)] = true
		}
	}
	var out []string
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out, nil
}
func (s *commandStore) AddVerdict(ctx context.Context, v store.Verdict) error {
	if s.bound == nil {
		return s.legacy.AddVerdict(ctx, v)
	}
	return s.bound.AddVerdict(ctx, s.local(v.N), v)
}
func (s *commandStore) AddVerdicts(ctx context.Context, vs []store.Verdict) error {
	if s.bound == nil {
		return s.legacy.AddVerdicts(ctx, vs)
	}
	batch := make([]store.BoundVerdict, len(vs))
	for i, v := range vs {
		batch[i] = store.BoundVerdict{Local: s.local(v.N), Verdict: v}
	}
	return s.bound.AddVerdictsBatch(ctx, batch)
}
func (s *commandStore) Verdicts(ctx context.Context, n int64) ([]store.Verdict, error) {
	if s.bound == nil {
		return s.legacy.Verdicts(ctx, n)
	}
	return s.bound.Verdicts(ctx, s.local(n))
}
func (s *commandStore) HasVerdict(ctx context.Context, n int64, stage, provenance, at string) (bool, error) {
	if s.bound == nil {
		return s.legacy.HasVerdict(ctx, n, stage, provenance, at)
	}
	return s.bound.HasVerdict(ctx, s.local(n), stage, provenance, at)
}
func (s *commandStore) LatestBySender(ctx context.Context, name string) (map[string][]store.Verdict, error) {
	if s.bound == nil {
		return s.legacy.LatestBySender(ctx, name)
	}
	return s.bound.LatestBySender(ctx, name)
}
func (s *commandStore) LatestVerdicts(ctx context.Context, f store.Filter) (map[int64][]store.Verdict, error) {
	if s.bound == nil {
		return s.legacy.LatestVerdicts(ctx, f)
	}
	rows, err := s.bound.LatestVerdicts(ctx, s.filter(f))
	out := map[int64][]store.Verdict{}
	for ref, vs := range rows {
		out[ref.N] = vs
	}
	return out, err
}
func (s *commandStore) ByMessageID(ctx context.Context, id string) ([]store.Message, error) {
	if s.bound == nil {
		return s.legacy.ByMessageID(ctx, id)
	}
	ms, err := s.bound.ByMessageID(ctx, id)
	return s.views(ms), err
}
func (s *commandStore) CreatePlan(ctx context.Context, scope string, items []store.PlanItem) (int64, error) {
	if s.bound == nil {
		return s.legacy.CreatePlan(ctx, scope, items)
	}
	return s.bound.CreateBoundPlan(ctx, scope, items)
}
func (s *commandStore) Plan(ctx context.Context, id int64) (store.Plan, []store.PlanItem, error) {
	if s.bound == nil {
		return s.legacy.Plan(ctx, id)
	}
	p, err := s.bound.PlanHistory(ctx, id)
	if errors.Is(err, mailbox.ErrStaleIdentity) {
		return store.Plan{}, nil, fmt.Errorf("no plan %d", id)
	}
	items := make([]store.PlanItem, len(p.Items))
	for i, item := range p.Items {
		items[i] = item.Item
		items[i].Folder = s.alias(items[i].Folder)
		items[i].PlannedFolder = s.alias(items[i].PlannedFolder)
	}
	return p.Plan, items, err
}
func (s *commandStore) Plans(ctx context.Context) ([]store.Plan, error) {
	if s.bound == nil {
		return s.legacy.Plans(ctx)
	}
	return s.bound.Plans(ctx)
}
func (s *commandStore) Actions(ctx context.Context, id int64) ([]store.Action, error) {
	if s.bound == nil {
		return s.legacy.Actions(ctx, id)
	}
	rows, err := s.bound.Actions(ctx, id)
	out := make([]store.Action, len(rows))
	for i, row := range rows {
		out[i] = row.Audit
		out[i].Destination = s.alias(out[i].Destination)
	}
	return out, err
}
func (s *commandStore) MovedSenders(ctx context.Context) ([]store.SenderMoves, error) {
	if s.bound == nil {
		return s.legacy.MovedSenders(ctx)
	}
	return s.bound.MovedSenders(ctx)
}
func (s *commandStore) ExpiredSenders(ctx context.Context) (map[string]int, error) {
	if s.bound == nil {
		return s.legacy.ExpiredSenders(ctx)
	}
	return s.bound.ExpiredSenders(ctx)
}
