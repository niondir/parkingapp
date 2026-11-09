package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/niondir/parkingapp/internal/fairparken"
	"github.com/niondir/parkingapp/internal/store"
)

// pollStats are shown to admins with /stats.
type pollStats struct {
	Started      time.Time
	LastPoll     time.Time
	LastDuration time.Duration
	Searches     int
	Errors       int
	LastError    string
}

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
	start := time.Now()
	plates, err := b.store.WatchedPlates(ctx)
	if err != nil {
		slog.Error("list plates", "err", err)
		b.pingHealth(ctx, false)
		return
	}
	var failed int
	for i, plate := range plates {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(b.cfg.RequestGap):
			}
		}
		res, err := b.fp.Search(ctx, plate)
		b.statsMu.Lock()
		b.stats.Searches++
		if err != nil {
			b.stats.Errors++
			b.stats.LastError = time.Now().In(berlin).Format("02.01. 15:04") + " " + err.Error()
		}
		b.statsMu.Unlock()
		if err != nil {
			// Don't touch the state on errors, otherwise open sessions would be reported as closed.
			slog.Warn("search", "plate", plate, "err", err)
			failed++
			continue
		}
		if err := b.process(ctx, plate, res, 0); err != nil {
			slog.Error("process", "plate", plate, "err", err)
		}
	}
	b.statsMu.Lock()
	b.stats.LastPoll = start
	b.stats.LastDuration = time.Since(start)
	b.statsMu.Unlock()
	b.pingHealth(ctx, len(plates) == 0 || failed < len(plates))
}

// pingHealth reports to an external monitor like healthchecks.io, which
// alerts when the pings stop or "/fail" is called.
func (b *Bot) pingHealth(ctx context.Context, ok bool) {
	if b.cfg.HealthcheckURL == "" {
		return
	}
	u := b.cfg.HealthcheckURL
	if !ok {
		u += "/fail"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		slog.Warn("healthcheck", "err", err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Warn("healthcheck", "err", err)
		return
	}
	resp.Body.Close()
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
	prev := map[string]store.Session{}
	for _, s := range open {
		prev[s.Key] = s
	}
	now := b.clock()
	seen := map[string]bool{}
	for _, r := range res {
		it := r.AnprResult
		key := it.Key()
		seen[key] = true
		raw, _ := json.Marshal(it.Raw)
		created, reopened, err := b.store.UpsertSession(ctx, plate, key, string(raw))
		if err != nil {
			return err
		}
		if it.QuotaID != 0 {
			if err := b.store.SetPlateInfo(ctx, plate, it.QuotaID, it.QuotaAddress); err != nil {
				return err
			}
		}
		switch {
		case created:
			slog.Info("new session", "plate", plate, "key", key)
			b.notify(ctx, plate, skipChat, nil, func(p store.Plate) string {
				return "🅿️ <b>Neuer Parkvorgang erkannt</b>\n\n" + fmtResult(r, p.Label, now)
			}, true)
		case reopened:
			// It was reported as finished, but the API returns it again.
			slog.Warn("session reappeared", "plate", plate, "key", key)
			b.notify(ctx, plate, 0, nil, func(p store.Plate) string {
				return "⚠️ <b>Korrektur:</b> Der Parkvorgang ist doch noch offen.\n\n" + fmtResult(r, p.Label, now)
			}, true)
		}
		s, had := prev[key]
		var old fairparken.ParkingItem
		if had {
			_ = json.Unmarshal([]byte(s.Item), &old)
		}
		switch {
		case (created || reopened) && !it.Parked(now):
			// First seen after leaving; the message above already shows the open fee.
			if err := b.markExited(ctx, plate, key, it); err != nil {
				return err
			}
		case had && s.ExitedAt.IsZero() && exitConfirmed(old, it, s.LastSeen, now):
			if err := b.markExited(ctx, plate, key, it); err != nil {
				return err
			}
			slog.Info("exit", "plate", plate, "key", key, "price", it.Price)
			if it.Price != nil && *it.Price > 0 {
				b.notify(ctx, plate, 0, nil, func(p store.Plate) string {
					return "🚙 <b>Ausfahrt erkannt</b> – es ist noch eine Gebühr offen.\n\n" + fmtResult(r, p.Label, now)
				}, true)
			}
		}
		// Alerts compare the state of the last check with now. A new session
		// counts from zero, so limits already exceeded are reported right away.
		lastCheck := time.Time{}
		if had {
			lastCheck = s.LastSeen
		}
		b.checkAlerts(ctx, plate, it, old, lastCheck, now)
		if !created {
			b.maybeRemind(ctx, plate, key, it, open)
		}
	}
	for _, s := range open {
		if seen[s.Key] {
			continue
		}
		// One empty answer may be a hiccup or the backend processing the exit.
		n, err := b.store.MarkMissing(ctx, plate, s.Key)
		if err != nil {
			return err
		}
		if n < missingToClose {
			continue
		}
		var it fairparken.ParkingItem
		_ = json.Unmarshal([]byte(s.Item), &it)
		// Gone while still parked: the camera saw the car leave without a fee
		// due. Gone after an exit with a fee due: it was paid.
		outcome := store.OutcomeNoFee
		if !s.ExitedAt.IsZero() && it.Price != nil && *it.Price > 0 {
			outcome = store.OutcomePaid
		}
		if err := b.store.CloseSession(ctx, plate, s.Key, outcome); err != nil {
			return err
		}
		slog.Info("session gone", "plate", plate, "key", s.Key, "outcome", outcome)
		b.notify(ctx, plate, 0, nil, func(p store.Plate) string {
			return fmtClosed(it, p.Label, outcome, s.LastSeen)
		}, false)
	}
	return nil
}

// missingToClose is the number of consecutive checks without a session
// before it counts as finished.
const missingToClose = 2

// exitConfirmed reports whether the car has left: the exit time stayed the
// same over two checks at least two minutes apart and is in the past. While
// parked, the API moves it forward every minute.
func exitConfirmed(old, cur fairparken.ParkingItem, lastCheck, now time.Time) bool {
	return cur.RealUTCOut != "" && old.RealUTCOut == cur.RealUTCOut &&
		now.Sub(lastCheck) >= 2*time.Minute && !cur.Parked(now)
}

func (b *Bot) markExited(ctx context.Context, plate, key string, it fairparken.ParkingItem) error {
	out, ok := it.Out()
	if !ok {
		out = b.clock()
	}
	return b.store.MarkExited(ctx, plate, key, out)
}

// parkedFor is the parking duration at time t. Once the car has left, the
// exit time is fixed and the duration stops growing.
func parkedFor(it fairparken.ParkingItem, t time.Time) time.Duration {
	in, ok := it.In()
	if !ok {
		return 0
	}
	if out, ok := it.Out(); ok && out.Before(t) && out.After(in) {
		t = out
	}
	return t.Sub(in)
}

func (b *Bot) checkAlerts(ctx context.Context, plate string, it, old fairparken.ParkingItem, lastCheck, now time.Time) {
	var oldPrice, newPrice float64
	if old.Price != nil {
		oldPrice = *old.Price
	}
	if it.Price != nil {
		newPrice = *it.Price
	}
	oldDur := time.Duration(0)
	if !lastCheck.IsZero() {
		oldDur = parkedFor(it, lastCheck)
	}
	newDur := parkedFor(it, now)
	isNew := lastCheck.IsZero()

	b.notify(ctx, plate, 0, func(p store.Plate) bool {
		return p.PriceLimit > 0 && it.Price != nil && oldPrice < p.PriceLimit && newPrice >= p.PriceLimit
	}, func(p store.Plate) string {
		return fmt.Sprintf("💸 <b>Preisalarm</b>: Die Gebühr hat %s erreicht.\n\n%s", fmtEuro(p.PriceLimit), fmtItem(it, p.Label, now))
	}, true)

	if !isNew && it.Price != nil && old.Price != nil && newPrice > oldPrice {
		b.notify(ctx, plate, 0, func(p store.Plate) bool {
			// The price alert above already covers the step over the limit.
			return p.PriceChange && !(p.PriceLimit > 0 && oldPrice < p.PriceLimit && newPrice >= p.PriceLimit)
		}, func(p store.Plate) string {
			return fmt.Sprintf("📈 Gebühr für %s gestiegen: %s → <b>%s</b>", plateTitle(plate, p.Label), fmtEuro(oldPrice), fmtEuro(newPrice))
		}, true)
	}

	b.notify(ctx, plate, 0, func(p store.Plate) bool {
		return p.DurationLimit > 0 && oldDur < p.DurationLimit && newDur >= p.DurationLimit
	}, func(p store.Plate) string {
		return fmt.Sprintf("⏱ <b>Parkdauer-Alarm</b>: %s parkt seit über %s.\n\n%s", plateTitle(plate, p.Label), fmtDuration(p.DurationLimit), fmtItem(it, p.Label, now))
	}, true)
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
		b.notify(ctx, plate, 0, func(p store.Plate) bool { return p.Remind }, func(p store.Plate) string {
			return fmt.Sprintf("⚠️ <b>Noch nicht bezahlt</b> – nur noch ca. %s Zeit.\n\n%s", fmtDuration(left), fmtItem(it, p.Label, time.Now()))
		}, true)
		return
	}
}

