package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCredentialTerminalRestoresAndCancels(t *testing.T) {
	for _, scenario := range []string{"success", "empty", "cancel", "prompt", "restore", "disable"} {
		t.Run(scenario, func(t *testing.T) {
			r, w, e := os.Pipe()
			if e != nil {
				t.Fatal(e)
			}
			defer r.Close()
			defer w.Close()
			original := unix.Termios{Lflag: unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG, Iflag: 123}
			var states []unix.Termios
			ops := terminalOps{get: func(int) (*unix.Termios, error) { c := original; return &c, nil }, set: func(_ int, c *unix.Termios) error {
				states = append(states, *c)
				if scenario == "disable" && len(states) == 1 || scenario == "restore" && len(states) == 2 {
					return errors.New("terminal error")
				}
				return nil
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if scenario != "cancel" {
				input := "line\n"
				if scenario == "empty" {
					input = ""
				}
				w.WriteString(input)
				w.Close()
			}
			var output io.Writer = io.Discard
			if scenario == "prompt" {
				output = credentialBrokenWriter{}
			}
			b, e := readCredentialFD(ctx, r, output, false, ops)
			if scenario == "success" {
				if e != nil || string(b) != "line" {
					t.Fatal("terminal read failed")
				}
			} else if e == nil {
				t.Fatal("terminal failure succeeded")
			}
			if len(states) < 1 || states[0].Lflag&(unix.ECHO|unix.ECHONL) != 0 {
				t.Fatal("echo was not disabled")
			}
			if len(states) != 2 || !reflect.DeepEqual(states[1], original) {
				t.Fatal("exact terminal state not restored")
			}
			if _, statErr := r.Stat(); statErr != nil {
				t.Fatal("borrowed stdin closed")
			}
			if scenario == "cancel" && !errors.Is(e, context.DeadlineExceeded) {
				t.Fatal("cancellation not preserved")
			}
		})
	}
	r, w, _ := os.Pipe()
	defer r.Close()
	w.WriteString("line")
	w.Close()
	if b, e := readCredentialInput(context.Background(), r, io.Discard, true); e != nil || string(b) != "line" {
		t.Fatal("real pipe EOF input failed")
	}
	if _, e := readCredentialInput(context.Background(), strings.NewReader("line"), io.Discard, true); e == nil {
		t.Fatal("unowned blocking reader admitted")
	}
}

type credentialBrokenWriter struct{}

func (credentialBrokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
