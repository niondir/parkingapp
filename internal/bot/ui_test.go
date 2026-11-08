package bot

import (
	"context"
	"strings"
	"testing"
	"time"
)

// press simulates a button in message msgID.
func press(b *Bot, chat, msgID int64, data string) {
	b.handleCallback(context.Background(), chat, msgID, data)
}

func buttons(m sent) []string {
	var r []string
	if m.kb == nil {
		return nil
	}
	for _, row := range m.kb.InlineKeyboard {
		for _, b := range row {
			if b.CallbackData != "" {
				r = append(r, b.CallbackData)
			} else {
				r = append(r, b.URL)
			}
		}
	}
	return r
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestAddFlowWithButtons(t *testing.T) {
	ctx := context.Background()
	b, tg, _ := setup(t)

	b.handleText(ctx, 1, "/start")
	m := tg.take()
	if len(m) != 2 || m[0].markup != replyKeyboard || !has(buttons(m[1]), "add") {
		t.Fatalf("welcome: %+v", m)
	}

	press(b, 1, 10, "add")
	m = tg.take()
	if len(m) != 1 || !strings.Contains(m[0].text, "Schick mir das Kennzeichen") || !has(buttons(m[0]), "x") {
		t.Fatalf("prompt: %+v", m)
	}
	// Invalid input is asked again, the question stays open.
	b.handleText(ctx, 1, "??")
	if m := tg.take(); len(m) != 1 || !strings.Contains(m[0].text, "nicht wie ein Kennzeichen") {
		t.Fatalf("retry: %+v", m)
	}

	b.handleText(ctx, 1, "HH-KF 860E")
	m = tg.take()
	if len(m) != 2 || m[0].edit == 0 || m[0].kb != nil || !strings.Contains(m[1].text, "Wie soll <b>HHKF860E</b> heißen") {
		t.Fatalf("label question, old prompt without buttons: %+v", m)
	}

	b.handleText(ctx, 1, "  Golf  ")
	m = tg.take()
	if !strings.Contains(texts(m), "✅ <b>HHKF860E</b> (Golf) hinzugefügt") {
		t.Fatalf("added: %s", texts(m))
	}
	if b.getPending(1) != nil {
		t.Fatal("question must be closed")
	}

	// "Ohne Namen" button.
	press(b, 1, 10, "add")
	b.handleText(ctx, 1, "B-XY 1")
	tg.take()
	press(b, 1, 11, "lbl:")
	if m := tg.take(); !strings.Contains(texts(m), "✅ <b>BXY1</b> hinzugefügt") {
		t.Fatalf("added without label: %s", texts(m))
	}

	// Plate list and plate menu are edited in place.
	press(b, 1, 20, "ml")
	m = tg.take()
	if len(m) != 1 || m[0].edit != 20 || !has(buttons(m[0]), "p:HHKF860E") || !strings.Contains(strings.Join(buttonTexts(m[0]), "|"), "🚗 HHKF860E · Golf") {
		t.Fatalf("list: %+v", m)
	}
	press(b, 1, 20, "p:HHKF860E")
	m = tg.take()
	for _, want := range []string{"c:HHKF860E", "https://pay.fairparken.com/plate/HHKF860E", "t:HHKF860E", "v:HHKF860E", "s:HHKF860E", "hist:HHKF860E", "rn:HHKF860E", "r:HHKF860E", "ml"} {
		if !has(buttons(m[0]), want) {
			t.Errorf("plate menu misses %s: %v", want, buttons(m[0]))
		}
	}
}

func buttonTexts(m sent) []string {
	var r []string
	for _, row := range m.kb.InlineKeyboard {
		for _, b := range row {
			r = append(r, b.Text)
		}
	}
	return r
}

func TestCancel(t *testing.T) {
	ctx := context.Background()
	b, tg, _ := setup(t)
	press(b, 1, 10, "add")
	tg.take()
	press(b, 1, 99, "x")
	m := tg.take()
	if len(m) != 1 || !strings.Contains(m[0].text, "Abgebrochen") || m[0].kb != nil {
		t.Fatalf("cancel: %+v", m)
	}
	// The next text is no longer taken as answer.
	b.handleText(ctx, 1, "hallo")
	if m := tg.take(); !strings.Contains(m[0].text, "Hauptmenü") {
		t.Fatalf("got %s", texts(m))
	}

	// Menu button and commands cancel too.
	press(b, 1, 10, "add")
	tg.take()
	b.handleText(ctx, 1, menuButton)
	if m := tg.take(); len(m) != 2 || !strings.Contains(m[0].text, "Abgebrochen") || !strings.Contains(m[1].text, "Hauptmenü") {
		t.Fatalf("got %s", texts(m))
	}
	press(b, 1, 10, "add")
	tg.take()
	b.handleText(ctx, 1, "/list")
	if b.getPending(1) != nil {
		t.Fatal("command must cancel the question")
	}
}

func TestSettingsMenus(t *testing.T) {
	ctx := context.Background()
	b, tg, _ := setup(t)
	b.handleText(ctx, 1, "/add HHKF860E")
	tg.take()

	press(b, 1, 30, "s:HHKF860E")
	m := tg.take()
	if m[0].edit != 30 || !has(buttons(m[0]), "pl:HHKF860E") || !has(buttons(m[0]), "p:HHKF860E") {
		t.Fatalf("settings: %+v", m)
	}

	press(b, 1, 30, "pl:HHKF860E")
	if m := tg.take(); !has(buttons(m[0]), "plv:HHKF860E:10") || !has(buttons(m[0]), "pli:HHKF860E") {
		t.Fatalf("price menu: %v", buttons(m[0]))
	}
	press(b, 1, 30, "plv:HHKF860E:10")
	if m := tg.take(); m[0].edit != 30 || !strings.Contains(m[0].text, "Preisalarm ab: 10,00 €") {
		t.Fatalf("got %s", texts(m))
	}

	press(b, 1, 30, "pli:HHKF860E")
	tg.take()
	b.handleText(ctx, 1, "abc")
	tg.take()
	b.handleText(ctx, 1, "12,50 €")
	if m := tg.take(); !strings.Contains(texts(m), "Preisalarm ab: 12,50 €") {
		t.Fatalf("got %s", texts(m))
	}

	press(b, 1, 30, "dlv:HHKF860E:90")
	if m := tg.take(); !strings.Contains(m[0].text, "Parkdauer-Alarm ab: 1 h 30 min") {
		t.Fatalf("got %s", texts(m))
	}
	press(b, 1, 30, "dli:HHKF860E")
	b.handleText(ctx, 1, "2,5")
	if m := tg.take(); !strings.Contains(texts(m), "Parkdauer-Alarm ab: 2 h 30 min") {
		t.Fatalf("got %s", texts(m))
	}

	press(b, 1, 30, "q:HHKF860E")
	if m := tg.take(); !has(buttons(m[0]), "qv:HHKF860E:22-7") {
		t.Fatalf("quiet menu: %v", buttons(m[0]))
	}
	press(b, 1, 30, "qv:HHKF860E:22-7")
	if m := tg.take(); !strings.Contains(m[0].text, "22–7 Uhr") {
		t.Fatalf("got %s", texts(m))
	}
	press(b, 1, 30, "qv:HHKF860E:aus")
	if m := tg.take(); !strings.Contains(m[0].text, "Ruhezeit (alle Kennzeichen): aus") {
		t.Fatalf("got %s", texts(m))
	}

	p, _ := b.store.GetPlate(ctx, 1, "HHKF860E")
	if p.PriceLimit != 12.5 || p.DurationLimit != 150*time.Minute {
		t.Fatalf("stored: %+v", p.PlateSettings)
	}
}

func TestRenameRemoveVoucher(t *testing.T) {
	ctx := context.Background()
	b, tg, api := setup(t)
	b.handleText(ctx, 1, "/add HHKF860E Alt")
	tg.take()

	press(b, 1, 40, "rn:HHKF860E")
	tg.take()
	b.handleText(ctx, 1, "Neu")
	if m := tg.take(); !strings.Contains(texts(m), "<b>HHKF860E</b> (Neu)") {
		t.Fatalf("got %s", texts(m))
	}
	press(b, 1, 40, "rn:HHKF860E")
	press(b, 1, 41, "rnx:HHKF860E")
	if p, _ := b.store.GetPlate(ctx, 1, "HHKF860E"); p.Label != "" {
		t.Fatalf("label %q", p.Label)
	}
	tg.take()

	api.set("HHKF860E", session("2026-11-08 10:00:00"))
	press(b, 1, 40, "v:HHKF860E")
	tg.take()
	b.handleText(ctx, 1, "RABATT")
	if m := tg.take(); !strings.Contains(texts(m), "Mit Code: <b>1,00 €</b>") {
		t.Fatalf("got %s", texts(m))
	}

	press(b, 1, 40, "r:HHKF860E")
	m := tg.take()
	if m[0].edit != 40 || !has(buttons(m[0]), "rx:HHKF860E") {
		t.Fatalf("confirm: %+v", m)
	}
	if ok, _ := b.store.HasPlate(ctx, 1, "HHKF860E"); !ok {
		t.Fatal("removed without confirmation")
	}
	press(b, 1, 40, "rx:HHKF860E")
	tg.take()
	if ok, _ := b.store.HasPlate(ctx, 1, "HHKF860E"); ok {
		t.Fatal("not removed")
	}
}

func TestBroadcastFlow(t *testing.T) {
	ctx := context.Background()
	b, tg, _ := setup(t)
	b.cfg.Admins = map[int64]bool{100: true}
	b.handleText(ctx, 1, "/add HHKF860E")
	tg.take()

	b.handle(ctx, msg(100, "/menu"))
	if m := tg.take(); !has(buttons(m[0]), "ab") {
		t.Fatalf("admin menu: %v", buttons(m[0]))
	}
	b.handle(ctx, msg(1, "/menu"))
	if m := tg.take(); has(buttons(m[0]), "ab") {
		t.Fatal("admin buttons for normal user")
	}

	b.handle(ctx, click(100, "ab"))
	tg.take()
	b.handle(ctx, msg(100, "Wartung"))
	m := tg.take()
	if len(m) != 2 || !strings.Contains(m[1].text, "An 2 Chats senden?") || !has(buttons(m[1]), "abx") {
		t.Fatalf("preview: %+v", m)
	}
	b.handle(ctx, click(1, "abx"))
	if got := byChat(tg.take(), 1); len(got) != 0 {
		t.Fatalf("normal user triggered broadcast: %+v", got)
	}
	b.handle(ctx, click(100, "abx"))
	m = tg.take()
	if got := byChat(m, 1); len(got) != 1 || got[0].text != "📢 Wartung" {
		t.Fatalf("broadcast: %+v", m)
	}
}
