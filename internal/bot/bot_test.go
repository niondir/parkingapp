package bot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/niondir/parkingapp/internal/fairparken"
	"github.com/niondir/parkingapp/internal/store"
	"github.com/niondir/parkingapp/internal/telegram"
)

type sent struct {
	chat   int64
	text   string
	kb     *telegram.InlineKeyboard
	silent bool
	edit   int64 // message ID for edits
	markup any
}

type fakeTG struct {
	mu      sync.Mutex
	msgs    []sent
	pending []telegram.Update
	offsets []int64
	nextID  int64
}

func (f *fakeTG) GetMe(context.Context) (telegram.User, error) { return telegram.User{}, nil }
func (f *fakeTG) GetUpdates(_ context.Context, offset int64, _ int) ([]telegram.Update, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offsets = append(f.offsets, offset)
	if offset == -1 && len(f.pending) > 0 {
		return f.pending[len(f.pending)-1:], nil
	}
	return nil, nil
}
func (f *fakeTG) AnswerCallback(context.Context, string, string) error            { return nil }
func (f *fakeTG) SetCommands(context.Context, []telegram.BotCommand, int64) error { return nil }
func (f *fakeTG) Send(ctx context.Context, chat int64, text string, kb *telegram.InlineKeyboard) error {
	return f.Push(ctx, chat, text, kb, false)
}
func (f *fakeTG) Push(_ context.Context, chat int64, text string, kb *telegram.InlineKeyboard, silent bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, sent{chat: chat, text: text, kb: kb, silent: silent})
	return nil
}
func (f *fakeTG) SendMsg(_ context.Context, chat int64, text string, markup any) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kb, _ := markup.(*telegram.InlineKeyboard)
	f.msgs = append(f.msgs, sent{chat: chat, text: text, kb: kb, markup: markup})
	f.nextID++
	return f.nextID, nil
}
func (f *fakeTG) Edit(_ context.Context, chat, msgID int64, text string, kb *telegram.InlineKeyboard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, sent{chat: chat, text: text, kb: kb, edit: msgID})
	return nil
}

func (f *fakeTG) take() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.msgs
	f.msgs = nil
	return m
}

// fakeAPI serves the fair parken endpoints from in-memory data.
type fakeAPI struct {
	mu       sync.Mutex
	sessions map[string][]map[string]any
	articles []any
	quotes   []int64
}

func (a *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case r.URL.Path == "/api/AnprResults/AnprResultSearch":
		var req struct{ Plate string }
		json.NewDecoder(r.Body).Decode(&req)
		var res []map[string]any
		for _, s := range a.sessions[req.Plate] {
			arts := a.articles
			if arts == nil {
				arts = []any{}
			}
			res = append(res, map[string]any{"anprResult": s, "articles": arts})
		}
		if res == nil {
			res = []map[string]any{}
		}
		json.NewEncoder(w).Encode(res)
	case strings.HasPrefix(r.URL.Path, "/api/AnprResults/GetTariffs/"):
		w.Write([]byte(`[{"id":1,"icon":null},{"id":2,"icon":"Wohnmobil"}]`))
	case r.URL.Path == "/api/Payment/ApplyVoucherCode":
		var item map[string]any
		json.NewDecoder(r.Body).Decode(&item)
		if vc, _ := item["voucherCode"].(string); vc != "" {
			if vc != "RABATT" {
				http.Error(w, `{"error":"invalid"}`, http.StatusBadRequest)
				return
			}
			item["price"] = 1.0
			item["voucherValue"] = 3.5
			json.NewEncoder(w).Encode(map[string]any{"parkingItem": item})
			return
		}
		id := int64(item["tariffId"].(float64))
		a.quotes = append(a.quotes, id)
		item["price"] = float64(id) * 2.5
		json.NewEncoder(w).Encode(map[string]any{"parkingItem": item})
	default:
		http.NotFound(w, r)
	}
}

func (a *fakeAPI) set(plate string, s ...map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[plate] = s
}

func session(in string) map[string]any {
	return map[string]any{
		"plate": "HHKF860E", "areaGkz": 7, "quotaID": 3, "tariffId": 1,
		"quotaAddress": "Teststr. 1, Hamburg", "realUTC_IN": in, "realUTC_OUT": "2026-11-08 12:00:00", "price": 4.5,
	}
}

