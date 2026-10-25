package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// wsTestPeer is the server end of a test connection.
type wsTestPeer struct {
	t       *testing.T
	conn    net.Conn
	br      *bufio.Reader
	request *http.Request
}

// newWSTestServer accepts websocket handshakes, checking them as a server
// must, and hands each connection over; accept may refuse one with a
// status instead.
func newWSTestServer(t *testing.T, accept func(*http.Request) int, peers chan<- *wsTestPeer) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		key, _ := base64.StdEncoding.DecodeString(request.Header.Get("Sec-WebSocket-Key"))
		if request.Method != http.MethodGet || request.Header.Get("Upgrade") != "websocket" ||
			!headerHasToken(request.Header, "Connection", "upgrade") ||
			request.Header.Get("Sec-WebSocket-Version") != "13" || len(key) != 16 ||
			request.ProtoMajor != 1 || request.Header.Get("Sec-WebSocket-Extensions") != "" {
			t.Errorf("bad handshake: %s %s %v", request.Method, request.Proto, request.Header)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		if accept != nil {
			if status := accept(request); status != http.StatusSwitchingProtocols {
				response.WriteHeader(status)
				return
			}
		}
		conn, rw, err := http.NewResponseController(response).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		digest := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + wsAcceptGUID))
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(digest[:]) + "\r\n\r\n")
		_ = rw.Flush()
		peer := &wsTestPeer{t: t, conn: conn, br: rw.Reader, request: request}
		t.Cleanup(func() { conn.Close() })
		peers <- peer
	}))
	t.Cleanup(server.Close)
	return server
}

func wsTestURL(server *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(server.URL, "http") + path
}

func waitPeer(t *testing.T, peers <-chan *wsTestPeer) *wsTestPeer {
	t.Helper()
	select {
	case peer := <-peers:
		return peer
	case <-time.After(5 * time.Second):
		t.Fatal("no websocket connection")
		return nil
	}
}

