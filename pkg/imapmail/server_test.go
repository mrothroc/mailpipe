package imapmail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

type scriptMessage struct {
	uid    uint32
	source string
	flags  string
}
type scriptServer struct {
	byeAtMetadata   int
	metadataFetches int
	watermarks      map[string]selected
	sourceReply     string
	folders         map[string][]scriptMessage
	fetchReply      string
	authFail        bool
	stallSource     bool

	mu          sync.Mutex
	listener    net.Listener
	conns       map[net.Conn]bool
	wg          sync.WaitGroup
	messages    []scriptMessage
	validity    uint32
	caps        string
	special     bool
	commands    []string
	selected    []string
	connections int
	byeOnce     bool
	stall       chan struct{}
	churn       bool
}

func localScript(t *testing.T) (*scriptServer, Config) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "imap.example.org"}, DNSNames: []string{"imap.example.org"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, e := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	parsed, e := x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	s := &scriptServer{listener: l, conns: map[net.Conn]bool{}, validity: 10, caps: "IMAP4rev1 ENABLE UIDONLY CONDSTORE SPECIAL-USE LIST-EXTENDED MOVE UIDPLUS OBJECTID", special: true, messages: []scriptMessage{{1, "From: sender@example.org\r\nSubject: invented\r\n\r\ninvented source", ""}, {3, "From: sender@example.org\r\nSubject: invented\r\n\r\ninvented source", `\Seen`}}}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, e := l.Accept()
			if e != nil {
				return
			}
			s.mu.Lock()
			s.conns[conn] = true
			s.connections++
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer conn.Close()
				defer func() { s.mu.Lock(); delete(s.conns, conn); s.mu.Unlock() }()
				c := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12})
				_ = c.SetDeadline(time.Now().Add(20 * time.Second))
				if e := c.HandshakeContext(context.Background()); e != nil {
					return
				}
				s.serve(c)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		s.mu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
		s.audit(t)
	})
	cfg := Config{Account: mailbox.Account{ID: "invented-account", ProviderID: "imap"}, Address: l.Addr().String(), ServerName: "imap.example.org", Roots: pool, Profile: Generic}
	return s, cfg
}

