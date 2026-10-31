package cli_test

import (
	"context"
	"strings"
	"time"

	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type boundFixture struct{ *store.AccountStore }

func (e *env) boundStore() (*boundFixture, error) {
	ctx := context.Background()
	subject, err := e.auth.Subject(ctx)
	if err != nil {
		return nil, err
	}
	s, err := store.OpenAccount(ctx, e.db, subject.Account())
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	cfg.AccountID = string(subject.Account().ID)
	cfg.Collections = map[string]string{"inbox": "F-inbox", "deleteditems": "F-deleteditems", "sentitems": "F-sentitems"}
	if err = config.Save(config.DefaultPath(), cfg); err != nil {
		_ = s.Close()
		return nil, err
	}
	return &boundFixture{s}, nil
}
func (s *boundFixture) Upsert(ctx context.Context, folder string, r store.Record) (mailbox.LocalRef, error) {
	received, _ := time.Parse(time.RFC3339, r.Received)
	m := mailbox.Message{Ref: mailbox.MessageRef{AccountID: s.Account().ID, MessageID: mailbox.MessageID(r.GraphID), Locator: mailbox.Locator(r.GraphID)}, Metadata: mailbox.Metadata{InternetMessageID: r.InternetMessageID, Sender: mailbox.Address{Address: r.Sender, Name: r.SenderName}, Subject: r.Subject, Received: received, Preview: r.Preview, HasAttachments: r.HasAttachments, IsRead: r.IsRead}}
	for _, recipient := range r.Recipients {
		m.Metadata.Recipients = append(m.Metadata.Recipients, mailbox.Address{Address: recipient})
	}
	return s.UpsertMessage(ctx, mailbox.Collection{Ref: mailbox.CollectionRef{AccountID: s.Account().ID, CollectionID: mailbox.CollectionID("F-" + folder)}}, store.MessageRecord{Message: m, GraphID: r.GraphID})
}
func (s *boundFixture) Messages(ctx context.Context, f store.Filter) ([]store.Message, error) {
	af := store.AccountFilter{Filter: f}
	af.Filter.Folder = ""
	if f.Folder != "" {
		c := mailbox.CollectionRef{AccountID: s.Account().ID, CollectionID: mailbox.CollectionID("F-" + f.Folder)}
		af.Collection = &c
	}
	ms, err := s.AccountStore.Messages(ctx, af)
	var out []store.Message
	for _, m := range ms {
		out = append(out, store.Message{N: m.Local.N, GraphID: string(m.State.Ref.MessageID), Sender: strings.ToLower(m.State.Metadata.Sender.Address)})
	}
	return out, err
}
func (s *boundFixture) CreatePlan(ctx context.Context, scope string, items []store.PlanItem) (int64, error) {
	return s.CreateBoundPlan(ctx, scope, items)
}
func (s *boundFixture) SetBody(ctx context.Context, n int64, body string) error {
	return s.AccountStore.SetBody(ctx, mailbox.LocalRef{AccountID: s.Account().ID, N: n}, body)
}
func (s *boundFixture) LogActions(ctx context.Context, actions []store.Action) error {
	var attempts []store.BoundAction
	for _, a := range actions {
		local := mailbox.LocalRef{AccountID: s.Account().ID, N: a.N}
		r, err := s.LoadReviewedAction(ctx, a.PlanID, local, s.Account())
		if err != nil {
			return err
		}
		result := r.Expected
		result.Memberships = []mailbox.Membership{{Collection: mailbox.CollectionRef{AccountID: s.Account().ID, CollectionID: mailbox.CollectionID("F-" + a.Destination)}}}
		attempts = append(attempts, store.BoundAction{PlanID: a.PlanID, Local: local, Expected: r.Expected, Result: &result, Action: a.Action, Destination: a.Destination, Status: a.Status})
	}
	return s.RecordBoundActions(ctx, attempts)
}