// readFrame reads a client frame, which must be final and masked.
func (peer *wsTestPeer) readFrame() (byte, []byte, error) {
	_ = peer.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var head [2]byte
	if _, err := io.ReadFull(peer.br, head[:]); err != nil {
		return 0, nil, err
	}
	if head[0]&0x80 == 0 || head[1]&0x80 == 0 {
		peer.t.Errorf("client frame %08b %08b is not final and masked", head[0], head[1])
	}
	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var extended [2]byte
		_, _ = io.ReadFull(peer.br, extended[:])
		length = uint64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		_, _ = io.ReadFull(peer.br, extended[:])
		length = binary.BigEndian.Uint64(extended[:])
	}
	var mask [4]byte
	if _, err := io.ReadFull(peer.br, mask[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(peer.br, payload); err != nil {
		return 0, nil, err
	}
	for index := range payload {
		payload[index] ^= mask[index%4]
	}
	return head[0] & 0x0F, payload, nil
}

// readMessage is the next text message as JSON, or nil for a close frame
// (its code in close).
func (peer *wsTestPeer) readMessage() (message map[string]any, close int) {
	peer.t.Helper()
	for {
		opcode, payload, err := peer.readFrame()
		if err != nil {
			peer.t.Fatalf("read client frame: %v", err)
		}
		switch opcode {
		case wsClose:
			return nil, int(binary.BigEndian.Uint16(payload))
		case wsText:
			if err := json.Unmarshal(payload, &message); err != nil {
				peer.t.Fatalf("client message %q: %v", payload, err)
			}
			return message, 0
		}
	}
}

func (peer *wsTestPeer) writeFrame(fin bool, opcode byte, payload []byte) {
	head := []byte{opcode, 0}
	if fin {
		head[0] |= 0x80
	}
	switch {
	case len(payload) < 126:
		head[1] = byte(len(payload))
	case len(payload) <= 0xFFFF:
		head[1] = 126
		head = binary.BigEndian.AppendUint16(head, uint16(len(payload)))
	default:
		head[1] = 127
		head = binary.BigEndian.AppendUint64(head, uint64(len(payload)))
	}
	if _, err := peer.conn.Write(append(head, payload...)); err != nil {
		peer.t.Errorf("write server frame: %v", err)
	}
}

func (peer *wsTestPeer) writeClose(code int) {
	peer.writeFrame(true, wsClose, binary.BigEndian.AppendUint16(nil, uint16(code)))
}

func TestWebSocketExchange(t *testing.T) {
	t.Parallel()
	peers := make(chan *wsTestPeer, 1)
	server := newWSTestServer(t, nil, peers)
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := dialWebSocket(ctx, newWSTransport(), wsTestURL(server, "/ws/?sid=a%2Bb"),
		http.Header{"Origin": {"https://player.example"}, "User-Agent": {"receiver"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The connection outlives the context of its handshake.
	cancel()
	peer := waitPeer(t, peers)
	if peer.request.URL.RawQuery != "sid=a%2Bb" || peer.request.Header.Get("Origin") != "https://player.example" ||
		peer.request.UserAgent() != "receiver" {
		t.Fatalf("handshake request: %s %v", peer.request.URL, peer.request.Header)
	}

	for _, size := range []int{5, 300, 70000} {
		text := bytes.Repeat([]byte("x"), size)
		if err := conn.writeText(text); err != nil {
			t.Fatal(err)
		}
		if opcode, payload, err := peer.readFrame(); err != nil || opcode != wsText || !bytes.Equal(payload, text) {
			t.Fatalf("%d bytes: opcode %d, %d bytes, %v", size, opcode, len(payload), err)
		}
	}

	// A fragmented message with a ping in the middle, then a long one.
	peer.writeFrame(false, wsText, []byte("con"))
	peer.writeFrame(true, wsPing, []byte("are you there"))
	peer.writeFrame(true, wsContinuation, []byte("tinued"))
	long := bytes.Repeat([]byte("y"), 70000)
	peer.writeFrame(true, wsText, long)
	if message, err := conn.readMessage(); err != nil || string(message) != "continued" {
		t.Fatalf("message = %q, %v", message, err)
	}
	if opcode, payload, err := peer.readFrame(); err != nil || opcode != wsPong || string(payload) != "are you there" {
		t.Fatalf("pong: opcode %d, %q, %v", opcode, payload, err)
	}
	if message, err := conn.readMessage(); err != nil || !bytes.Equal(message, long) {
		t.Fatalf("long message: %d bytes, %v", len(message), err)
	}

	// The server's close is answered with its code.
	peer.writeClose(wsCloseGoingAway)
	var closed *wsCloseError
	if _, err := conn.readMessage(); !errors.As(err, &closed) || closed.code != wsCloseGoingAway {
		t.Fatalf("read after close: %v", err)
	}
	if _, code := peer.readMessage(); code != wsCloseGoingAway {
		t.Fatalf("close answered with %d", code)
	}
	if err := conn.writeText([]byte("late")); err == nil {
		t.Fatal("wrote after the close")
	}
}

func TestWebSocketRejects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	refusing := newWSTestServer(t, func(*http.Request) int { return http.StatusForbidden }, nil)
	var refused *alternativeStatusError
	if _, err := dialWebSocket(ctx, newWSTransport(), wsTestURL(refusing, "/"), nil); !errors.As(err, &refused) || refused.code != http.StatusForbidden {
		t.Fatalf("refused handshake: %v", err)
	}

	wrongAccept := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		conn, rw, _ := http.NewResponseController(response).Hijack()
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: bm9wZQ==\r\n\r\n")
		_ = rw.Flush()
	}))
	defer wrongAccept.Close()
	if _, err := dialWebSocket(ctx, newWSTransport(), wsTestURL(wrongAccept, "/"), nil); err == nil {
		t.Fatal("accepted a wrong Sec-WebSocket-Accept")
	}

	if _, err := dialWebSocket(ctx, newWSTransport(), "https://example.com/", nil); err == nil {
		t.Fatal("dialed a non-websocket URL")
	}

	// Server frames must not be masked.
	peers := make(chan *wsTestPeer, 1)
	server := newWSTestServer(t, nil, peers)
	conn, err := dialWebSocket(ctx, newWSTransport(), wsTestURL(server, "/"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer := waitPeer(t, peers)
	_, _ = peer.conn.Write([]byte{0x81, 0x82, 1, 2, 3, 4, 'h' ^ 1, 'i' ^ 2})
	if _, err := conn.readMessage(); !errors.Is(err, errWSProtocol) {
		t.Fatalf("masked server frame: %v", err)
	}
}