func (s *scriptServer) serve(c net.Conn) {
	r := bufio.NewReader(c)
	fmt.Fprint(c, "* OK [CAPABILITY IMAP4rev1] invented greeting\r\n")
	enabled := false
	authenticated := false
	selectedFolder := ""
	for {
		line, e := r.ReadString('\n')
		if e != nil {
			return
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return
		}
		tag, cmd := parts[0], strings.ToUpper(parts[1])
		s.mu.Lock()
		audit := cmd
		if cmd == "UID" && len(parts) > 2 {
			audit += " " + strings.ToUpper(parts[2])
		}
		s.commands = append(s.commands, audit)
		caps, special, validity := s.caps, s.special, s.validity
		messages := append([]scriptMessage(nil), s.messages...)
		if scoped, ok := s.folders[selectedFolder]; ok {
			messages = append([]scriptMessage(nil), scoped...)
		}
		if cmd == "UID" && !strings.Contains(line, "BODY.PEEK[]") {
			s.metadataFetches++
			if s.byeAtMetadata > 0 && s.metadataFetches == s.byeAtMetadata {
				s.byeOnce = true
			}
		}
		bye := s.byeOnce
		churn := s.churn
		stall := s.stall
		reply, authFail, stallSource := s.fetchReply, s.authFail, s.stallSource
		if strings.Contains(line, "BODY.PEEK[]") && s.sourceReply != "" {
			reply = s.sourceReply
		}
		s.mu.Unlock()
		switch cmd {
		case "LOGIN":
			if authFail {
				fmt.Fprintf(c, "%s NO [AUTHENTICATIONFAILED] invented-private-token\r\n", tag)
				return
			}
			authenticated = true
			fmt.Fprintf(c, "%s OK authenticated\r\n", tag)
		case "CAPABILITY":
			if !authenticated {
				caps = "IMAP4rev1 X-INVENTED-PREAUTH"
			}
			fmt.Fprintf(c, "* CAPABILITY %s\r\n%s OK capability\r\n", caps, tag)
		case "ENABLE":
			enabled = true
			fmt.Fprintf(c, "* ENABLED UIDONLY\r\n%s OK enabled\r\n", tag)
		case "LIST":
			for _, name := range []string{"INBOX", "Folder A", "Folder B", "Folder C"} {
				attr := ""
				if special {
					attr = map[string]string{"Folder A": `\Sent`, "Folder B": `\Trash`, "Folder C": `\Junk`}[name]
				}
				fmt.Fprintf(c, "* LIST (%s) \"/\" %q\r\n", attr, name)
			}
			fmt.Fprintf(c, "%s OK list\r\n", tag)
		case "EXAMINE":
			name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, tag+" "+parts[1]+" "), "\r\n"))
			name = strings.Trim(name, "\"")
			s.mu.Lock()
			s.selected = append(s.selected, name)
			selectedFolder = name
			if scoped, ok := s.folders[selectedFolder]; ok {
				messages = append([]scriptMessage(nil), scoped...)
			}
			s.mu.Unlock()
			next := uint32(1)
			for _, m := range messages {
				if m.uid >= next {
					next = m.uid + 1
				}
			}
			s.mu.Lock()
			if s.watermarks == nil {
				s.watermarks = map[string]selected{}
			}
			if old, ok := s.watermarks[name]; ok && old.validity == validity && old.next > next {
				next = old.next
			}
			s.watermarks[name] = selected{validity: validity, next: next, count: uint32(len(messages))}
			s.mu.Unlock()
			fmt.Fprintf(c, "* %d EXISTS\r\n* OK [UIDVALIDITY %d] validity\r\n* OK [UIDNEXT %d] next\r\n%s OK [READ-ONLY] selected\r\n", len(messages), validity, next, tag)
		case "UID":
			if len(parts) < 5 {
				return
			}
			if bye {
				s.mu.Lock()
				s.byeOnce = false
				s.mu.Unlock()
				fmt.Fprint(c, "* BYE [UNAVAILABLE] invented disconnect\r\n")
				return
			}
			if reply != "" {
				fmt.Fprint(c, strings.ReplaceAll(reply, "${tag}", tag))
				return
			}
			if stall != nil && (!stallSource || strings.Contains(line, "BODY.PEEK[]")) {
				select {
				case stall <- struct{}{}:
				default:
				}
				_, _ = io.Copy(io.Discard, r)
				return
			}
			rangePart := strings.Split(parts[3], ":")
			lo, _ := strconv.ParseUint(rangePart[0], 10, 32)
			hi := lo
			if len(rangePart) > 1 {
				hi, _ = strconv.ParseUint(rangePart[1], 10, 32)
			}
			source := strings.Contains(line, "BODY.PEEK[]")
			for index, m := range messages {
				if uint64(m.uid) < lo || uint64(m.uid) > hi {
					continue
				}
				kind := "FETCH"
				prefix := uint32(index + 1)
				if enabled {
					kind = "UIDFETCH"
					prefix = m.uid
				}
				if source {
					start, length := 0, 32768
					if pos := strings.Index(line, "<"); pos >= 0 {
						_, _ = fmt.Sscanf(line[pos:], "<%d.%d>", &start, &length)
					}
					if start > len(m.source) {
						return
					}
					end := min(start+length, len(m.source))
					chunk := m.source[start:end]
					fmt.Fprintf(c, "* %d %s (UID %d RFC822.SIZE %d BODY[]<%d> {%d}\r\n%s)\r\n", prefix, kind, m.uid, len(m.source), start, len(chunk), chunk)
				} else {
					fmt.Fprintf(c, "* %d %s (UID %d FLAGS (%s) INTERNALDATE \"01-Jan-2026 00:00:00 +0000\" RFC822.SIZE %d ENVELOPE (NIL \"invented\" NIL NIL NIL NIL NIL NIL NIL \"<duplicate@example.org>\"))\r\n", prefix, kind, m.uid, m.flags, len(m.source))
				}
			}
			if churn {
				fmt.Fprint(c, "* 99 EXISTS\r\n")
			}
			fmt.Fprintf(c, "%s OK fetched\r\n", tag)
		default:
			fmt.Fprintf(c, "%s BAD forbidden\r\n", tag)
			return
		}
	}
}
func inventedCredentials(account mailbox.Account) CredentialFunc {
	return func(context.Context) (Credentials, error) {
		return Credentials{Account: account, Login: "owner@example.com", Password: "invented-test-password"}, nil
	}
}
