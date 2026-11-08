package bot

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/niondir/parkingapp/internal/fairparken"
	"github.com/niondir/parkingapp/internal/store"
)

func texts(m []sent) string {
	var sb strings.Builder
	for _, x := range m {
		sb.WriteString(x.text + "\n---\n")
	}
	return sb.String()
}

func TestAlerts(t *testing.T) {
	ctx := context.Background()
	b, tg, api := setup(t)
	b.handleText(ctx, 1, "/add HHKF860E")
	b.handleText(ctx, 1, "/alarm 5")
	b.handleText(ctx, 1, "/dauer HHKF860E 2")
	tg.take()
	p, _ := b.store.GetPlate(ctx, 1, "HHKF860E")
	if p.PriceLimit != 5 || p.DurationLimit != 2*time.Hour {
		t.Fatalf("settings not saved: %+v", p.PlateSettings)
	}

	in := time.Now().UTC().Add(-90 * time.Minute)
	s := session(in.Format("2006-01-02 15:04:05"))
	s["realUTC_OUT"] = time.Now().UTC().Format("2006-01-02 15:04:05")
	s["price"] = 3.0
	api.set("HHKF860E", s)
	b.pollAll(ctx)
	if m := tg.take(); len(m) != 1 || !strings.Contains(m[0].text, "Neuer Parkvorgang") {
		t.Fatalf("got %s", texts(m))
	}

	// Price rises below the limit: no alert (price change notifications are off).
	s["price"] = 4.0
	api.set("HHKF860E", s)
	b.pollAll(ctx)
	if m := tg.take(); len(m) != 0 {
		t.Fatalf("got %s", texts(m))
	}

	// Crossing the limit alerts once.
	s["price"] = 5.5
	api.set("HHKF860E", s)
	b.pollAll(ctx)
	if m := tg.take(); len(m) != 1 || !strings.Contains(m[0].text, "Preisalarm") || !strings.Contains(m[0].text, "5,00 €") {
		t.Fatalf("got %s", texts(m))
	}
	b.pollAll(ctx)
	if m := tg.take(); len(m) != 0 {
		t.Fatalf("alert repeated: %s", texts(m))
	}

	// Price change notifications.
	b.handleCallback(ctx, 1, 77, "sc:HHKF860E")
	if m := tg.take(); len(m) != 1 || m[0].edit != 77 || !strings.Contains(m[0].text, "Jede Preiserhöhung melden: an") {
		t.Fatalf("toggle must edit the settings message: %+v", m)
	}
	s["price"] = 6.0
	api.set("HHKF860E", s)
	b.pollAll(ctx)
	if m := tg.take(); len(m) != 1 || !strings.Contains(m[0].text, "5,50 € → <b>6,00 €</b>") {
		t.Fatalf("got %s", texts(m))
	}

	// Duration alert: the session is 90 min old, pretend the last check was before 2 h and now is after.
	s["realUTC_OUT"] = in.Add(3 * time.Hour).Format("2006-01-02 15:04:05")
	b.mu.Lock()
	b.checkAlerts(ctx, "HHKF860E", mustItem(t, s), mustItem(t, s), in.Add(119*time.Minute), in.Add(121*time.Minute))
	b.mu.Unlock()
	if m := tg.take(); len(m) != 1 || !strings.Contains(m[0].text, "Parkdauer-Alarm") || !strings.Contains(m[0].text, "2 h 0 min") {
		t.Fatalf("got %s", texts(m))
	}

	// Paused: nothing at all, and the plate is not polled.
	b.handleCallback(ctx, 1, 77, "sp:HHKF860E")
	tg.take()
	if w, _ := b.store.WatchedPlates(ctx); len(w) != 0 {
		t.Fatalf("paused plate polled: %v", w)
	}
	b.handleCallback(ctx, 1, 77, "sp:HHKF860E")
	tg.take()
}