// notify pushes a message to every subscriber of a plate that hasn't paused
// it and passes filter (nil = all). During quiet hours pushes are silent.
func (b *Bot) notify(ctx context.Context, plate string, skipChat int64, filter func(store.Plate) bool,
	text func(store.Plate) string, withButtons bool) {
	subs, err := b.store.Subscribers(ctx, plate)
	if err != nil {
		slog.Error("subscribers", "plate", plate, "err", err)
		return
	}
	for _, s := range subs {
		if s.ChatID == skipChat || s.Paused || (filter != nil && !filter(s)) {
			continue
		}
		silent := b.quiet(ctx, s.ChatID, time.Now())
		var err error
		if withButtons {
			err = b.tg.Push(ctx, s.ChatID, text(s), itemKeyboard(plate), silent)
		} else {
			err = b.tg.Push(ctx, s.ChatID, text(s), nil, silent)
		}
		if err != nil {
			slog.Warn("send push", "chat", s.ChatID, "plate", plate, "err", err)
		}
	}
}

// quiet reports whether t is within the chat's quiet hours.
func (b *Bot) quiet(ctx context.Context, chatID int64, t time.Time) bool {
	cs, err := b.store.GetChatSettings(ctx, chatID)
	if err != nil {
		slog.Warn("chat settings", "chat", chatID, "err", err)
		return false
	}
	return inQuietHours(cs, t)
}

func inQuietHours(cs store.ChatSettings, t time.Time) bool {
	if cs.QuietFrom == cs.QuietTo {
		return false
	}
	h := t.In(berlin).Hour()
	if cs.QuietFrom < cs.QuietTo {
		return h >= cs.QuietFrom && h < cs.QuietTo
	}
	return h >= cs.QuietFrom || h < cs.QuietTo // over midnight
}
