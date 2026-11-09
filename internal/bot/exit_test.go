package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/niondir/parkingapp/internal/store"
)

func utc(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// liveSession looks like the API while the car is parked: OUT follows the current time.
func liveSession(in time.Time) map[string]any {
	s := session(utc(in))
	s["realUTC_OUT"] = utc(time.Now().Truncate(time.Minute))
	s["paidUntil"] = utc(in.Add(time.Hour))
	s["price"] = 3.0
	s["quotaAddition"] = "Kaufland"
	s["hasVoucher"] = true
	return s
}

func TestFreeExit(t *testing.T) {
	ctx := context.Background()
	b, tg, api := setup(t)
	b.handleText(ctx, 1, "/add HHKF860E")
	tg.take()

	in := time.Now().Add(-6 * time.Minute)
	api.set("HHKF860E", liveSession(in))
	b.pollAll(ctx)
	m := tg.take()
	if len(m) != 1 {
		t.Fatalf("got %s", texts(m))
	}
	for _, want := range []string{"Teststr. 1, Hamburg (Kaufland)", "Parkt seit:", "Bei Zahlung jetzt: <b>3,00 €</b> (deckt bis", "Freiparkzeit", "Rabattcode möglich"} {
		if !strings.Contains(m[0].text, want) {
			t.Errorf("missing %q in %s", want, m[0].text)
		}
	}
	if strings.Contains(m[0].text, "zahlbar bis") || strings.Contains(m[0].text, "Ausfahrt:") {
		t.Errorf("no exit while parked: %s", m[0].text)
	}

	b.pollAll(ctx)
	if m := tg.take(); len(m) != 0 {
		t.Fatalf("still parked, nothing new: %s", texts(m))
	}

	// Gone while parked: left without a fee. One empty answer isn't enough.
	api.set("HHKF860E")
	b.pollAll(ctx)
	if m := tg.take(); len(m) != 0 {
		t.Fatalf("got %s", texts(m))
	}
	b.pollAll(ctx)
	m = tg.take()
	if len(m) != 1 || !strings.Contains(m[0].text, "ohne dass eine Gebühr offen ist") || strings.Contains(m[0].text, "3,00 €") {
		t.Fatalf("got %s", texts(m))
	}

	b.handleText(ctx, 1, "/verlauf")
	m = tg.take()
	if !strings.Contains(m[0].text, "· ohne Gebühr") || !strings.Contains(m[0].text, "1 × · 0,00 € (1 ohne Gebühr)") {
		t.Fatalf("history: %s", m[0].text)
	}
}

func TestExitWithFee(t *testing.T) {
	ctx := context.Background()
	b, tg, api := setup(t)
	b.handleText(ctx, 1, "/add HHKF860E")
	tg.take()

	in := time.Now().Add(-2 * time.Hour)
	s := liveSession(in)
	api.set("HHKF860E", s)
	b.pollAll(ctx)
	tg.take()

	// The car left 10 minutes ago, the session stays: fee due. The exit time
	// must stay the same over two checks before the exit counts.
	s["realUTC_OUT"] = utc(time.Now().Add(-10 * time.Minute))
	s["price"] = 6.0
	api.set("HHKF860E", s)
	b.pollAll(ctx)
	if m := tg.take(); len(m) != 0 {
		t.Fatalf("exit after one check: %s", texts(m))
	}
	b.clock = func() time.Time { return time.Now().Add(3 * time.Minute) }
	b.pollAll(ctx)
	m := tg.take()
	if len(m) != 1 || !strings.Contains(m[0].text, "Ausfahrt erkannt") || !strings.Contains(m[0].text, "Offen: <b>6,00 €</b>") ||
		!strings.Contains(m[0].text, "zahlbar bis") || m[0].kb == nil {
		t.Fatalf("got %s", texts(m))
	}
	b.pollAll(ctx)
	if m := tg.take(); len(m) != 0 {
		t.Fatalf("exit only once: %s", texts(m))
	}

	b.handleText(ctx, 1, "/verlauf")
	if m := tg.take(); !strings.Contains(m[0].text, "6,00 € <b>offen</b>") {
		t.Fatalf("history: %s", m[0].text)
	}

	api.set("HHKF860E")
	b.pollAll(ctx)
	b.pollAll(ctx)
	m = tg.take()
	if len(m) != 1 || !strings.Contains(m[0].text, "ist bezahlt") || !strings.Contains(m[0].text, "6,00 €") {
		t.Fatalf("got %s", texts(m))
	}
	b.handleText(ctx, 1, "/verlauf")
	if m := tg.take(); !strings.Contains(m[0].text, "1 × · 6,00 €") || strings.Contains(m[0].text, "ohne Gebühr)") {
		t.Fatalf("history: %s", m[0].text)
	}
}

func TestLegacySessionState(t *testing.T) {
	in := time.Now().Add(-time.Hour)
	free := mustItem(t, liveSession(in))
	last := time.Now()
	if got := sessionState(store.Session{Closed: true, LastSeen: last}, free); got != store.OutcomeNoFee {
		t.Errorf("gone while parked: %s", got)
	}
	exited := liveSession(in)
	exited["realUTC_OUT"] = utc(last.Add(-20 * time.Minute))
	if got := sessionState(store.Session{Closed: true, LastSeen: last}, mustItem(t, exited)); got != store.OutcomePaid {
		t.Errorf("gone after exit: %s", got)
	}
}

func TestHiccupAndCorrection(t *testing.T) {
	ctx := context.Background()
	b, tg, api := setup(t)
	b.handleText(ctx, 1, "/add HHKF860E")
	tg.take()
	live := liveSession(time.Now().Add(-10 * time.Minute))
	api.set("HHKF860E", live)
	b.pollAll(ctx)
	tg.take()

	// A single empty answer in between changes nothing.
	api.set("HHKF860E")
	b.pollAll(ctx)
	api.set("HHKF860E", live)
	b.pollAll(ctx)
	if m := tg.take(); len(m) != 0 {
		t.Fatalf("got %s", texts(m))
	}

	// Reported as finished, then back: correction.
	api.set("HHKF860E")
	b.pollAll(ctx)
	b.pollAll(ctx)
	tg.take()
	api.set("HHKF860E", live)
	b.pollAll(ctx)
	m := tg.take()
	if len(m) != 1 || !strings.Contains(m[0].text, "Korrektur") || !strings.Contains(m[0].text, "doch noch offen") {
		t.Fatalf("got %s", texts(m))
	}
	b.handleText(ctx, 1, "/verlauf")
	if m := tg.take(); !strings.Contains(m[0].text, "<b>läuft</b>") {
		t.Fatalf("reopened session must be running again: %s", m[0].text)
	}
}

func TestExitWithoutFee(t *testing.T) {
	ctx := context.Background()
	b, tg, api := setup(t)
	b.handleText(ctx, 1, "/add HHKF860E")
	tg.take()
	s := liveSession(time.Now().Add(-10 * time.Minute))
	api.set("HHKF860E", s)
	b.pollAll(ctx)
	tg.take()

	// Exit with price 0: no "fee due" message, and gone means free, not paid.
	s["realUTC_OUT"] = utc(time.Now().Add(-5 * time.Minute))
	s["price"] = 0.0
	api.set("HHKF860E", s)
	b.pollAll(ctx)
	b.clock = func() time.Time { return time.Now().Add(3 * time.Minute) }
	b.pollAll(ctx)
	if m := tg.take(); len(m) != 0 {
		t.Fatalf("got %s", texts(m))
	}
	api.set("HHKF860E")
	b.pollAll(ctx)
	b.pollAll(ctx)
	if m := tg.take(); len(m) != 1 || !strings.Contains(m[0].text, "ohne dass eine Gebühr offen ist") {
		t.Fatalf("got %s", texts(m))
	}
}
