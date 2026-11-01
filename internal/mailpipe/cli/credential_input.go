package cli

import (
	"context"
	"errors"
	"io"
)

func credentialError(ctx context.Context, what string) error {
	return errors.Join(errors.New("IMAP credentials: "+what), ctx.Err())
}

func credentialLine(ctx context.Context, r io.Reader, terminal bool) (value []byte, err error) {
	b := make([]byte, 0, 258)
	defer func() {
		if err != nil {
			clear(b[:cap(b)])
		}
	}()
	var one [1]byte
	ended := false
	for {
		if ctx.Err() != nil {
			return nil, credentialError(ctx, "input interrupted")
		}
		n, e := r.Read(one[:])
		if n > 0 {
			if ended {
				return nil, credentialError(ctx, "exactly one input line required")
			}
			b = append(b, one[0])
			if len(b) > 258 {
				return nil, credentialError(ctx, "input exceeds limit")
			}
			if one[0] == '\n' {
				ended = true
				if terminal {
					break
				}
			}
		}
		if e != nil {
			if e == io.EOF {
				break
			}
			return nil, credentialError(ctx, "input read failed")
		}
		if n == 0 {
			return nil, credentialError(ctx, "input read failed")
		}
	}
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
		if len(b) > 0 && b[len(b)-1] == '\r' {
			b = b[:len(b)-1]
		}
	}
	if len(b) == 0 || len(b) > 256 {
		return nil, credentialError(ctx, "nonempty bounded app password required")
	}
	for _, c := range b {
		if c < 32 || c > 126 {
			return nil, credentialError(ctx, "unsupported input encoding")
		}
	}
	return b, nil
}
