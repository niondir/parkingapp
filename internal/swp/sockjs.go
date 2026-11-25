package swp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// A minimal WebSocket client (RFC 6455) carrying SockJS frames carrying STOMP
// frames – just what the search needs, without a dependency.

// wsTransport speaks HTTP/1.1 only: a WebSocket upgrade does not work over HTTP/2.
var wsTransport = &http.Transport{
	Proxy:        http.ProxyFromEnvironment,
	TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
}

type sockConn struct {
	rw     io.ReadWriteCloser
	r      *bufio.Reader
	queued []string // STOMP frames received but not read yet
}

func dialSockJS(ctx context.Context, base string) (*sockConn, error) {
	u := strings.TrimSuffix(base, "/") + "/" + fmt.Sprint(100+randInt(900)) + "/" + newUUID()[:8] + "/websocket"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	k := base64.StdEncoding.EncodeToString(key)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", k)
	req.Header.Set("User-Agent", userAgent)
	resp, err := (&http.Client{Transport: wsTransport}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		return nil, fmt.Errorf("websocket upgrade: %s", resp.Status)
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(k) {
		resp.Body.Close()
		return nil, errors.New("websocket upgrade: bad accept key")
	}
	rw, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, errors.New("websocket upgrade: body not writable")
	}
	c := &sockConn{rw: rw, r: bufio.NewReader(rw)}
	// SockJS opens with "o".
	f, err := c.readText()
	if err != nil {
		c.Close()
		return nil, err
	}
	if f != "o" {
		c.Close()
		return nil, fmt.Errorf("sockjs: expected open frame, got %q", f)
	}
	return c, nil
}

func acceptKey(k string) string {
	h := sha1.Sum([]byte(k + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

func (c *sockConn) Close() error { return c.rw.Close() }

// --- WebSocket frames ---

const (
	opCont  = 0
	opText  = 1
	opClose = 8
	opPing  = 9
	opPong  = 10
)

func (c *sockConn) writeFrame(op byte, p []byte) error {
	h := []byte{0x80 | op}
	switch n := len(p); {
	case n < 126:
		h = append(h, 0x80|byte(n))
	case n < 1<<16:
		h = append(h, 0x80|126, byte(n>>8), byte(n))
	default:
		h = append(h, 0x80|127)
		h = binary.BigEndian.AppendUint64(h, uint64(n))
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	h = append(h, mask[:]...)
	buf := make([]byte, len(h)+len(p))
	copy(buf, h)
	for i, b := range p {
		buf[len(h)+i] = b ^ mask[i%4]
	}
	_, err := c.rw.Write(buf)
	return err
}

// maxMessage protects against a broken length field; answers are a few KB.
const maxMessage = 1 << 20

// readText returns the next text message, answering pings on the way.
func (c *sockConn) readText() (string, error) {
	var msg []byte
	for {
		b0, err := c.r.ReadByte()
		if err != nil {
			return "", err
		}
		b1, err := c.r.ReadByte()
		if err != nil {
			return "", err
		}
		n := uint64(b1 & 0x7f)
		switch n {
		case 126:
			var x [2]byte
			if _, err := io.ReadFull(c.r, x[:]); err != nil {
				return "", err
			}
			n = uint64(binary.BigEndian.Uint16(x[:]))
		case 127:
			var x [8]byte
			if _, err := io.ReadFull(c.r, x[:]); err != nil {
				return "", err
			}
			n = binary.BigEndian.Uint64(x[:])
		}
		if n > maxMessage || uint64(len(msg))+n > maxMessage {
			return "", errors.New("websocket: message too large")
		}
		var mask [4]byte
		if b1&0x80 != 0 { // servers must not mask, but tolerate it
			if _, err := io.ReadFull(c.r, mask[:]); err != nil {
				return "", err
			}
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(c.r, p); err != nil {
			return "", err
		}
		if b1&0x80 != 0 {
			for i := range p {
				p[i] ^= mask[i%4]
			}
		}
		switch op := b0 & 0x0f; op {
		case opPing:
			if err := c.writeFrame(opPong, p); err != nil {
				return "", err
			}
			continue
		case opPong:
			continue
		case opClose:
			return "", io.EOF
		case opText, opCont:
			msg = append(msg, p...)
		default:
			return "", fmt.Errorf("websocket: unexpected opcode %d", op)
		}
		if b0&0x80 != 0 {
			return string(msg), nil
		}
	}
}

// --- SockJS and STOMP ---

type stompFrame struct {
	command string
	headers map[string]string
	body    string
}

func (c *sockConn) sendSTOMP(command string, headers map[string]string, body string) error {
	var b strings.Builder
	b.WriteString(command + "\n")
	for k, v := range headers {
		b.WriteString(k + ":" + v + "\n")
	}
	b.WriteString("\n" + body + "\x00")
	p, err := json.Marshal([]string{b.String()})
	if err != nil {
		return err
	}
	return c.writeFrame(opText, p)
}

// readSTOMP returns the next STOMP frame, skipping SockJS heartbeats.
func (c *sockConn) readSTOMP() (stompFrame, error) {
	for len(c.queued) == 0 {
		s, err := c.readText()
		if err != nil {
			return stompFrame{}, err
		}
		frames, err := parseSockJS(s)
		if err != nil {
			return stompFrame{}, err
		}
		c.queued = frames
	}
	s := c.queued[0]
	c.queued = c.queued[1:]
	return parseSTOMP(s), nil
}

// parseSockJS returns the STOMP frames of a SockJS frame: "h" is a heartbeat,
// "a[...]" a JSON array of messages, "c[code,reason]" closes the session.
func parseSockJS(s string) ([]string, error) {
	switch {
	case s == "h" || s == "":
		return nil, nil
	case strings.HasPrefix(s, "a"):
		var msgs []string
		if err := json.Unmarshal([]byte(s[1:]), &msgs); err != nil {
			return nil, fmt.Errorf("sockjs: %w", err)
		}
		var frames []string
		for _, m := range msgs {
			// One message may hold several STOMP frames, each ended by NUL.
			for _, f := range strings.Split(m, "\x00") {
				if strings.TrimSpace(f) != "" {
					frames = append(frames, f)
				}
			}
		}
		return frames, nil
	case strings.HasPrefix(s, "c"):
		return nil, fmt.Errorf("sockjs: closed by server: %s", s[1:])
	}
	return nil, fmt.Errorf("sockjs: unexpected frame %q", s)
}

func parseSTOMP(s string) stompFrame {
	s = strings.TrimLeft(s, "\r\n") // heart-beat newlines
	head, body, _ := strings.Cut(s, "\n\n")
	lines := strings.Split(head, "\n")
	f := stompFrame{command: strings.TrimSpace(lines[0]), headers: map[string]string{}, body: body}
	for _, l := range lines[1:] {
		if k, v, ok := strings.Cut(l, ":"); ok {
			if _, dup := f.headers[k]; !dup { // the first one wins (STOMP 1.2)
				f.headers[k] = v
			}
		}
	}
	return f
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func randInt(n int) int {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return int(binary.BigEndian.Uint16(b[:])) % n
}
