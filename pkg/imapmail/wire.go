package imapmail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	imap "github.com/emersion/go-imap"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

var errWireLimit = errors.New("IMAP wire resource limit")

type protocolFault struct {
	code      string
	transient bool
	hint      time.Duration
	window    int
}

func (e *protocolFault) Error() string { return "IMAP protocol: " + e.code }
func fault(code string, transient bool) error {
	return &protocolFault{code: code, transient: transient}
}

// budgetReader implements ALL reader paths used by the codec. bufio read-ahead
// is bounded independently; parser-visible bytes cannot bypass per-response and
// command budgets via ReadString or ReadRune. Literal allocation has its own cap.
type budgetReader struct {
	r                 *bufio.Reader
	response, command int
	last              int
}

func (b *budgetReader) charge(n int) error {
	b.response += n
	b.command += n
	if b.response > 128<<10 || b.command > 8<<20 {
		return errWireLimit
	}
	return nil
}
func (b *budgetReader) Read(p []byte) (int, error) {
	limit := min((128<<10)-b.response, (8<<20)-b.command)
	if limit <= 0 {
		return 0, errWireLimit
	}
	if len(p) > limit {
		p = p[:limit]
	}
	n, e := b.r.Read(p)
	b.last = 0
	if f := b.charge(n); f != nil {
		return n, f
	}
	return n, e
}
func (b *budgetReader) ReadRune() (rune, int, error) {
	r, n, e := b.r.ReadRune()
	b.last = n
	if e != nil {
		return r, n, e
	}
	if e = b.charge(n); e != nil {
		return 0, 0, e
	}
	return r, n, nil
}
func (b *budgetReader) UnreadRune() error {
	if e := b.r.UnreadRune(); e != nil {
		return e
	}
	b.response -= b.last
	b.command -= b.last
	b.last = 0
	return nil
}
func (b *budgetReader) ReadString(delim byte) (string, error) {
	var s strings.Builder
	for {
		r, _, e := b.ReadRune()
		if e != nil {
			return s.String(), e
		}
		s.WriteRune(r)
		if r == rune(delim) {
			return s.String(), nil
		}
	}
}

type session struct {
	conn             net.Conn
	budget           *budgetReader
	reader           *imap.Reader
	ctx              context.Context
	cancel           context.CancelFunc
	stop             func() bool
	done             chan struct{}
	callbackCloseErr error
	once             sync.Once
	caps             Capabilities
	tag              uint64
	dirty            bool
	uidOnly          bool
	limit            int
}

