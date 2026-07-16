// Package cliprdr is a minimal, portable (headless) implementation of the
// RDP Clipboard Virtual Channel (CLIPRDR) static channel for text capture only.
//
// It implements plugin.ChannelTransport and only needs to RECEIVE remote
// clipboard text (CF_UNICODETEXT). It does NOT touch the OS clipboard and
// builds on every platform (no Windows-only symbols).
//
// Protocol (MS-RDPECLIP), receive flow we care about:
//
//	Server -> CB_CLIP_CAPS             (server capabilities)
//	Server -> CB_MONITOR_READY          (server ready)
//	Client -> CB_CLIP_CAPS             (our capabilities)
//	Client -> CB_FORMAT_LIST           (formats we "offer": CF_UNICODETEXT)
//	Server -> CB_FORMAT_LIST_RESPONSE
//	... remote clipboard changes ...
//	Server -> CB_FORMAT_LIST           (announces new content)
//	Client -> CB_FORMAT_LIST_RESPONSE
//	Client -> CB_FORMAT_DATA_REQUEST   (request CF_UNICODETEXT)
//	Server -> CB_FORMAT_DATA_RESPONSE  (UTF-16LE text) -> captured into OnText
package cliprdr

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/tomatome/grdp/core"
	"github.com/tomatome/grdp/glog"
	"github.com/tomatome/grdp/plugin"
)

// CLIPRDR message types (MS-RDPECLIP 2.2).
const (
	CB_MONITOR_READY         = 0x0001
	CB_FORMAT_LIST           = 0x0002
	CB_FORMAT_LIST_RESPONSE  = 0x0003
	CB_FORMAT_DATA_REQUEST   = 0x0004
	CB_FORMAT_DATA_RESPONSE  = 0x0005
	CB_TEMP_DIRECTORY        = 0x0006
	CB_CLIP_CAPS             = 0x0007
	CB_FILECONTENTS_REQUEST  = 0x0008
	CB_FILECONTENTS_RESPONSE = 0x0009
	CB_LOCK_CLIPDATA         = 0x000A
	CB_UNLOCK_CLIPDATA       = 0x000B
)

// Response flags.
const (
	CB_RESPONSE_OK   = 0x0001
	CB_RESPONSE_FAIL = 0x0002
)

// Capability general flags.
const (
	CB_USE_LONG_FORMAT_NAMES   = 0x00000002
	CB_STREAM_FILECLIP_ENABLED = 0x00000004
	CB_FILECLIP_NO_FILE_PATHS  = 0x00000008
	CB_CAN_LOCK_CLIPDATA       = 0x00000010
)

// Capability set type / version.
const (
	CB_CAPSTYPE_GENERAL = 0x0001
	CB_CAPS_VERSION_2   = 0x00000002
)

// Standard clipboard format IDs (Win32).
const (
	CF_UNICODETEXT = 13 // 0x000D
)

// Client is a headless CLIPRDR client that captures remote clipboard text.
//
// Wire up with:
//
//	cc := cliprdr.NewClient()
//	channels.Register(cc)
//	// ... after server sends CB_FORMAT_LIST (remote clipboard changed):
//	cc.RequestText()  // sends CB_FORMAT_DATA_REQUEST for CF_UNICODETEXT
//	// then wait on cc.OnText channel
type Client struct {
	w core.ChannelSender

	mu      sync.Mutex
	useLong bool

	// OnText receives captured UTF-8 text from the remote clipboard
	// (CF_UNICODETEXT). Buffered(16). Read from it after RequestText().
	OnText chan string

	// OnFormatList is signalled (non-blocking, best-effort) every time the
	// server announces new clipboard content (CB_FORMAT_LIST).
	OnFormatList chan struct{}

	// lastRemoteFormats is the list of format IDs announced by the server
	// in the most recent CB_FORMAT_LIST.
	lastRemoteFormats []uint32

	// sentInitialFormatList tracks whether we already sent our initial
	// CB_FORMAT_LIST in response to CB_MONITOR_READY.
	sentInitialFormatList bool
}

// NewClient creates a new headless clipboard client.
func NewClient() *Client {
	return &Client{
		OnText:       make(chan string, 16),
		OnFormatList: make(chan struct{}, 16),
	}
}

// GetType returns the channel name and options (ChannelTransport).
func (c *Client) GetType() (string, uint32) {
	return plugin.CLIPRDR_SVC_CHANNEL_NAME,
		plugin.CHANNEL_OPTION_INITIALIZED | plugin.CHANNEL_OPTION_ENCRYPT_RDP |
			plugin.CHANNEL_OPTION_COMPRESS_RDP | plugin.CHANNEL_OPTION_SHOW_PROTOCOL
}

