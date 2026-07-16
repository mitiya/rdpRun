package main

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/mitiya/rdprun/cliprdr"
	"github.com/tomatome/grdp/core"
	"github.com/tomatome/grdp/glog"
	"github.com/tomatome/grdp/plugin"
	"github.com/tomatome/grdp/protocol/nla"
	"github.com/tomatome/grdp/protocol/pdu"
	"github.com/tomatome/grdp/protocol/sec"
	"github.com/tomatome/grdp/protocol/t125"
	"github.com/tomatome/grdp/protocol/tpkt"
	"github.com/tomatome/grdp/protocol/x224"
)

// rdpSession assembles the RDP stack manually (like grdp.go in the upstream
// package) so we keep access to pdu, channels and the clipboard client, which
// the high-level client.Client wrapper hides.
type rdpSession struct {
	host     string
	user     string
	password string
	domain   string
	width    int
	height   int
	auth     string

	tpkt     *tpkt.TPKT
	x224     *x224.X224
	mcs      *t125.MCSClient
	sec      *sec.Client
	pdu      *pdu.Client
	channels *plugin.Channels
	clip     *cliprdr.Client

	// ready is closed once the server signals the "ready" event (logged in,
	// desktop active and accepting input).
	readyCh chan struct{}
	// errCh receives the first error/close from the PDU layer.
	errCh chan error

	// bitmap accumulation (for UAC brightness detection)
	bmp *bitmapAccumulator
}

// connect dials the server and performs the RDP login. It blocks until either
// the session is "ready" (success) or an error occurs.
func newSession(cfg *Config) (*rdpSession, error) {
	domain, user := splitDomain(cfg.User)
	s := &rdpSession{
		host:     cfg.Server,
		user:     user,
		domain:   domain,
		password: cfg.Password,
		width:    cfg.Width,
		height:   cfg.Height,
		auth:     cfg.Auth,
		readyCh:  make(chan struct{}),
		errCh:    make(chan error, 4),
		bmp:      newBitmapAccumulator(cfg.Width, cfg.Height),
	}

	conn, err := net.DialTimeout("tcp", s.host, 6*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", s.host, err)
	}

	s.tpkt = tpkt.New(core.NewSocketLayer(conn), nla.NewNTLMv2(s.domain, s.user, s.password))
	s.x224 = x224.New(s.tpkt)
	s.mcs = t125.NewMCSClient(s.x224)
	s.sec = sec.NewClient(s.mcs)
	s.pdu = pdu.NewClient(s.sec)
	s.channels = plugin.NewChannels(s.sec)

	s.mcs.SetClientCoreData(uint16(s.width), uint16(s.height))

	s.sec.SetUser(s.user)
	s.sec.SetPwd(s.password)
	s.sec.SetDomain(s.domain)

	s.tpkt.SetFastPathListener(s.sec)
	s.sec.SetFastPathListener(s.pdu)
	s.sec.SetChannelSender(s.mcs)
	s.channels.SetChannelSender(s.sec)

	// Choose requested protocol based on --auth. NLA = HYBRID; standard = RDP.
	// "auto" tries NLA first (HYBRID lets the server fall back to standard).
	switch s.auth {
	case "standard":
		s.x224.SetRequestedProtocol(x224.PROTOCOL_RDP)
	case "nla":
		s.x224.SetRequestedProtocol(x224.PROTOCOL_HYBRID)
	default: // auto
		s.x224.SetRequestedProtocol(x224.PROTOCOL_HYBRID)
	}

	// Register the clipboard (cliprdr) static channel if we will need output
	// capture. Even when not capturing we can register safely.
	s.clip = cliprdr.NewClient()
	s.channels.Register(s.clip)

	// Wire PDU events.
	var readyOnce sync.Once
	s.pdu.On("error", func(e error) {
		glog.Error("rdp error:", e)
		select {
		case s.errCh <- e:
		default:
		}
	}).On("close", func() {
		glog.Info("rdp closed")
		select {
		case s.errCh <- fmt.Errorf("session closed"):
		default:
		}
	}).On("success", func() {
		glog.Info("rdp success")
	}).On("ready", func() {
		glog.Info("rdp ready")
		readyOnce.Do(func() { close(s.readyCh) })
	}).On("update", func(rectangles []pdu.BitmapData) {
		s.bmp.update(rectangles)
	})

	if err := s.x224.Connect(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("x224 connect: %w", err)
	}

	// Wait for ready or error, honoring the connect timeout.
	select {
	case <-s.readyCh:
		return s, nil
	case err := <-s.errCh:
		s.Close()
		return nil, err
	case <-time.After(20 * time.Second):
		s.Close()
		return nil, fmt.Errorf("timeout waiting for RDP ready")
	}
}

// splitDomain splits "DOMAIN\user" or "DOMAIN/user" into (domain, user).
func splitDomain(user string) (domain, uname string) {
	if i := strings.IndexAny(user, "\\/"); i != -1 {
		return user[:i], user[i+1:]
	}
	return "", user
}

// Close tears down the RDP connection.
func (s *rdpSession) Close() {
	if s != nil && s.tpkt != nil {
		s.tpkt.Close()
	}
}

// keyDown sends a scancode key-down event.
func (s *rdpSession) keyDown(scancode int, extended bool) {
	p := &pdu.ScancodeKeyEvent{}
	p.KeyCode = uint16(scancode)
	if extended {
		p.KeyboardFlags |= pdu.KBDFLAGS_EXTENDED
	}
	s.pdu.SendInputEvents(pdu.INPUT_EVENT_SCANCODE, []pdu.InputEventsInterface{p})
}

// keyUp sends a scancode key-up event.
func (s *rdpSession) keyUp(scancode int, extended bool) {
	p := &pdu.ScancodeKeyEvent{}
	p.KeyCode = uint16(scancode)
	p.KeyboardFlags |= pdu.KBDFLAGS_RELEASE
	if extended {
		p.KeyboardFlags |= pdu.KBDFLAGS_EXTENDED
	}
	s.pdu.SendInputEvents(pdu.INPUT_EVENT_SCANCODE, []pdu.InputEventsInterface{p})
}

// keyPress sends a down+up with a small delay between.
func (s *rdpSession) keyPress(scancode int, extended bool, delay time.Duration) {
	s.keyDown(scancode, extended)
	time.Sleep(delay)
	s.keyUp(scancode, extended)
	time.Sleep(delay)
}

// unicodeDown/Up send a unicode key event. Each rune is encoded to UTF-16; a
// rune outside the BMP is sent as a surrogate pair (two events, no release in
// between). The server inserts the literal code point regardless of the
// active keyboard layout.
func (s *rdpSession) unicodeDown(r rune) {
	for _, u := range utf16.Encode([]rune{r}) {
		p := &pdu.UnicodeKeyEvent{}
		p.Unicode = u
		p.KeyboardFlags |= pdu.KBDFLAGS_DOWN
		s.pdu.SendInputEvents(pdu.INPUT_EVENT_UNICODE, []pdu.InputEventsInterface{p})
	}
}

func (s *rdpSession) unicodeUp(r rune) {
	units := utf16.Encode([]rune{r})
	// Release the last unit only; surrogates are typed as one logical keypress.
	if len(units) == 0 {
		return
	}
	p := &pdu.UnicodeKeyEvent{}
	p.Unicode = units[len(units)-1]
	p.KeyboardFlags |= pdu.KBDFLAGS_RELEASE
	s.pdu.SendInputEvents(pdu.INPUT_EVENT_UNICODE, []pdu.InputEventsInterface{p})
}
