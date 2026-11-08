package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/niondir/parkingapp/internal/fairparken"
	"github.com/niondir/parkingapp/internal/store"
)

// runMonitor polls all watched plates and pushes changes to the subscribers.
func (b *Bot) runMonitor(ctx context.Context) {
	for {
		b.pollAll(ctx)
		// Jitter so we don't hit the API in a fixed rhythm.
		wait := b.cfg.PollInterval + time.Duration(rand.Int64N(int64(b.cfg.PollInterval/5)+1))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (b *Bot) pollAll(ctx context.Context) {
	plates, err := b.store.WatchedPlates(ctx)
	if err != nil {
		slog.Error("list plates", "err", err)
		return
	}
	for i, plate := range plates {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(b.cfg.RequestGap):
			}
		}
		res, err := b.fp.Search(ctx, plate)
		if err != nil {
			// Don't touch the state on errors, otherwise open sessions would be reported as closed.
			slog.Warn("search", "plate", plate, "err", err)
			continue
		}
		if err := b.process(ctx, plate, res, 0); err != nil {
			slog.Error("process", "plate", plate, "err", err)
		}
	}
}

// process compares a fresh search result with the stored state and notifies
// all subscribers about new and finished sessions. The chat skipChat already
// got the result as a direct answer and gets no push for new sessions.
func (b *Bot) process(ctx context.Context, plate string, res []fairparken.SearchResult, skipChat int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	open, err := b.store.OpenSessions(ctx, plate)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, r := range res {
		it := r.AnprResult
		key := it.Key()
		seen[key] = true
		raw, _ := json.Marshal(it.Raw)
		created, err := b.store.UpsertSession(ctx, plate, key, string(raw))
		if err != nil {
			return err
		}
		if it.QuotaID != 0 {
			if err := b.store.SetPlateInfo(ctx, plate, it.QuotaID, it.QuotaAddress); err != nil {
				return err
			}
		}
		if created {
			slog.Info("new session", "plate", plate, "key", key)
			b.notify(ctx, plate, skipChat, func(label string) string {
				return "🅿️ <b>Neuer Parkvorgang erkannt</b>\n\n" + fmtItem(it, label)
			}, true)
			continue
		}
		b.maybeRemind(ctx, plate, key, it, open)
	}
	for _, s := range open {
		if seen[s.Key] {
			continue
		}
		if err := b.store.CloseSession(ctx, plate, s.Key); err != nil {
			return err
		}
		slog.Info("session gone", "plate", plate, "key", s.Key)
		var it fairparken.ParkingItem
		_ = json.Unmarshal([]byte(s.Item), &it)
		b.notify(ctx, plate, 0, func(label string) string {
			msg := "✅ Parkvorgang von " + plateTitle(plate, label) + " ist nicht mehr offen (bezahlt oder ausgebucht)."
			if it.QuotaAddress != "" {
				msg += "\n📍 " + esc(it.QuotaAddress)
			}
			return msg
		}, false)
	}
	return nil
}

// maybeRemind warns once if a session is still unpaid shortly before the
// online payment window closes. The exit time only stays constant once the
// car has left, so a running session never triggers this.
func (b *Bot) maybeRemind(ctx context.Context, plate, key string, it fairparken.ParkingItem, open []store.Session) {
	out, ok := it.Out()
	if !ok || b.cfg.RemindBefore <= 0 {
		return
	}
	for _, s := range open {
		if s.Key != key || s.ReminderSent {
			continue
		}
		left := time.Until(out.Add(PayWindow))
		if left > b.cfg.RemindBefore || left < 0 {
			return
		}
		if err := b.store.MarkReminderSent(ctx, plate, key); err != nil {
			slog.Error("mark reminder", "err", err)
			return
		}
		b.notify(ctx, plate, 0, func(label string) string {
			return fmt.Sprintf("⚠️ <b>Noch nicht bezahlt</b> – nur noch ca. %s Zeit.\n\n%s", fmtDuration(left), fmtItem(it, label))
		}, true)
		return
	}
}

func (b *Bot) notify(ctx context.Context, plate string, skipChat int64, text func(label string) string, withButtons bool) {
	subs, err := b.store.Subscribers(ctx, plate)
	if err != nil {
		slog.Error("subscribers", "plate", plate, "err", err)
		return
	}
	for _, s := range subs {
		if s.ChatID == skipChat {
			continue
		}
		var err error
		if withButtons {
			err = b.tg.Send(ctx, s.ChatID, text(s.Label), itemKeyboard(plate))
		} else {
			err = b.tg.Send(ctx, s.ChatID, text(s.Label), nil)
		}
		if err != nil {
			slog.Warn("send push", "chat", s.ChatID, "plate", plate, "err", err)
		}
	}
}