// Sender wires the channel sender (ChannelTransport). Called by Channels.Register.
func (c *Client) Sender(f core.ChannelSender) {
	c.w = f
}

// Send writes a raw CLIPRDR PDU to the channel.
func (c *Client) Send(s []byte) (int, error) {
	glog.Debug("cliprdr send:", len(s), hex.EncodeToString(s))
	name, _ := c.GetType()
	if c.w == nil {
		return 0, fmt.Errorf("cliprdr: channel sender not set")
	}
	return c.w.SendToChannel(name, s)
}

// Process handles an incoming reassembled CLIPRDR PDU (ChannelTransport).
func (c *Client) Process(s []byte) {
	glog.Debug("cliprdr recv:", hex.EncodeToString(s))
	r := bytes.NewReader(s)

	msgType, err := core.ReadUint16LE(r)
	if err != nil {
		glog.Error("cliprdr: read msgType:", err)
		return
	}
	flag, _ := core.ReadUint16LE(r)
	length, _ := core.ReadUInt32LE(r)
	glog.Debugf("cliprdr: type=0x%x flag=%d length=%d remaining=%d", msgType, flag, length, r.Len())

	b, _ := core.ReadBytes(int(length), r)

	switch msgType {
	case CB_CLIP_CAPS:
		c.processClipCaps(b)
	case CB_MONITOR_READY:
		c.processMonitorReady(b)
	case CB_FORMAT_LIST:
		c.processFormatList(b)
	case CB_FORMAT_LIST_RESPONSE:
		// nothing useful to do; log only
		glog.Debug("cliprdr: CB_FORMAT_LIST_RESPONSE flag=", flag)
	case CB_FORMAT_DATA_REQUEST:
		// Server asks us for clipboard data. Headless = we have nothing to
		// offer, respond with empty failure so the protocol stays consistent.
		c.sendFormatDataResponse(nil, false)
	case CB_FORMAT_DATA_RESPONSE:
		c.processFormatDataResponse(flag, b)
	case CB_LOCK_CLIPDATA, CB_UNLOCK_CLIPDATA:
		// ignore
	default:
		glog.Debugf("cliprdr: unhandled type 0x%x", msgType)
	}
}

// ---- inbound handlers ----

func (c *Client) processClipCaps(b []byte) {
	r := bytes.NewReader(b)
	// CLIPRDR_CAPABILITIES_PDU: cCapabilitiesSets(uint16) + pad(uint16) + capability sets
	numSets, _ := core.ReadUint16LE(r)
	_, _ = core.ReadUint16LE(r) // pad
	for i := uint16(0); i < numSets; i++ {
		// CliprdrGeneralCapabilitySet: capabilitySetType(u16) capabilitySetLength(u16) version(u32) generalFlags(u32)
		capType, _ := core.ReadUint16LE(r)
		capLen, _ := core.ReadUint16LE(r)
		version, _ := core.ReadUInt32LE(r)
		generalFlags, _ := core.ReadUInt32LE(r)
		glog.Debugf("cliprdr caps: type=%d len=%d ver=%d flags=0x%x", capType, capLen, version, generalFlags)
		c.mu.Lock()
		c.useLong = generalFlags&CB_USE_LONG_FORMAT_NAMES != 0
		c.mu.Unlock()
	}
}

func (c *Client) processMonitorReady(b []byte) {
	// Respond with our capabilities, then an initial format list offering
	// CF_UNICODETEXT so the server knows we accept text.
	c.sendClientCapabilitiesPDU()
	c.sendFormatListPDU()
	c.mu.Lock()
	c.sentInitialFormatList = true
	c.mu.Unlock()
}

func (c *Client) processFormatList(b []byte) {
	formats := c.readFormatList(b)
	glog.Debug("cliprdr: server announced", len(formats), "formats")

	// Acknowledge the format list.
	c.sendFormatListResponse(CB_RESPONSE_OK)

	// Notify any waiter that the remote clipboard changed.
	select {
	case c.OnFormatList <- struct{}{}:
	default:
		// drop if nobody is listening; non-blocking
	}
}

func (c *Client) processFormatDataResponse(flag uint16, b []byte) {
	if flag != CB_RESPONSE_OK {
		glog.Error("cliprdr: CB_FORMAT_DATA_RESPONSE failed flag=", flag)
		// still deliver empty so a waiter doesn't block forever
		select {
		case c.OnText <- "":
		default:
		}
		return
	}
	// b is UTF-16LE text (null-terminated) for CF_UNICODETEXT requests.
	text := decodeUTF16LE(b)
	select {
	case c.OnText <- text:
	default:
		glog.Warn("cliprdr: OnText channel full, dropping captured text")
	}
}

