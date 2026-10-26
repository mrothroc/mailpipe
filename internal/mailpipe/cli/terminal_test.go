package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestTerminalWriterPreservesSplitUnicodeAndEscapesControls(t *testing.T) {
	input := []byte("Hello 世界 🌍\n\t\x1b[31m\u009b\u202e\r\a\xff\xe2")
	for cut := 0; cut <= len(input); cut++ {
		var out bytes.Buffer
		w := &terminalWriter{out: &out}
		for _, chunk := range [][]byte{input[:cut], input[cut:]} {
			if _, err := w.Write(chunk); err != nil {
				t.Fatal(err)
			}
		}
		w.flush()
		if got, want := out.String(), "Hello 世界 🌍\n\t\\x1b[31m\\u009b\\u202e\\r\\a\\xff\\xe2"; got != want {
			t.Fatalf("split %d: got %q want %q", cut, got, want)
		}
	}
}

type shortTerminalSink struct{}

func (shortTerminalSink) Write(p []byte) (int, error) { return len(p) - 1, nil }
func TestTerminalWriterReportsShortWrites(t *testing.T) {
	w := &terminalWriter{out: shortTerminalSink{}}
	if _, err := w.Write([]byte("message")); err != io.ErrShortWrite {
		t.Fatalf("err=%v", err)
	}
}

func TestFilingRootContainsConcurrentSymlinkSwaps(t *testing.T) {
	path, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(path, "inside"), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, target := range []string{outside, "inside"} {
				_ = os.Remove(filepath.Join(path, "link"))
				_ = os.Symlink(target, filepath.Join(path, "link"))
			}
		}
	}()
	for i := 0; i < 50; i++ {
		_, _ = writeNew(root, fmt.Sprintf("link/new/%d.pdf", i), []byte("%PDF-synthetic"))
	}
	close(stop)
	wg.Wait()
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("escaped root during swap: entries=%v err=%v", entries, err)
	}
}
