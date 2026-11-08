package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/niondir/parkingapp/internal/store"
)

const heartbeatInterval = 30 * time.Second

func (b *Bot) runHeartbeat(ctx context.Context) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		if err := b.store.Heartbeat(ctx, time.Now()); err != nil && ctx.Err() == nil {
			slog.Warn("heartbeat", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// recoverDowntime checks how long the bot was offline. After a long downtime
// it drops the Telegram updates queued in the meantime and tells all users.
// It returns the update offset to continue with.
func (b *Bot) recoverDowntime(ctx context.Context) int64 {
	if b.cfg.OfflineNotice <= 0 {
		return 0
	}
	last, err := b.store.LastAlive(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return 0 // first start
	}
	if err != nil {
		slog.Error("last alive", "err", err)
		return 0
	}
	gap := time.Since(last)
	if gap < b.cfg.OfflineNotice {
		return 0
	}
	slog.Info("was offline", "since", last, "gap", gap.Round(time.Second))

	var offset int64
	// offset -1 returns only the newest update; confirming it drops all older ones.
	if upd, err := b.tg.GetUpdates(ctx, -1, 0); err != nil {
		slog.Warn("drop pending updates", "err", err)
	} else if len(upd) > 0 {
		offset = upd[len(upd)-1].UpdateID + 1
		slog.Info("dropped pending updates", "up_to", offset-1)
	}

	chats, err := b.store.Chats(ctx)
	if err != nil {
		slog.Error("list chats", "err", err)
	}
	seen := map[int64]bool{}
	for _, ids := range [][]int64{chats, keys(b.cfg.Admins), keys(b.cfg.AllowedUsers)} {
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			b.reply(ctx, id, offlineText(last, gap), nil)
		}
	}
	return offset
}

func offlineText(since time.Time, gap time.Duration) string {
	return fmt.Sprintf("⚠️ Ich war seit %s weg (%s). In der Zeit habe ich keine Meldungen verschickt, "+
		"und Befehle aus dieser Zeit sind verworfen — falls du etwas geschickt hast, bitte nochmal.",
		since.In(berlin).Format("02.01. 15:04"), fmtGap(gap))
}

func fmtGap(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d Minuten", int(d.Round(time.Minute)/time.Minute))
	case d < 48*time.Hour:
		return strings.Replace(fmt.Sprintf("%.1f Stunden", d.Hours()), ".", ",", 1)
	default:
		return strings.Replace(fmt.Sprintf("%.1f Tage", d.Hours()/24), ".", ",", 1)
	}
}

func keys(m map[int64]bool) []int64 {
	var r []int64
	for k := range m {
		r = append(r, k)
	}
	return r
}