// readFormatList parses a CB_FORMAT_LIST payload into format IDs (+ names).
func (c *Client) readFormatList(b []byte) []uint32 {
	r := bytes.NewReader(b)
	out := make([]uint32, 0, 8)
	for r.Len() > 0 {
		formatId, err := core.ReadUInt32LE(r)
		if err != nil {
			break
		}
		out = append(out, formatId)

		c.mu.Lock()
		useLong := c.useLong
		c.mu.Unlock()

		if useLong {
			// long format names: UTF-16LE null-terminated string
			for r.Len() >= 2 {
				ch, err := core.ReadUint16LE(r)
				if err != nil || ch == 0 {
					break
				}
			}
		}
		// In short format name mode there is no name; just the 4-byte id.
		// (Windows servers typically use CB_USE_LONG_FORMAT_NAMES.)
	}
	c.mu.Lock()
	c.lastRemoteFormats = out
	c.mu.Unlock()
	return out
}

// ---- outbound senders ----

func (c *Client) sendClientCapabilitiesPDU() {
	// Header(8) + cCapabilitiesSets(u16) + pad(u16) + one capability set(12) = 24
	buff := &bytes.Buffer{}
	writeHeader(buff, CB_CLIP_CAPS, 0, 16) // DataLen covers everything after the 8-byte header
	core.WriteUInt16LE(1, buff)            // cCapabilitiesSets
	core.WriteUInt16LE(0, buff)            // pad
	// CliprdrGeneralCapabilitySet
	core.WriteUInt16LE(CB_CAPSTYPE_GENERAL, buff) // capabilitySetType
	core.WriteUInt16LE(12, buff)                  // capabilitySetLength
	core.WriteUInt32LE(CB_CAPS_VERSION_2, buff)   // version
	// generalFlags: long format names only (no file clip)
	core.WriteUInt32LE(CB_USE_LONG_FORMAT_NAMES, buff)

	c.Send(buff.Bytes())
}

// sendFormatListPDU announces the formats we (the client) "have" on our local
// clipboard. For text capture we advertise CF_UNICODETEXT so the server will
// offer text back to us.
func (c *Client) sendFormatListPDU() {
	body := &bytes.Buffer{}
	// Format entry: FormatId(u32) + FormatName(UTF-16LE, null-terminated).
	// For standard formats the name may be empty (just a terminating null u16).
	core.WriteUInt32LE(CF_UNICODETEXT, body)
	core.WriteUInt16LE(0, body) // empty name terminator

	buff := &bytes.Buffer{}
	writeHeader(buff, CB_FORMAT_LIST, 0, uint32(body.Len()))
	buff.Write(body.Bytes())

	c.Send(buff.Bytes())
}

func (c *Client) sendFormatListResponse(flags uint16) {
	buff := &bytes.Buffer{}
	writeHeader(buff, CB_FORMAT_LIST_RESPONSE, flags, 0)
	c.Send(buff.Bytes())
}

// RequestText sends a CB_FORMAT_DATA_REQUEST for CF_UNICODETEXT. After calling
// this, read the captured text from OnText.
func (c *Client) RequestText() {
	buff := &bytes.Buffer{}
	writeHeader(buff, CB_FORMAT_DATA_REQUEST, 0, 4)
	core.WriteUInt32LE(CF_UNICODETEXT, buff)
	c.Send(buff.Bytes())
}

func (c *Client) sendFormatDataResponse(data []byte, ok bool) {
	flag := uint16(CB_RESPONSE_FAIL)
	if ok {
		flag = CB_RESPONSE_OK
	}
	buff := &bytes.Buffer{}
	writeHeader(buff, CB_FORMAT_DATA_RESPONSE, flag, uint32(len(data)))
	buff.Write(data)
	c.Send(buff.Bytes())
}

// ---- helpers ----

// writeHeader writes an 8-byte CLIPRDR PDU header (MsgType, MsgFlags, DataLen).
func writeHeader(buff *bytes.Buffer, msgType, flags uint16, dataLen uint32) {
	core.WriteUInt16LE(msgType, buff)
	core.WriteUInt16LE(flags, buff)
	core.WriteUInt32LE(dataLen, buff)
}

// decodeUTF16LE decodes UTF-16LE bytes (possibly null-terminated) to a UTF-8
// string, trimming trailing NULs.
func decodeUTF16LE(b []byte) string {
	r := bytes.NewReader(b)
	codes := make([]uint16, 0, len(b)/2)
	for r.Len() >= 2 {
		ch, err := core.ReadUint16LE(r)
		if err != nil {
			break
		}
		if ch == 0 {
			break
		}
		codes = append(codes, ch)
	}
	s := string(utf16.Decode(codes))
	return strings.TrimRight(s, "\x00")
}