func setup(t *testing.T) (*Bot, *fakeTG, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{sessions: map[string][]map[string]any{}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fp := fairparken.New()
	fp.BaseURL = srv.URL + "/"
	tg := &fakeTG{}
	b := New(Config{PollInterval: time.Minute, MaxPlates: 2}, tg, fp, st)
	return b, tg, api
}

func TestMonitorLifecycle(t *testing.T) {
	ctx := context.Background()
	b, tg, api := setup(t)
	b.handleText(ctx, 1, "/add HH-KF 860E Golf")
	b.handleText(ctx, 2, "/add hhkf860e")
	tg.take()

	b.pollAll(ctx)
	if m := tg.take(); len(m) != 0 {
		t.Fatalf("no session, expected no push, got %v", m)
	}

	api.set("HHKF860E", session("2026-11-08 10:00:00"))
	b.pollAll(ctx)
	m := tg.take()
	if len(m) != 2 || !strings.Contains(m[0].text, "Neuer Parkvorgang") || !strings.Contains(m[0].text, "Golf") {
		t.Fatalf("expected push to both chats, got %+v", m)
	}
	if !strings.Contains(m[0].text, "4,50 €") || !strings.Contains(m[0].text, "11:00") {
		t.Errorf("unexpected formatting: %s", m[0].text)
	}
	if m[0].kb == nil || m[0].kb.InlineKeyboard[0][0].URL != "https://pay.fairparken.com/plate/HHKF860E" {
		t.Errorf("missing pay button: %+v", m[0].kb)
	}

	b.pollAll(ctx)
	if m := tg.take(); len(m) != 0 {
		t.Fatalf("same session must not be pushed twice, got %v", m)
	}

	// A manual check by chat 1 finds a new session: chat 1 gets the answer, chat 2 the push.
	api.set("HHKF860E", session("2026-11-08 10:00:00"), session("2026-11-08 11:00:00"))
	b.handleText(ctx, 1, "/check")
	m = tg.take()
	var c1, c2 int
	for _, x := range m {
		switch x.chat {
		case 1:
			c1++
		case 2:
			c2++
			if !strings.Contains(x.text, "Neuer Parkvorgang") {
				t.Errorf("chat 2 should get a push: %s", x.text)
			}
		}
	}
	if c1 != 2 || c2 != 1 {
		t.Fatalf("chat1=%d chat2=%d: %+v", c1, c2, m)
	}

	api.set("HHKF860E")
	b.pollAll(ctx)
	m = tg.take()
	if len(m) != 4 || !strings.Contains(m[0].text, "ist bezahlt") {
		t.Fatalf("expected closed notification for 2 sessions x 2 chats, got %+v", m)
	}

	b.handleText(ctx, 2, "/remove HHKF860E")
	tg.take()
	api.set("HHKF860E", session("2026-11-09 10:00:00"))
	b.pollAll(ctx)
	if m := tg.take(); len(m) != 1 || m[0].chat != 1 {
		t.Fatalf("removed chat must not get pushes: %+v", m)
	}
}

func TestMaxPlatesAndValidation(t *testing.T) {
	ctx := context.Background()
	b, tg, _ := setup(t)
	b.handleText(ctx, 1, "/add AB1")
	b.handleText(ctx, 1, "/add CD2")
	tg.take()
	b.handleText(ctx, 1, "/add EF3")
	if m := tg.take(); len(m) != 1 || !strings.Contains(m[0].text, "Maximal 2") {
		t.Fatalf("expected limit message, got %+v", m)
	}
	b.handleText(ctx, 1, "/add 12")
	if m := tg.take(); len(m) != 1 || !strings.Contains(m[0].text, "Bitte so") {
		t.Fatalf("expected usage, got %+v", m)
	}
	b.handleText(ctx, 3, "/list")
	if m := tg.take(); !strings.Contains(m[0].text, "Noch keine") {
		t.Fatalf("users must be separate: %+v", m)
	}
}

func TestTariffs(t *testing.T) {
	ctx := context.Background()
	b, tg, api := setup(t)
	b.handleText(ctx, 1, "/add HHKF860E")
	tg.take()

	b.handleText(ctx, 1, "/tarife")
	if m := tg.take(); !strings.Contains(m[0].text, "noch kein Parkplatz bekannt") {
		t.Fatalf("got %+v", m)
	}

	api.set("HHKF860E", session("2026-11-08 10:00:00"))
	b.handleText(ctx, 1, "/tarife")
	m := tg.take()
	if len(m) != 1 || !strings.Contains(m[0].text, "Auto (aktuell): <b>2,50 €</b>") || !strings.Contains(m[0].text, "Wohnmobil: <b>5,00 €</b>") {
		t.Fatalf("got %+v", m)
	}
	if q := api.quotes; len(q) != 3 || q[2] != 1 {
		t.Errorf("original tariff must be restored last, quotes=%v", q)
	}

	// Quota is remembered after the session is gone.
	api.set("HHKF860E")
	b.handleText(ctx, 1, "/tarife")
	m = tg.take()
	if len(m) != 2 || !strings.Contains(m[0].text, "ist bezahlt") ||
		!strings.Contains(m[1].text, "Wohnmobil") || !strings.Contains(m[1].text, "Teststr. 1") {
		t.Fatalf("got %+v", m)
	}
}

func TestReminder(t *testing.T) {
	ctx := context.Background()
	b, tg, api := setup(t)
	b.cfg.RemindBefore = 12 * time.Hour
	b.handleText(ctx, 1, "/add HHKF860E")
	out := time.Now().UTC().Add(-40 * time.Hour)
	s := session(out.Add(-time.Hour).Format("2006-01-02 15:04:05"))
	s["realUTC_OUT"] = out.Format("2006-01-02 15:04:05")
	api.set("HHKF860E", s)
	b.pollAll(ctx)
	tg.take()
	b.pollAll(ctx)
	m := tg.take()
	if len(m) != 1 || !strings.Contains(m[0].text, "Noch nicht bezahlt") {
		t.Fatalf("expected reminder, got %+v", m)
	}
	b.pollAll(ctx)
	if m := tg.take(); len(m) != 0 {
		t.Fatalf("reminder only once, got %+v", m)
	}
}

func TestPlainPlateIsCheck(t *testing.T) {
	ctx := context.Background()
	b, tg, _ := setup(t)
	b.handleText(ctx, 1, "hh-kf 860e")
	m := tg.take()
	if len(m) != 1 || !strings.Contains(m[0].text, "kein offener Parkvorgang") || m[0].kb == nil {
		t.Fatalf("got %+v", m)
	}
}

func TestSplitPlateLabel(t *testing.T) {
	for in, want := range map[string][2]string{
		"HH-KF 860E Golf":      {"HHKF860E", "Golf"},
		"HHKF860E":             {"HHKF860E", ""},
		"hh kf 860e Mein Auto": {"HHKF860E", "Mein Auto"},
		"":                     {"", ""},
	} {
		p, l := splitPlateLabel(in)
		if p != want[0] || l != want[1] {
			t.Errorf("%q: got %q %q", in, p, l)
		}
	}
}
