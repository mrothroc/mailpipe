package document

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// MaxSourceBytes bounds complete input collected before MIME parsing. This is
// not a parser allocation bound; adapters independently bound copy scratch.
const MaxSourceBytes int64 = 64 << 20

// MIMEReader is the complete-source subset of mailbox.Provider.
type MIMEReader interface {
	Account() mailbox.Account
	WriteMIME(context.Context, mailbox.MessageRef, io.Writer) error
}

type sourceCollector struct {
	ctx context.Context
	bytes.Buffer
	err error
}

func (w *sourceCollector) Write(b []byte) (int, error) {
	if w.err == nil {
		w.err = w.ctx.Err()
	}
	if w.err == nil && int64(len(b)) > MaxSourceBytes-int64(w.Len()) {
		w.err = fmt.Errorf("read MIME: message exceeds %d-byte content limit; incomplete content was discarded", MaxSourceBytes)
	}
	if w.err != nil {
		return 0, w.err
	}
	return w.Buffer.Write(b)
}

// ReadSource fetches once and returns complete bounded bytes or nil on error.
// The collector buffers the source (up to 64 MiB), while transport copy scratch
// is separately limited to 32 KiB. It neither closes nor retries the reader.
func ReadSource(ctx context.Context, src MIMEReader, ref mailbox.MessageRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if src == nil {
		return nil, errors.New("read MIME: source is required")
	}
	if err := src.Account().Validate(); err != nil {
		return nil, err
	}
	if err := ref.Validate(src.Account().ID); err != nil {
		return nil, err
	}
	w := &sourceCollector{ctx: ctx}
	err := src.WriteMIME(ctx, ref, w)
	if err = errors.Join(err, w.err, ctx.Err()); err != nil {
		return nil, err
	}
	return w.Bytes(), nil
}
