//go:build !darwin

package cli

import (
	"context"
	"io"
)

func readCredentialInput(ctx context.Context, _ io.Reader, _ io.Writer, _ bool) ([]byte, error) {
	return nil, credentialError(ctx, "interactive credential input requires macOS")
}
