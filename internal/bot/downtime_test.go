package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/niondir/parkingapp/internal/store"
	"github.com/niondir/parkingapp/internal/telegram"
)

func TestRecoverDowntime(t *testing.T) {
	ctx := context.Background()
	b, tg, _ := setup(t)
	b.cfg.OfflineNotice = 5 * time.Minute
	b.cfg.Admins = map[int64]bool{100: true}

	if off := b.recoverDowntime(ctx); off != 0 || len(tg.take()) != 0 {
		t.Fatal("first start must be silent")
	}

	b.handleText(ctx, 1, "/add HHKF860E")
	b.handleText(ctx, 1, "/add AB123")
	b.store.SetUserStatus(ctx, 2, store.UserAllowed)
	b.store.SetUserStatus(ctx, 3, store.UserBlocked)
	tg.take()

	// Short restart: nothing dropped, nobody informed.
	b.store.Heartbeat(ctx, time.Now().Add(-time.Minute))
	tg.pending = []telegram.Update{{UpdateID: 41}, {UpdateID: 42}}
	if off := b.recoverDowntime(ctx); off != 0 || len(tg.take()) != 0 {
		t.Fatalf("short restart: offset %d", off)
	}

	since := time.Date(2026, 11, 8, 10, 9, 0, 0, time.UTC)
	b.store.Heartbeat(ctx, since)
	off := b.recoverDowntime(ctx)
	if off != 43 {
		t.Fatalf("pending updates must be confirmed, offset %d", off)
	}
	m := tg.take()
	got := map[int64]bool{}
	for _, x := range m {
		got[x.chat] = true
		if !strings.Contains(x.text, "Ich war seit 08.11. 11:09 weg (") || !strings.Contains(x.text, "bitte nochmal") {
			t.Errorf("text: %s", x.text)
		}
	}
	if len(m) != 3 || !got[1] || !got[2] || !got[100] {
		t.Fatalf("expected one notice each for chats 1, 2 and admin 100, got %+v", m)
	}
}

func TestFmtGap(t *testing.T) {
	for d, want := range map[time.Duration]string{
		7 * time.Minute:               "7 Minuten",
		2*time.Hour + 18*time.Minute:  "2,3 Stunden",
		3*24*time.Hour + 12*time.Hour: "3,5 Tage",
	} {
		if got := fmtGap(d); got != want {
			t.Errorf("%v: got %q want %q", d, got, want)
		}
	}
}