func TestQuietHours(t *testing.T) {
	ctx := context.Background()
	b, tg, api := setup(t)
	b.handleText(ctx, 1, "/add HHKF860E")
	h := time.Now().In(berlin).Hour()
	b.handleText(ctx, 1, "/ruhezeit "+itoa(h)+"-"+itoa((h+1)%24))
	if m := tg.take(); !strings.Contains(m[len(m)-1].text, "ohne Ton") {
		t.Fatalf("got %s", texts(m))
	}
	api.set("HHKF860E", session("2026-11-08 10:00:00"))
	b.pollAll(ctx)
	if m := tg.take(); len(m) != 1 || !m[0].silent {
		t.Fatalf("push in quiet hours must be silent: %+v", m)
	}
	b.handleText(ctx, 1, "/ruhezeit 25-3")
	if m := tg.take(); !strings.Contains(m[0].text, "Bitte so") {
		t.Fatalf("got %s", texts(m))
	}

	for _, c := range []struct {
		from, to, hour int
		want           bool
	}{{22, 7, 23, true}, {22, 7, 3, true}, {22, 7, 7, false}, {22, 7, 12, false}, {8, 12, 9, true}, {0, 0, 9, false}} {
		tm := time.Date(2026, 11, 8, c.hour, 30, 0, 0, berlin)
		if got := inQuietHours(store.ChatSettings{QuietFrom: c.from, QuietTo: c.to}, tm); got != c.want {
			t.Errorf("%d-%d at %d: got %v", c.from, c.to, c.hour, got)
		}
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestHistory(t *testing.T) {
	ctx := context.Background()
	b, tg, api := setup(t)
	b.handleText(ctx, 1, "/add HHKF860E")
	tg.take()
	s1 := session(time.Now().UTC().Add(-3 * time.Hour).Format("2006-01-02 15:04:05"))
	s1["price"] = 3.0
	api.set("HHKF860E", s1)
	b.pollAll(ctx)
	api.set("HHKF860E")
	b.pollAll(ctx)
	s2 := session(time.Now().UTC().Add(-time.Hour).Format("2006-01-02 15:04:05"))
	s2["price"] = 2.0
	api.set("HHKF860E", s2)
	b.pollAll(ctx)
	tg.take()

	b.handleText(ctx, 1, "/verlauf")
	m := tg.take()
	if len(m) != 1 || strings.Count(m[0].text, "• ") != 2 || !strings.Contains(m[0].text, "<b>offen</b>") || !strings.Contains(m[0].text, "2 × · 5,00 €") {
		t.Fatalf("got %s", texts(m))
	}

	// A user who adds the plate later doesn't see older sessions.
	time.Sleep(1100 * time.Millisecond)
	b.handleText(ctx, 2, "/add HHKF860E")
	tg.take()
	b.handleText(ctx, 2, "/verlauf")
	if m := tg.take(); !strings.Contains(m[0].text, "Noch keine Parkvorgänge") {
		t.Fatalf("history leaked: %s", texts(m))
	}
}

func TestVoucherAndArticles(t *testing.T) {
	ctx := context.Background()
	b, tg, api := setup(t)
	b.handleText(ctx, 1, "/add HHKF860E")
	tg.take()
	b.handleText(ctx, 1, "/rabatt RABATT")
	if m := tg.take(); !strings.Contains(m[0].text, "Kein offener Parkvorgang") {
		t.Fatalf("got %s", texts(m))
	}
	api.articles = []any{map[string]any{"name": "Tageskarte_Erwachsener", "price": map[string]any{"amount": 300, "currency": "EUR"}}}
	api.set("HHKF860E", session("2026-11-08 10:00:00"))
	b.handleText(ctx, 1, "/rabatt RABATT")
	m := tg.take()
	if len(m) != 1 || !strings.Contains(m[0].text, "Ohne Code: 4,50 €") || !strings.Contains(m[0].text, "Mit Code: <b>1,00 €</b>") {
		t.Fatalf("got %s", texts(m))
	}
	b.handleText(ctx, 1, "/rabatt FALSCH")
	if m := tg.take(); !strings.Contains(m[0].text, "nicht angenommen") {
		t.Fatalf("got %s", texts(m))
	}
	b.handleText(ctx, 1, "/check")
	if m := tg.take(); !strings.Contains(texts(m), "Tageskarte Erwachsene: 3,00 €") {
		t.Fatalf("got %s", texts(m))
	}
	b.handleText(ctx, 1, "/kassenzettel")
	if m := tg.take(); m[0].kb.InlineKeyboard[0][0].URL != "https://kassenzettel.fairparken.com" {
		t.Fatalf("got %+v", m)
	}
}

func TestStatsBroadcastHealth(t *testing.T) {
	ctx := context.Background()
	b, tg, _ := setup(t)
	var pings, fails atomic.Int32
	hc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/fail") {
			fails.Add(1)
		} else {
			pings.Add(1)
		}
	}))
	defer hc.Close()
	b.cfg.HealthcheckURL = hc.URL + "/uuid"
	b.cfg.Admins = map[int64]bool{100: true}
	b.handleText(ctx, 1, "/add HHKF860E")
	b.pollAll(ctx)
	if pings.Load() != 1 || fails.Load() != 0 {
		t.Fatalf("pings %d fails %d", pings.Load(), fails.Load())
	}
	b.fp.BaseURL = "http://127.0.0.1:1/"
	b.pollAll(ctx)
	if fails.Load() != 1 {
		t.Fatalf("fails %d", fails.Load())
	}
	tg.take()

	b.handle(ctx, msg(100, "/stats"))
	if m := tg.take(); !strings.Contains(m[0].text, "Kennzeichen: 1") || !strings.Contains(m[0].text, "Fehler: 1") {
		t.Fatalf("got %s", texts(m))
	}
	b.handle(ctx, msg(1, "/stats"))
	if m := tg.take(); !strings.Contains(texts(m), "Unbekannter Befehl") && !strings.Contains(texts(m), "angefragt") {
		t.Fatalf("stats only for admins: %s", texts(m))
	}
	b.handle(ctx, msg(100, "/rundnachricht Wartung <heute>"))
	m := tg.take()
	got := map[int64]string{}
	for _, x := range m {
		got[x.chat] = x.text
	}
	if got[1] != "📢 Wartung &lt;heute&gt;" || !strings.Contains(got[100], "An 2 Chats") {
		t.Fatalf("got %+v", got)
	}
}

func TestMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE plates (chat_id INTEGER NOT NULL, plate TEXT NOT NULL, label TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL, PRIMARY KEY (chat_id, plate));
		INSERT INTO plates VALUES (1, 'HHKF860E', 'Golf', 1);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p, err := st.GetPlate(context.Background(), 1, "HHKF860E")
	if err != nil || p.Label != "Golf" || !p.Remind || p.Paused {
		t.Fatalf("got %+v %v", p, err)
	}
}

func mustItem(t *testing.T, s map[string]any) fairparken.ParkingItem {
	t.Helper()
	b, _ := json.Marshal(s)
	var it fairparken.ParkingItem
	if err := json.Unmarshal(b, &it); err != nil {
		t.Fatal(err)
	}
	return it
}
