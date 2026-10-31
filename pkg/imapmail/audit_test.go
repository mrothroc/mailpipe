package imapmail

import "testing"

// Audit actual commands read by the TLS server. Arguments and credentials are
// never retained in the transcript. Every unrecognized/mutating verb fails.
func (s *scriptServer) audit(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	forbidden := 0
	for _, cmd := range s.commands {
		counts[cmd]++
		switch cmd {
		case "LOGIN", "CAPABILITY", "LIST", "ENABLE", "EXAMINE", "UID FETCH":
		default:
			forbidden++
			t.Errorf("mutation/unknown command: %s", cmd)
		}
	}
	t.Logf("read-only wire transcript audit: command_counts=%v mutation_or_unknown_count=%d", counts, forbidden)
}
