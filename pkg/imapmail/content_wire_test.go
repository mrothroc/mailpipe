package imapmail

import (
	"bufio"
	imap "github.com/emersion/go-imap"
	"strings"
	"testing"
)

func TestContentWireNestingBeforeCodecAllocation(t *testing.T) {
	for _, test := range []struct {
		name, input string
		fail        bool
	}{
		{"deep", "* 1 FETCH " + strings.Repeat("(", 129) + strings.Repeat(")", 129) + "\r\n", true},
		{"quoted", `* 1 FETCH (UID 1 X "` + strings.Repeat("(", 500) + `\\\"quoted")` + "\r\n", false},
		{"literal", "* 1 FETCH (UID 1 X {512}\r\n" + strings.Repeat("(", 512) + ")\r\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := &budgetReader{r: bufio.NewReader(strings.NewReader(test.input))}
			r := imap.NewReader(b)
			r.MaxLiteralSize = 65536
			_, e := imap.ReadResp(r)
			if (e != nil) != test.fail {
				t.Fatalf("error=%v", e)
			}
		})
	}
}
