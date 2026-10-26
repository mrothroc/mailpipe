// Package mailsync mirrors mail folders into the store with Graph delta
// queries: a complete pass the first time, only changes after that.
package mailsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/store"
)

// Select lists the message fields requested from Graph.
var Select = []string{"id", "internetMessageId", "subject", "from", "sender", "toRecipients", "ccRecipients",
	"receivedDateTime", "hasAttachments", "isRead", "bodyPreview"}

// DefaultFolders are the well-known folders synced when none are named.
// Sent Items feeds the allowlist stage (everyone the owner has written to).
var DefaultFolders = []string{"inbox", "deleteditems", "sentitems"}

// Source is the part of the Graph client a sync needs.
type Source interface {
	Pages(ctx context.Context, start string, query url.Values, fn func(graph.Page) error) error
	FolderTotal(ctx context.Context, folder string) (int, error)
	Get(ctx context.Context, path string, query url.Values, out any) error
}

// Result summarizes one folder's sync; Local and Remote should agree.
type Result struct {
	Folder  string
	Added   int
	Updated int
	Removed int
	Local   int
	Remote  int
}

type address struct {
	EmailAddress struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

type message struct {
	ID                string          `json:"id"`
	InternetMessageID string          `json:"internetMessageId"`
	Subject           string          `json:"subject"`
	From              *address        `json:"from"`
	Sender            *address        `json:"sender"`
	ToRecipients      []address       `json:"toRecipients"`
	CcRecipients      []address       `json:"ccRecipients"`
	ReceivedDateTime  string          `json:"receivedDateTime"`
	HasAttachments    bool            `json:"hasAttachments"`
	IsRead            bool            `json:"isRead"`
	BodyPreview       string          `json:"bodyPreview"`
	Removed           json.RawMessage `json:"@removed"`
}

func (m message) record() store.Record {
	from := m.From
	if from == nil {
		from = m.Sender
	}
	r := store.Record{GraphID: m.ID, InternetMessageID: m.InternetMessageID, Received: m.ReceivedDateTime,
		Subject: m.Subject, Preview: m.BodyPreview, HasAttachments: m.HasAttachments, IsRead: m.IsRead}
	if from != nil {
		r.Sender, r.SenderName = from.EmailAddress.Address, from.EmailAddress.Name
	}
	for _, a := range append(append([]address{}, m.ToRecipients...), m.CcRecipients...) {
		r.Recipients = append(r.Recipients, a.EmailAddress.Address)
	}
	return r
}

// decode reads one delta record. An incremental delta can report a changed
// message with only some properties (every full record has its received
// time); storing that would blank the rest, so the full message is fetched
// instead. gone reports a message deleted before it could be fetched.
func decode(ctx context.Context, src Source, raw json.RawMessage) (m message, gone bool, err error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return m, false, fmt.Errorf("decode message: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, false, fmt.Errorf("decode message: %w", err)
	}
	if _, full := keys["receivedDateTime"]; full || len(m.Removed) > 0 {
		return m, false, nil
	}
	id := m.ID
	err = src.Get(ctx, "/me/messages/"+url.PathEscape(id), url.Values{"$select": {strings.Join(Select, ",")}}, &m)
	var apiErr *graph.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return message{ID: id}, true, nil
	}
	if err != nil {
		return m, false, fmt.Errorf("fetch changed message %s: %w", id, err)
	}
	m.ID = id
	return m, false, nil
}

// SyncFolder brings folder up to date. With full, saved progress is ignored and
// the whole folder is re-read; a complete pass also drops messages Graph no
// longer reports. The delta link is saved only after a pass completes, so an
// interrupted sync simply starts over.
func SyncFolder(ctx context.Context, s *store.Store, src Source, folder string, full bool) (Result, error) {
	r := Result{Folder: folder}
	link := ""
	if !full {
		var err error
		if link, err = s.DeltaLink(ctx, folder); err != nil {
			return r, err
		}
	}
	start, query := link, url.Values(nil)
	if link == "" {
		start = "/me/mailFolders/" + url.PathEscape(folder) + "/messages/delta"
		query = url.Values{"$select": {strings.Join(Select, ",")}}
	}
	seen := map[string]bool{}
	delta := ""
	err := src.Pages(ctx, start, query, func(p graph.Page) error {
		for _, raw := range p.Value {
			m, gone, err := decode(ctx, src, raw)
			if err != nil {
				return err
			}
			if gone || len(m.Removed) > 0 {
				gone, err := s.Remove(ctx, folder, m.ID)
				if err != nil {
					return err
				}
				if gone {
					r.Removed++
				}
				continue
			}
			seen[m.ID] = true
			change, err := s.Upsert(ctx, folder, m.record())
			if err != nil {
				return err
			}
			if change == store.Added {
				r.Added++
			} else {
				r.Updated++
			}
		}
		if p.DeltaLink != "" {
			delta = p.DeltaLink
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	if link == "" {
		n, err := s.RemoveUnseen(ctx, folder, seen)
		if err != nil {
			return r, err
		}
		r.Removed += n
	}
	if delta != "" {
		if err := s.SetDeltaLink(ctx, folder, delta); err != nil {
			return r, err
		}
	}
	if r.Local, err = s.Count(ctx, store.Filter{Folder: folder}); err != nil {
		return r, err
	}
	r.Remote, err = src.FolderTotal(ctx, folder)
	return r, err
}
