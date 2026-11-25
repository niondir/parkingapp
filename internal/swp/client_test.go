package swp

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer speaks just enough SockJS/STOMP to answer one search.
type fakeServer struct {
	t       *testing.T
	answers []string // queue messages sent after the search request
	mu      sync.Mutex
	conns   map[string]net.Conn // by STOMP username
	srv     *httptest.Server
	search  map[string]any
}

func newFakeServer(t *testing.T, answers ...string) *fakeServer {
	f := &fakeServer{t: t, answers: answers, conns: map[string]net.Conn{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/", f.ws)
	mux.HandleFunc("PUT /api/v1/information", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&f.search)
		f.mu.Lock()
		c := f.conns[f.search["clientId"].(string)]
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		go func() {
			for _, a := range f.answers {
				frame := "MESSAGE\ndestination:/users/queue/messages\n\n" + a + "\x00"
				p, _ := json.Marshal([]string{frame})
				writeServerFrame(c, "a"+string(p))
			}
		}()
	})
	mux.HandleFunc("GET /api/cities-list", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"isCaptcha":"false","cities":[{"code":"Hamburg","car_parks":[{"id":"entervo.core@99589@8225","name":"Krohnstieg Center"}]}]}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) client() *Client {
	c := New()
	c.APIURL = f.srv.URL + "/api/"
	c.WSURL = f.srv.URL + "/ws"
	c.Timeout = 5 * time.Second
	return c
}

func writeServerFrame(c net.Conn, s string) {
	h := []byte{0x81}
	switch n := len(s); {
	case n < 126:
		h = append(h, byte(n))
	default:
		h = append(h, 126, byte(n>>8), byte(n))
	}
	c.Write(append(h, s...))
}

func readClientFrame(r *bufio.Reader) (string, error) {
	b0, _ := r.ReadByte()
	b1, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	n := int(b1 & 0x7f)
	if n == 126 {
		var x [2]byte
		io.ReadFull(r, x[:])
		n = int(binary.BigEndian.Uint16(x[:]))
	}
	var mask [4]byte
	io.ReadFull(r, mask[:])
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return "", err
	}
	for i := range p {
		p[i] ^= mask[i%4]
	}
	_ = b0
	return string(p), nil
}

func (f *fakeServer) ws(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/websocket") || r.Header.Get("Upgrade") != "websocket" {
		http.Error(w, "no", http.StatusBadRequest)
		return
	}
	c, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		f.t.Error(err)
		return
	}
	rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		acceptKey(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n")
	rw.Flush()
	writeServerFrame(c, "o")
	writeServerFrame(c, "h")
	for {
		msg, err := readClientFrame(rw.Reader)
		if err != nil {
			return
		}
		var frames []string
		_ = json.Unmarshal([]byte(msg), &frames)
		for _, fr := range frames {
			st := parseSTOMP(strings.TrimSuffix(fr, "\x00"))
			if st.command == "CONNECT" {
				f.mu.Lock()
				f.conns[st.headers["username"]] = c
				f.mu.Unlock()
				writeServerFrame(c, `a["CONNECTED\nversion:1.1\n\n\u0000"]`)
			}
		}
	}
}

const (
	noSession = `{"dataAction":"ResponseData","data":{"carParkName":"Krohnstieg Center","amount":"0","tariffTimeStart":null}}`
	present   = `{"dataAction":"ResponseData","data":{"carParkName":"Krohnstieg Center","carParkAddress":"Krohnstieg 41-43, 22415 Hamburg","operator":99589,"cellComputer":8225,"systemTenant":"entervo.core","amount":"350","originalAmount":"350","currency":"EUR","currencyDecimal":2,"tariffTimeStart":"2026-11-25T13:00:00Z","tariffTimeEnd":"2026-11-25T15:00:00Z","transactionType":"present"}}`
	notFound  = `{"dataAction":"FinishMessage","data":"FAILED: Payments not found","errorCode":true}`
	finished  = `{"dataAction":"FinishMessage","data":"OK"}`
)

