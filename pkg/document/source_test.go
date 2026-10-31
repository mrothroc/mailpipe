package document_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/document"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

type mimeFixture struct {
	calls    int
	fail     error
	overflow bool
	exact    bool
}

func (f *mimeFixture) Account() mailbox.Account {
	return mailbox.Account{ID: "fixture", ProviderID: "fixture"}
}
func (f *mimeFixture) WriteMIME(ctx context.Context, ref mailbox.MessageRef, w io.Writer) error {
	f.calls++
	if f.overflow || f.exact {
		chunk := make([]byte, 32768)
		chunk[len(chunk)-1] = 'Z'
		end := document.MaxSourceBytes
		if f.overflow {
			end += int64(len(chunk))
		}
		for n := int64(0); n < end; n += int64(len(chunk)) {
			_, _ = w.Write(chunk)
		}
		return nil
	}
	_, err := io.WriteString(w, "complete tail")
	return errors.Join(err, f.fail)
}
func TestReadSourceCompleteOrError(t *testing.T) {
	ref := mailbox.MessageRef{AccountID: "fixture", MessageID: "one", Locator: "one"}
	f := &mimeFixture{}
	b, err := document.ReadSource(context.Background(), f, ref)
	if err != nil || string(b) != "complete tail" || f.calls != 1 {
		t.Fatalf("%q %v calls %d", b, err, f.calls)
	}
	f.fail = io.ErrUnexpectedEOF
	if b, err = document.ReadSource(context.Background(), f, ref); b != nil || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial: %q %v", b, err)
	}
	f.overflow = true
	if b, err = document.ReadSource(context.Background(), f, ref); b != nil || err == nil {
		t.Fatal("ignored collector overflow")
	}
	ref.AccountID = "other"
	before := f.calls
	if _, err = document.ReadSource(context.Background(), f, ref); err == nil || f.calls != before {
		t.Fatal("cross-account fetch")
	}
}

func TestReadSourceExactLimitAndCancellation(t *testing.T) {
	ref := mailbox.MessageRef{AccountID: "fixture", MessageID: "one", Locator: "one"}
	f := &mimeFixture{exact: true}
	b, err := document.ReadSource(context.Background(), f, ref)
	if err != nil || int64(len(b)) != document.MaxSourceBytes || b[len(b)-1] != 'Z' {
		t.Fatalf("complete limit/tail %d %v", len(b), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := f.calls
	if b, err = document.ReadSource(ctx, f, ref); b != nil || !errors.Is(err, context.Canceled) || f.calls != before {
		t.Fatal("canceled source invoked", err)
	}
}
