package bot

import (
	"context"
	"strings"
	"testing"

	"github.com/niondir/parkingapp/internal/telegram"
)

func msg(user int64, text string) telegram.Update {
	return telegram.Update{Message: &telegram.Message{
		From: &telegram.User{ID: user, FirstName: "U" + text[:1]},
		Chat: telegram.Chat{ID: user, Type: "private"},
		Text: text,
	}}
}

func click(user int64, data string) telegram.Update {
	return telegram.Update{CallbackQuery: &telegram.CallbackQuery{
		ID: "q", From: telegram.User{ID: user}, Data: data,
		Message: &telegram.Message{Chat: telegram.Chat{ID: user}},
	}}
}

func byChat(m []sent, chat int64) []sent {
	var r []sent
	for _, x := range m {
		if x.chat == chat {
			r = append(r, x)
		}
	}
	return r
}

func TestAdminApproval(t *testing.T) {
	ctx := context.Background()
	b, tg, _ := setup(t)
	const admin, user = 100, 200
	b.cfg.Admins = map[int64]bool{admin: true}

	b.handle(ctx, msg(admin, "/list"))
	if m := tg.take(); len(m) != 1 || !strings.Contains(m[0].text, "Noch keine") {
		t.Fatalf("admin must have access: %+v", m)
	}

	// First message creates a request and notifies the admin.
	b.handle(ctx, msg(user, "/start"))
	m := tg.take()
	if u := byChat(m, user); len(u) != 1 || !strings.Contains(u[0].text, "angefragt") {
		t.Fatalf("user reply: %+v", m)
	}
	a := byChat(m, admin)
	if len(a) != 1 || !strings.Contains(a[0].text, "Zugangsanfrage") || a[0].kb.InlineKeyboard[0][0].CallbackData != "ua:200" {
		t.Fatalf("admin notification: %+v", m)
	}

	// Repeated messages don't spam the admin.
	b.handle(ctx, msg(user, "/list"))
	if m := tg.take(); len(m) != 1 || m[0].chat != user || !strings.Contains(m[0].text, "wartet") {
		t.Fatalf("got %+v", m)
	}

	// Normal users can't use admin actions.
	b.handle(ctx, click(user, "ua:200"))
	tg.take()
	if ok, _ := b.allowed(ctx, user); ok {
		t.Fatal("user approved himself")
	}

	b.handle(ctx, click(admin, "ua:200"))
	m = tg.take()
	if u := byChat(m, user); len(u) != 3 || !strings.Contains(u[0].text, "freigeschaltet") || u[1].markup != replyKeyboard ||
		!strings.Contains(u[2].text, "Hauptmenü") {
		t.Fatalf("user must get welcome and menu: %+v", m)
	}
	b.handle(ctx, msg(user, "/add HHKF860E"))
	if m := tg.take(); !strings.Contains(m[0].text, "hinzugefügt") {
		t.Fatalf("approved user: %+v", m)
	}

	b.handle(ctx, msg(admin, "/users"))
	if m := tg.take(); len(m) != 1 || !strings.Contains(m[0].text, "✅ <code>200</code>") || !strings.Contains(m[0].text, "1 Kennzeichen") {
		t.Fatalf("users: %+v", m)
	}

	b.handle(ctx, msg(admin, "/block 200"))
	m = tg.take()
	if u := byChat(m, user); len(u) != 1 || !strings.Contains(u[0].text, "entzogen") {
		t.Fatalf("user not informed: %+v", m)
	}
	if n, _ := b.store.CountPlates(ctx, user); n != 0 {
		t.Fatalf("plates of blocked user must be removed, got %d", n)
	}
	b.handle(ctx, msg(user, "/list"))
	if m := tg.take(); len(m) != 1 || !strings.Contains(m[0].text, "Kein Zugriff") {
		t.Fatalf("blocked: %+v", m)
	}

	b.handle(ctx, msg(admin, "/block 100"))
	if m := tg.take(); !strings.Contains(m[0].text, "ADMIN_USER_IDS") {
		t.Fatalf("admin must not block admins: %+v", m)
	}
}

func TestOpenWithoutAdmins(t *testing.T) {
	ctx := context.Background()
	b, tg, _ := setup(t)
	b.handle(ctx, msg(5, "/list"))
	if m := tg.take(); !strings.Contains(m[0].text, "Noch keine") {
		t.Fatalf("got %+v", m)
	}
	b.handle(ctx, msg(5, "/users"))
	if m := tg.take(); !strings.Contains(m[0].text, "Unbekannter Befehl") {
		t.Fatalf("admin commands only for admins: %+v", m)
	}
}