func TestSearch(t *testing.T) {
	f := newFakeServer(t, present, noSession, finished)
	res, err := f.client().Search(context.Background(), "Hamburg", "HHKF860E")
	if err != nil {
		t.Fatal(err)
	}
	if f.search["city"] != "Hamburg" || f.search["mediumValue"] != "HHKF860E" || f.search["parking"] != "ALL" || f.search["mediumType"] != "lpn" {
		t.Errorf("search body: %v", f.search)
	}
	if !res.Complete || len(res.Hits) != 1 {
		t.Fatalf("result: %+v", res)
	}
	h := res.Hits[0]
	if p, ok := h.Price(); !ok || p != 3.5 || h.City != "Hamburg" || h.ParkID() != "99589@8225" {
		t.Errorf("hit: %+v price %v", h, p)
	}
}

func TestSearchNothing(t *testing.T) {
	f := newFakeServer(t, noSession, notFound)
	res, err := f.client().Search(context.Background(), "Hamburg", "HHKF860E")
	if err != nil || !res.Complete || len(res.Hits) != 0 {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestSearchUnknownCity(t *testing.T) {
	f := newFakeServer(t, `{"dataAction":"FinishMessage","data":"FAILED: ZR or tenant not found","errorCode":true,"parkingUnreachable":true}`)
	_, err := f.client().Search(context.Background(), "Atlantis", "HHKF860E")
	if !errors.Is(err, ErrUnknownCity) {
		t.Fatalf("err %v", err)
	}
}

func TestSearchUnreachable(t *testing.T) {
	f := newFakeServer(t, present, `{"dataAction":"FinishMessage","data":"OK","parkingUnreachable":true}`)
	res, err := f.client().Search(context.Background(), "Hamburg", "HHKF860E")
	if err != nil || res.Complete || len(res.Hits) != 1 {
		t.Fatalf("res %+v err %v", res, err)
	}
	f = newFakeServer(t, `{"dataAction":"FinishMessage","data":"timeout","parkingUnreachable":true}`)
	if _, err := f.client().Search(context.Background(), "Hamburg", "HHKF860E"); err == nil {
		t.Fatal("no car park answered, want error")
	}
}

func TestSearchTimeout(t *testing.T) {
	f := newFakeServer(t, noSession) // no FinishMessage
	c := f.client()
	c.Timeout = 300 * time.Millisecond
	if _, err := c.Search(context.Background(), "Hamburg", "HHKF860E"); err == nil {
		t.Fatal("want timeout")
	}
}

func TestCities(t *testing.T) {
	f := newFakeServer(t)
	c := f.client()
	city, err := c.FindCity(context.Background(), "hamburg")
	if err != nil || city.Name != "Hamburg" || len(city.Parks) != 1 {
		t.Fatalf("city %+v err %v", city, err)
	}
	if _, err := c.FindCity(context.Background(), "Atlantis"); !errors.Is(err, ErrUnknownCity) {
		t.Fatalf("err %v", err)
	}
}

func TestParseSockJS(t *testing.T) {
	frames, err := parseSockJS(`a["MESSAGE\nx:1\n\n{}\u0000CONNECTED\n\n\u0000"]`)
	if err != nil || len(frames) != 2 {
		t.Fatalf("%q %v", frames, err)
	}
	if f := parseSTOMP(frames[0]); f.command != "MESSAGE" || f.headers["x"] != "1" || f.body != "{}" {
		t.Errorf("%+v", f)
	}
	if _, err := parseSockJS(`c[3000,"Go away!"]`); err == nil {
		t.Error("close frame: want error")
	}
	if f, err := parseSockJS("h"); err != nil || f != nil {
		t.Error("heartbeat")
	}
}

func TestPayURL(t *testing.T) {
	h := Hit{Operator: 99589, CellComputer: 8225, SystemTenant: "entervo.core"}
	want := "https://smart-web-pay.scheidt-bachmann.net/parking/select?city=Hamburg&country=DE&lpn=HHKF860E&parking=99589%408225"
	if got := PayURL("Hamburg", "HHKF860E", &h); got != want {
		t.Errorf("got %s", got)
	}
	if got := PayURL("Bad Salzuflen", "HHKF860E", nil); got != "https://smart-web-pay.scheidt-bachmann.net/parking/select?city=Bad+Salzuflen&country=DE&lpn=HHKF860E" {
		t.Errorf("got %s", got)
	}
}
