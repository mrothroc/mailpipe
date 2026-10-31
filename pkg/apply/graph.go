package apply

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/mrothroc/mailpipe/pkg/graph"
)

// Graph is the part of the Graph client the mailbox uses.
type Graph interface {
	Batch(ctx context.Context, reqs []graph.BatchRequest) ([]graph.BatchResponse, error)
	Get(ctx context.Context, path string, query url.Values, out any) error
}

// GraphMailbox is a Mailbox on Microsoft Graph, using immutable ids.
type GraphMailbox struct{ g Graph }

// NewGraphMailbox returns a Mailbox over g.
func NewGraphMailbox(g Graph) *GraphMailbox { return &GraphMailbox{g: g} }

const lookupSelect = "id,internetMessageId,from,sender,parentFolderId"

// FolderID returns the id of a folder, by well-known name or id.
func (m *GraphMailbox) FolderID(ctx context.Context, name string) (string, error) {
	var f struct {
		ID string `json:"id"`
	}
	if err := m.g.Get(ctx, "/me/mailFolders/"+url.PathEscape(name), url.Values{"$select": {"id"}}, &f); err != nil {
		return "", err
	}
	if f.ID == "" {
		return "", fmt.Errorf("folder %s has no id", name)
	}
	return f.ID, nil
}

// batches sends one request per id, MaxBatch at a time, returning the
// responses in id order.
func (m *GraphMailbox) batches(ctx context.Context, ids []string, build func(id string) graph.BatchRequest) ([]graph.BatchResponse, error) {
	out := make([]graph.BatchResponse, 0, len(ids))
	for start := 0; start < len(ids); start += graph.MaxBatch {
		chunk := ids[start:min(start+graph.MaxBatch, len(ids))]
		reqs := make([]graph.BatchRequest, len(chunk))
		for i, id := range chunk {
			reqs[i] = build(id)
			reqs[i].ID = strconv.Itoa(i + 1)
		}
		resps, err := m.g.Batch(ctx, reqs)
		if err != nil {
			return nil, err
		}
		out = append(out, resps...)
	}
	return out, nil
}

type address struct {
	EmailAddress struct {
		Address string `json:"address"`
	} `json:"emailAddress"`
}

// Lookup reads each message's Message-ID, sender and folder. The sender is
// From, falling back to Sender, as in the mirror.
func (m *GraphMailbox) Lookup(ctx context.Context, ids []string) ([]Found, error) {
	resps, err := m.batches(ctx, ids, func(id string) graph.BatchRequest {
		return graph.BatchRequest{Method: http.MethodGet, URL: "/me/messages/" + url.PathEscape(id) + "?" + url.Values{"$select": {lookupSelect}}.Encode()}
	})
	if err != nil {
		return nil, err
	}
	out := make([]Found, len(resps))
	for i, r := range resps {
		if r.Status == http.StatusNotFound {
			out[i].Missing = true
			continue
		}
		if err := r.Err(); err != nil {
			out[i].Err = err
			continue
		}
		var msg struct {
			InternetMessageID string   `json:"internetMessageId"`
			From              *address `json:"from"`
			Sender            *address `json:"sender"`
			ParentFolderID    string   `json:"parentFolderId"`
		}
		if err := json.Unmarshal(r.Body, &msg); err != nil {
			out[i].Err = fmt.Errorf("decode message: %w", err)
			continue
		}
		from := msg.From
		if from == nil {
			from = msg.Sender
		}
		out[i].Remote = Remote{InternetMessageID: msg.InternetMessageID, ParentFolderID: msg.ParentFolderID}
		if from != nil {
			out[i].Remote.Sender = from.EmailAddress.Address
		}
	}
	return out, nil
}

// Move moves each message to destination (a well-known name or folder id).
func (m *GraphMailbox) Move(ctx context.Context, ids []string, destination string) ([]error, error) {
	resps, err := m.batches(ctx, ids, func(id string) graph.BatchRequest {
		return graph.BatchRequest{Method: http.MethodPost, URL: "/me/messages/" + url.PathEscape(id) + "/move",
			Body: map[string]string{"destinationId": destination}}
	})
	if err != nil {
		return nil, err
	}
	out := make([]error, len(resps))
	for i, r := range resps {
		out[i] = r.Err()
	}
	return out, nil
}

// ChunkSize keeps Graph batch-size knowledge in this explicit adapter.
func (m *GraphMailbox) ChunkSize() int { return graph.MaxBatch }
