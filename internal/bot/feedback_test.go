package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/niondir/parkingapp/internal/store"
	"github.com/niondir/parkingapp/internal/telegram"
)

func TestFeedbackFlow(t *testing.T) {
	ctx := context.Background()
	b, tg, _ := setup(t)
	const admin, user = 100, 1
	b.cfg.Admins = map[int64]bool{admin: true}
	b.store.SetUserStatus(ctx, user, store.UserAllowed)

	// Name is taken from the Telegram user.
	b.handle(ctx, telegram.Update{CallbackQuery: &telegram.CallbackQuery{ID: "q",
		From: telegram.User{ID: user, FirstName: "Tobi", Username: "niondir"}, Data: "fb",
		Message: &telegram.Message{MessageID: 5, Chat: telegram.Chat{ID: user}}}})
	if m := tg.take(); !has(buttons(m[0]), "fbn") || !has(buttons(m[0]), "fbm") {
		t.Fatalf("feedback menu: %v", buttons(m[0]))
	}
	b.handle(ctx, click(user, "fbn"))
	tg.take()
	b.handle(ctx, msg(user, "Die Karte fehlt <b>noch</b>"))
	m := tg.take()
	u, a := byChat(m, user), byChat(m, admin)
	if len(u) != 2 || !strings.Contains(u[1].text, "Gespeichert als <b>#1</b>") {
		t.Fatalf("user: %+v", u)
	}
	if len(a) != 1 || !strings.Contains(a[0].text, "Neues Feedback") || !strings.Contains(a[0].text, "Von <code>1</code>") ||
		!strings.Contains(a[0].text, "&lt;b&gt;noch") || !has(buttons(a[0]), "fbs:1:done") || !has(buttons(a[0]), "fbr:1") {
		t.Fatalf("admin: %+v", a)
	}

	// Directly via command, too.
	b.handle(ctx, msg(user, "/feedback Zweites"))
	tg.take()

	// Admin inbox lists open feedback; the menu shows the count.
	b.handle(ctx, msg(admin, "/menu"))
	if m := tg.take(); !strings.Contains(strings.Join(buttonTexts(m[0]), "|"), "📥 Feedback (2)") {
		t.Fatalf("admin menu: %v", buttonTexts(m[0]))
	}
	b.handle(ctx, click(admin, "fba"))
	if m := tg.take(); !has(buttons(m[0]), "fbd:1") || !has(buttons(m[0]), "fbd:2") {
		t.Fatalf("inbox: %v", buttons(m[0]))
	}

	// Status change informs the user.
	b.handle(ctx, click(admin, "fbs:1:progress"))
	m = tg.take()
	if u := byChat(m, user); len(u) != 1 || !strings.Contains(u[0].text, "#1</b> ist jetzt: 🔧 in Arbeit") {
		t.Fatalf("user not informed: %+v", m)
	}

	// Reply goes to the user and is stored.
	b.handle(ctx, click(admin, "fbr:1"))
	tg.take()
	b.handle(ctx, msg(admin, "Kommt mit dem nächsten Update"))
	m = tg.take()
	if u := byChat(m, user); len(u) != 1 || !strings.Contains(u[0].text, "Antwort auf dein Feedback #1") {
		t.Fatalf("reply: %+v", m)
	}
	b.handle(ctx, click(admin, "fbs:1:done"))
	tg.take()

	b.handle(ctx, click(user, "fbm"))
	m = tg.take()
	if !strings.Contains(m[0].text, "#1</b> · ") || !strings.Contains(m[0].text, "✅ erledigt") || !strings.Contains(m[0].text, "Kommt mit dem nächsten Update") ||
		!strings.Contains(m[0].text, "#2</b>") {
		t.Fatalf("own feedback: %s", m[0].text)
	}
	b.handle(ctx, click(admin, "fba"))
	if m := tg.take(); has(buttons(m[0]), "fbd:1") || !has(buttons(m[0]), "fbd:2") {
		t.Fatalf("done feedback must leave the open list: %v", buttons(m[0]))
	}

	// Users can't change status or reply.
	b.handle(ctx, click(user, "fbs:2:rejected"))
	b.handle(ctx, click(user, "fbr:2"))
	tg.take()
	if f, _ := b.store.GetFeedback(ctx, 2); f.Status != store.FeedbackNew {
		t.Fatalf("status changed by user: %s", f.Status)
	}
	if p := b.getPending(user); p != nil {
		t.Fatal("user got a reply prompt")
	}
}

func TestUsageStats(t *testing.T) {
	ctx := context.Background()
	b, tg, api := setup(t)
	b.cfg.Admins = map[int64]bool{100: true}
	b.store.SetUserStatus(ctx, 1, store.UserAllowed)
	b.store.SetUserStatus(ctx, 2, store.UserAllowed)

	b.handle(ctx, msg(1, "/add HHKF860E"))
	b.handle(ctx, click(1, "chk"))
	b.handle(ctx, click(2, "c:HHKF860E"))
	b.handle(ctx, msg(2, "HH-KF 860E"))
	b.handle(ctx, click(1, "add"))
	b.handle(ctx, msg(1, "B-XY 1"))
	api.set("HHKF860E", liveSession(time.Now().Add(-5*time.Minute)))
	b.pollAll(ctx)
	b.handle(ctx, msg(100, "/stats"))
	tg.take()

	b.handle(ctx, msg(100, "/nutzung 7"))
	m := tg.take()
	text := m[0].text
	for _, want := range []string{"letzte 7 Tage", "Aktive Nutzer: 2 (7 Tage)", "Prüfen: 2 · 2", "Kennzeichen hinzufügen: 3 · 1",
		"Prüfen (Kennzeichen geschickt): 1 · 1", "Push: neuer Parkvorgang: 1 · 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "/stats") {
		t.Errorf("admin features must be hidden:\n%s", text)
	}
	if !has(buttons(m[0]), "us:90") {
		t.Errorf("period buttons: %v", buttons(m[0]))
	}
}

func TestAGBHints(t *testing.T) {
	in := time.Now().Add(-30 * time.Minute)
	it := mustItem(t, liveSession(in))
	if s := fmtItem(it, "", time.Now(), 15*time.Minute); !strings.Contains(s, "Nach dem Bezahlen hast du 15 min zum Ausfahren") {
		t.Errorf("after grace: %s", s)
	}
	it = mustItem(t, liveSession(time.Now().Add(-5*time.Minute)))
	if s := fmtItem(it, "", time.Now(), 15*time.Minute); strings.Contains(s, "Nach dem Bezahlen") || !strings.Contains(s, "Karenzzeit 15 min") {
		t.Errorf("within grace: %s", s)
	}
}
