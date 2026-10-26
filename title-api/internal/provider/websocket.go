package provider

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A minimal RFC 6455 client, enough for the alternative player's playback
// socket: text messages both ways, pings answered, a clean close. net/http
// does the handshake, through the same proxy as every other request, and
// hands over the connection on 101.

const (
	wsContinuation = 0x0
	wsText         = 0x1
	wsBinary       = 0x2
	wsClose        = 0x8
	wsPing         = 0x9
	wsPong         = 0xA

	wsCloseNormal    = 1000
	wsCloseGoingAway = 1001

	wsAcceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxMessage = 1 << 20
)

var errWSProtocol = errors.New("websocket protocol error")

// wsCloseError is the close frame that ended the connection.
type wsCloseError struct{ code int }

func (e *wsCloseError) Error() string { return fmt.Sprintf("websocket closed (%d)", e.code) }

type wsConn struct {
	rwc io.ReadWriteCloser
	br  *bufio.Reader

	writeMu   sync.Mutex
	closeSent bool

	// Of the message being read, for continuation frames.
	message    []byte
	reading    bool
	compressed bool
	inflate    *wsInflater // permessage-deflate, when negotiated
}

// wsInflater decompresses permessage-deflate messages. The server may
// refer back into earlier messages (context takeover), so each message is
// inflated with the last 32 KiB of output as its dictionary.
type wsInflater struct {
	window []byte
}

func (inflater *wsInflater) inflate(payload []byte) ([]byte, error) {
	source := io.MultiReader(bytes.NewReader(payload), bytes.NewReader([]byte{0, 0, 0xff, 0xff, 1, 0, 0, 0xff, 0xff}))
	reader := flate.NewReaderDict(source, inflater.window)
	message, err := io.ReadAll(io.LimitReader(reader, wsMaxMessage+1))
	if err != nil {
		return nil, err
	}
	if len(message) > wsMaxMessage {
		return nil, errWSProtocol
	}
	inflater.window = append(inflater.window, message...)
	if excess := len(inflater.window) - 32<<10; excess > 0 {
		inflater.window = append([]byte(nil), inflater.window[excess:]...)
	}
	return message, nil
}

// newWSTransport is an HTTP/1.1-only transport: an upgrade needs the
// connection to itself, which HTTP/2 would not offer.
func newWSTransport() *http.Transport {
	return &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   false,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

// dialWebSocket opens a ws:// or wss:// URL with the given extra headers
// (Origin, User-Agent and the like). ctx bounds the handshake only.
func dialWebSocket(ctx context.Context, transport http.RoundTripper, rawURL string, header http.Header) (*wsConn, error) {
	target, err := wsHTTPURL(rawURL)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	for name, values := range header {
		request.Header[name] = values
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", key)
	response, err := transport.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		response.Body.Close()
		return nil, &alternativeStatusError{status: response.Status, code: response.StatusCode, vd: response.Header.Get("X-VD")}
	}
	rwc, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		response.Body.Close()
		return nil, errors.New("websocket upgrade: connection not writable")
	}
	digest := sha1.Sum([]byte(key + wsAcceptGUID))
	if !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") ||
		!headerHasToken(response.Header, "Connection", "upgrade") ||
		response.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(digest[:]) {
		rwc.Close()
		return nil, errors.New("websocket upgrade: bad handshake reply")
	}
	return &wsConn{rwc: rwc, br: bufio.NewReader(rwc)}, nil
}

// wsHTTPURL is the http(s) URL the handshake of a ws(s) URL goes to.
func wsHTTPURL(rawURL string) (*url.URL, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	switch target.Scheme {
	case "ws":
		target.Scheme = "http"
	case "wss":
		target.Scheme = "https"
	default:
		return nil, fmt.Errorf("not a websocket URL: %q", rawURL)
	}
	return target, nil
}

func headerHasToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// writeFrame sends one final, masked frame, as clients must.
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closeSent {
		return net.ErrClosed
	}
	if opcode == wsClose {
		c.closeSent = true
	}
	frame := make([]byte, 0, 14+len(payload))
	frame = append(frame, 0x80|opcode)
	switch length := len(payload); {
	case length < 126:
		frame = append(frame, 0x80|byte(length))
	case length <= 0xFFFF:
		frame = append(frame, 0x80|126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(length))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(length))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	frame = append(frame, mask[:]...)
	for index, value := range payload {
		frame = append(frame, value^mask[index%4])
	}
	_, err := c.rwc.Write(frame)
	return err
}

func (c *wsConn) writeText(text []byte) error {
	return c.writeFrame(wsText, text)
}

// writeClose starts the closing handshake; the reader sees the server's
// answer as a wsCloseError.
func (c *wsConn) writeClose(code int) error {
	return c.writeFrame(wsClose, binary.BigEndian.AppendUint16(nil, uint16(code)))
}

func (c *wsConn) Close() error {
	return c.rwc.Close()
}

// readFrame reads one frame. Servers don't mask, and only RSV1 has a
// meaning (a compressed message), so a mask or another reserved bit is a
// protocol error.
func (c *wsConn) readFrame() (fin, rsv1 bool, opcode byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(c.br, head[:]); err != nil {
		return
	}
	fin, rsv1, opcode = head[0]&0x80 != 0, head[0]&0x40 != 0, head[0]&0x0F
	// RSV1 marks a compressed message, when permessage-deflate is on.
	if head[0]&0x30 != 0 || rsv1 && (c.inflate == nil || opcode >= wsClose || opcode == wsContinuation) || head[1]&0x80 != 0 {
		return false, false, 0, nil, errWSProtocol
	}
	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var extended [2]byte
		if _, err = io.ReadFull(c.br, extended[:]); err != nil {
			return
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err = io.ReadFull(c.br, extended[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint64(extended[:])
	}
	if opcode >= wsClose && (!fin || length > 125) || length > wsMaxMessage {
		return false, false, 0, nil, errWSProtocol
	}
	payload = make([]byte, length)
	_, err = io.ReadFull(c.br, payload)
	return
}

// readMessage returns the next text or binary message, put together from
// its continuation frames. Pings are answered on the way; a close frame is
// answered too and ends the connection with a wsCloseError.
func (c *wsConn) readMessage() ([]byte, error) {
	for {
		fin, rsv1, opcode, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case wsPing:
			if err := c.writeFrame(wsPong, payload); err != nil && !errors.Is(err, net.ErrClosed) {
				return nil, err
			}
			continue
		case wsPong:
			continue
		case wsClose:
			code := 1005
			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload))
			}
			// Echoes the code, unless this answers our own close.
			_ = c.writeFrame(wsClose, payload[:min(len(payload), 2)])
			return nil, &wsCloseError{code: code}
		case wsText, wsBinary:
			if c.reading {
				return nil, errWSProtocol
			}
			c.message, c.reading, c.compressed = payload, true, rsv1
		case wsContinuation:
			if !c.reading || len(c.message)+len(payload) > wsMaxMessage {
				return nil, errWSProtocol
			}
			c.message = append(c.message, payload...)
		default:
			return nil, errWSProtocol
		}
		if fin {
			message, compressed := c.message, c.compressed
			c.message, c.reading, c.compressed = nil, false, false
			if compressed {
				return c.inflate.inflate(message)
			}
			return message, nil
		}
	}
}
