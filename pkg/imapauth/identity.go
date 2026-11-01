package imapauth

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/netip"
	"strconv"
	"strings"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// Service isolates versioned IMAP credentials from Graph and model items.
const Service = "mailpipe-imap-v1"

// Principal preserves login bytes and gives one canonical implicit-TLS endpoint.
type Principal struct{ Provider, Endpoint, Login string }

func printable(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for i := range s {
		if s[i] < 32 || s[i] > 126 {
			return false
		}
	}
	return true
}

// NewPrincipal rejects unsupported endpoint ambiguity without DNS resolution.
func NewPrincipal(provider, endpoint, login string) (Principal, error) {
	bad := errors.New("IMAP setup: invalid provider, TLS endpoint or login encoding")
	if provider == "" || !printable(login, 254) {
		return Principal{}, bad
	}
	for i := range endpoint {
		if endpoint[i] < 33 || endpoint[i] > 126 {
			return Principal{}, bad
		}
	}
	for _, c := range provider {
		if c < 'a' || c > 'z' {
			return Principal{}, bad
		}
	}
	if endpoint == "" || strings.ContainsAny(endpoint, " /\\@%?#[]\t\r\n") || strings.Count(endpoint, ":") > 1 {
		return Principal{}, bad
	}
	host, port, hasPort := strings.Cut(endpoint, ":")
	if !hasPort {
		port = "993"
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return Principal{}, bad
	}
	host = strings.ToLower(host)
	if ip, e := netip.ParseAddr(host); e == nil {
		if !ip.Is4() || ip.String() != host {
			return Principal{}, bad
		}
	} else {
		if len(host) > 253 || !strings.Contains(host, ".") {
			return Principal{}, bad
		}
		labels := strings.Split(host, ".")
		letter := false
		for _, c := range labels[len(labels)-1] {
			letter = letter || c >= 'a' && c <= 'z'
		}
		if !letter {
			return Principal{}, bad
		}
		for _, label := range labels {
			if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "xn--") || label[0] == '-' || label[len(label)-1] == '-' {
				return Principal{}, bad
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return Principal{}, bad
				}
			}
		}
	}
	return Principal{provider, host + ":" + port, login}, nil
}

func (p Principal) tuple() string {
	b := append([]byte("mailpipe-imap-principal\x00"), 0, 0, 0, 1)
	for _, v := range []string{p.Provider, p.Endpoint, p.Login} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(v)))
		b = append(b, v...)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func (p Principal) Account() mailbox.Account {
	return mailbox.Account{ID: mailbox.AccountID("imap-principal-v1:" + p.tuple()), ProviderID: mailbox.ProviderID(p.Provider)}
}
func (p Principal) Item() string { return "principal-v1." + p.tuple() }
func (p Principal) Host() string { h, _, _ := strings.Cut(p.Endpoint, ":"); return h }