func (s *session) close() error {
	var e error
	s.once.Do(func() {
		s.cancel()
		e = s.conn.Close()
		if errors.Is(e, net.ErrClosed) {
			e = nil
		}
		if !s.stop() {
			<-s.done
			e = errors.Join(e, s.callbackCloseErr)
		}
	})
	return e
}
func (s *session) read() (imap.Resp, error) {
	s.budget.response = 0
	r, e := imap.ReadResp(s.reader)
	if e != nil {
		if s.ctx.Err() != nil {
			return nil, s.ctx.Err()
		}
		if errors.Is(e, errWireLimit) {
			return nil, fault("response limit exceeded", false)
		}
		if imap.IsParseError(e) {
			return nil, fault("malformed response", false)
		}
		var ne net.Error
		return nil, fault("transport interrupted", errors.Is(e, io.EOF) || errors.Is(e, io.ErrUnexpectedEOF) || errors.As(e, &ne))
	}
	return r, nil
}
func (s *session) deadline(ctx context.Context) error {
	d := time.Now().Add(30 * time.Second)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(d) {
		d = deadline
	}
	return s.conn.SetDeadline(d)
}
func (s *session) command(name string, args []any, consume func(imap.Resp) error) error {
	switch name {
	case "LOGIN", "CAPABILITY", "LIST", "ENABLE", "EXAMINE", "UID FETCH":
	default:
		return fault("command not permitted", false)
	}
	if e := s.ctx.Err(); e != nil {
		return e
	}
	if e := s.deadline(s.ctx); e != nil {
		return fault("transport deadline failed", false)
	}
	s.tag++
	tag := "A" + strconv.FormatUint(s.tag, 10)
	var encoded bytes.Buffer
	if e := (&imap.Command{Tag: tag, Name: name, Arguments: args}).WriteTo(imap.NewWriter(&encoded)); e != nil {
		return fault("command encoding failed", false)
	}
	if encoded.Len() > 64<<10 {
		return fault("command limit exceeded", false)
	}
	s.budget.command = 0
	if _, e := io.Copy(s.conn, &encoded); e != nil {
		return fault("transport write interrupted", true)
	}
	for {
		r, e := s.read()
		if e != nil {
			return e
		}
		switch r := r.(type) {
		case *imap.StatusResp:
			r.Code = imap.StatusRespCode(strings.ToUpper(string(r.Code)))
			if r.Code == "MESSAGELIMIT" {
				if r.Tag != tag || (r.Type != imap.StatusRespOk && r.Type != imap.StatusRespNo) {
					return fault("invalid message-limit response status", false)
				}
				if len(r.Arguments) != 2 {
					return fault("invalid message-limit response", false)
				}
				n, e := imap.ParseNumber(r.Arguments[0])
				last, e2 := imap.ParseNumber(r.Arguments[1])
				if e != nil || e2 != nil || n == 0 || last == 0 {
					return fault("invalid message-limit response", false)
				}
				if uint64(n) < uint64(s.limit) {
					return &protocolFault{code: "smaller message window required", transient: true, window: int(n)}
				}
				return fault("partial message-limit response", false)
			}
			if r.Code == "UIDNOTSTICKY" {
				return fault("persistent UID identity unavailable", false)
			}
			if r.Type == imap.StatusRespBye {
				return fault("server disconnected", len(r.Arguments) == 0 && (r.Code == "UNAVAILABLE" || r.Code == "THROTTLED" || r.Code == ""))
			}
			if r.Tag == tag {
				if r.Type != imap.StatusRespOk {
					return fault("command refused", r.Type == imap.StatusRespNo && len(r.Arguments) == 0 && (r.Code == "UNAVAILABLE" || r.Code == "THROTTLED"))
				}
				if consume != nil {
					return consume(r)
				}
				return nil
			}
			if r.Tag != "*" {
				return fault("unexpected tagged response", false)
			}
		case *imap.ContinuationReq:
			return fault("unexpected continuation", false)
		case *imap.DataResp:
			if r.Tag != "*" {
				return fault("unexpected response tag", false)
			}
		}
		if consume != nil {
			if e = consume(r); e != nil {
				return e
			}
		}
	}
}
func (c *Client) connect(ctx context.Context) (*session, error) {
	creds, e := c.credentials(ctx)
	if e != nil {
		temporary, typed := e.(*TemporaryCredentialError)
		if typed && temporary.RetryAfter >= 0 && temporary.RetryAfter <= 30*time.Second {
			return nil, &protocolFault{code: "credentials temporarily unavailable", transient: true, hint: temporary.RetryAfter}
		}
		return nil, failure("credentials unavailable")
	}
	if creds.Account != c.Account() {
		return nil, safe("credential account changed", mailbox.ErrAccountMismatch)
	}
	for _, v := range []string{creds.Login, creds.Password} {
		if !validText(v, 8192) {
			return nil, failure("invalid credential encoding")
		}
		for _, r := range v {
			if r < 32 || r > 126 {
				return nil, failure("unsupported credential encoding")
			}
		}
	}
	if c.login != "" && c.login != creds.Login {
		return nil, failure("credential login changed")
	}
	c.login = creds.Login
	host, _, e := net.SplitHostPort(c.cfg.Address)
	if e != nil {
		return nil, failure("invalid TLS endpoint")
	}
	name := c.cfg.ServerName
	if name == "" {
		name = host
	}
	if !validText(name, 253) {
		return nil, failure("invalid TLS server name")
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDial()
	raw, e := (&net.Dialer{}).DialContext(dialCtx, "tcp", c.cfg.Address)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fault("connection unavailable", true)
	}
	child, cancel := context.WithCancel(ctx)
	conn := tls.Client(raw, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: name, RootCAs: c.cfg.Roots})
	s := &session{conn: conn, ctx: child, cancel: cancel, done: make(chan struct{}), caps: Capabilities{}, limit: c.cfg.UIDWindow}
	s.stop = context.AfterFunc(child, func() {
		s.callbackCloseErr = s.conn.Close()
		if errors.Is(s.callbackCloseErr, net.ErrClosed) {
			s.callbackCloseErr = nil
		}
		close(s.done)
	})
	fail := func(e error) (*session, error) { return nil, failedSession(s, e) }
	if e = conn.HandshakeContext(dialCtx); e != nil {
		return fail(safe("TLS verification or handshake failed", ctx.Err()))
	}
	s.budget = &budgetReader{r: bufio.NewReaderSize(conn, 4096)}
	s.reader = imap.NewReader(s.budget)
	s.reader.MaxLiteralSize = 64 << 10
	_ = s.deadline(child)
	greeting, e := s.read()
	if e != nil {
		return fail(e)
	}
	status, ok := greeting.(*imap.StatusResp)
	if !ok || status.Type != imap.StatusRespOk || status.Tag != "*" {
		return fail(fault("authenticated greeting required", false))
	}
	// Fresh CAPABILITY before authentication checks LOGINDISABLED. Only the next,
	// post-auth command establishes authenticated features.
	if e = s.capability(); e != nil {
		return fail(e)
	}
	if s.caps.Has("LOGINDISABLED") {
		return fail(fault("LOGIN unavailable; compatible authentication required", false))
	}
	if e = s.command("LOGIN", []any{creds.Login, creds.Password}, nil); e != nil {
		var f *protocolFault
		if errors.As(e, &f) {
			f.transient = false
		}
		return fail(e)
	}
	if e = s.capability(); e != nil {
		return fail(e)
	}
	if c.cfg.Profile == Yahoo {
		if !s.caps.Has("ENABLE") || !s.caps.Has("UIDONLY") || !s.caps.Has("CONDSTORE") {
			return fail(fault("Yahoo full view requires ENABLE UIDONLY and CONDSTORE", false))
		}
		confirmed := false
		e = s.command("ENABLE", []any{imap.RawString("UIDONLY")}, func(r imap.Resp) error {
			if d, ok := r.(*imap.DataResp); ok && len(d.Fields) > 1 && atom(d.Fields[0]) == "ENABLED" {
				for _, f := range d.Fields[1:] {
					if atom(f) == "UIDONLY" {
						confirmed = true
					}
				}
			}
			return nil
		})
		if e != nil {
			return fail(e)
		}
		if !confirmed {
			return fail(fault("Yahoo full view not acknowledged", false))
		}
		s.uidOnly = true
	}
	if s.limit > 1000 {
		s.limit = 1000
	}
	return s, nil
}
func (s *session) capability() error {
	caps := Capabilities{}
	seen := false
	e := s.command("CAPABILITY", nil, func(r imap.Resp) error {
		if d, ok := r.(*imap.DataResp); ok && len(d.Fields) > 0 && atom(d.Fields[0]) == "CAPABILITY" {
			seen = true
			for _, f := range d.Fields[1:] {
				v := atom(f)
				if v == "" {
					return fault("invalid capability", false)
				}
				caps[v] = true
				if strings.HasPrefix(v, "MESSAGELIMIT=") {
					n, e := strconv.Atoi(strings.TrimPrefix(v, "MESSAGELIMIT="))
					if e != nil || n <= 0 {
						return fault("invalid message limit", false)
					}
					s.limit = min(s.limit, n)
				}
			}
		}
		return nil
	})
	if e != nil {
		return e
	}
	if !seen {
		return fault("missing capability evidence", false)
	}
	s.caps = caps
	return nil
}
func atom(v any) string { s, _ := v.(string); return strings.ToUpper(s) }

// A preparation transport error grants no retry authority if its owned socket
// cleanup is uncertain. Only a clean preparation retains the protocol marker.
func failedSession(s *session, original error) error {
	if e := s.close(); e != nil {
		return safe("connection cleanup failed; reconcile before retry", errors.Join(original, e))
	}
	return original
}
