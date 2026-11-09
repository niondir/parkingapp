package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/niondir/parkingapp/internal/fairparken"
	"github.com/niondir/parkingapp/internal/store"
	"github.com/niondir/parkingapp/internal/telegram"
)

// --- settings ---

func onOff(v bool) string {
	if v {
		return "an"
	}
	return "aus"
}

func fmtQuiet(cs store.ChatSettings) string {
	if cs.QuietFrom == cs.QuietTo {
		return "aus"
	}
	return fmt.Sprintf("%d–%d Uhr (Meldungen ohne Ton)", cs.QuietFrom, cs.QuietTo)
}

func settingsText(p store.Plate, cs store.ChatSettings) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "⚙️ <b>Einstellungen</b> für %s\n\n", plateTitle(p.Plate, p.Label))
	if p.Paused {
		sb.WriteString("Status: ⏸ <b>pausiert</b> – keine Meldungen\n")
	} else {
		sb.WriteString("Status: ▶️ aktiv\n")
	}
	fmt.Fprintf(&sb, "Erinnerung vor Ablauf der Zahlungsfrist: %s\n", onOff(p.Remind))
	fmt.Fprintf(&sb, "Jede Preiserhöhung melden: %s\n", onOff(p.PriceChange))
	if p.PriceLimit > 0 {
		fmt.Fprintf(&sb, "Preisalarm ab: %s\n", fmtEuro(p.PriceLimit))
	} else {
		sb.WriteString("Preisalarm: aus\n")
	}
	if p.DurationLimit > 0 {
		fmt.Fprintf(&sb, "Parkdauer-Alarm ab: %s\n", fmtDuration(p.DurationLimit))
	} else {
		sb.WriteString("Parkdauer-Alarm: aus\n")
	}
	fmt.Fprintf(&sb, "Ruhezeit (alle Kennzeichen): %s", fmtQuiet(cs))
	return sb.String()
}

func settingsKeyboard(p store.Plate) *telegram.InlineKeyboard {
	pause := "⏸ Pausieren"
	if p.Paused {
		pause = "▶️ Fortsetzen"
	}
	return &telegram.InlineKeyboard{InlineKeyboard: [][]telegram.InlineButton{
		{{Text: pause, CallbackData: "sp:" + p.Plate}},
		{{Text: "🔔 Frist-Erinnerung " + onOff(!p.Remind) + "schalten", CallbackData: "sr:" + p.Plate}},
		{{Text: "📈 Preiserhöhungen " + onOff(!p.PriceChange) + "schalten", CallbackData: "sc:" + p.Plate}},
		{{Text: "🔔 Alarme", CallbackData: "al:" + p.Plate}},
		{{Text: "🌙 Ruhezeit", CallbackData: "q:" + p.Plate}, {Text: "⬅️ Zurück", CallbackData: "p:" + p.Plate}},
	}}
}

// choosePlate resolves the plate argument of a settings command. With no
// argument and several plates it asks with buttons using callback prefix cb.
func (b *Bot) choosePlate(ctx context.Context, chatID int64, args, cb, question string) (store.Plate, bool) {
	if p := fairparken.NormalizePlate(args); p != "" {
		pl, err := b.store.GetPlate(ctx, chatID, p)
		if errors.Is(err, store.ErrNotFound) {
			b.reply(ctx, chatID, "<b>"+esc(p)+"</b> ist nicht gespeichert. /list zeigt deine Kennzeichen.", nil)
			return pl, false
		}
		if err != nil {
			b.fail(ctx, chatID, "Laden", err)
			return pl, false
		}
		return pl, true
	}
	plates, err := b.store.Plates(ctx, chatID)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return store.Plate{}, false
	}
	switch len(plates) {
	case 0:
		b.reply(ctx, chatID, "Noch keine Kennzeichen. Hinzufügen mit <code>/add HH-AB123 Name</code>", nil)
		return store.Plate{}, false
	case 1:
		return plates[0], true
	}
	kb := &telegram.InlineKeyboard{}
	for _, p := range plates {
		kb.InlineKeyboard = append(kb.InlineKeyboard, []telegram.InlineButton{{Text: p.Plate + " " + p.Label, CallbackData: cb + ":" + p.Plate}})
	}
	b.reply(ctx, chatID, question, kb)
	return store.Plate{}, false
}

func (b *Bot) cmdSettings(ctx context.Context, chatID int64, args string) {
	p, ok := b.choosePlate(ctx, chatID, args, "s", "Einstellungen für welches Kennzeichen?")
	if !ok {
		return
	}
	cs, err := b.store.GetChatSettings(ctx, chatID)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	b.reply(ctx, chatID, settingsText(p, cs), settingsKeyboard(p))
}

func (b *Bot) settingsCallback(ctx context.Context, chatID, msgID int64, kind, plate string) {
	p, err := b.store.GetPlate(ctx, chatID, plate)
	if err != nil {
		b.reply(ctx, chatID, "<b>"+esc(plate)+"</b> ist nicht gespeichert.", nil)
		return
	}
	switch kind {
	case "sp":
		p.Paused = !p.Paused
	case "sr":
		p.Remind = !p.Remind
	case "sc":
		p.PriceChange = !p.PriceChange
	}
	if err := b.store.SetPlateSettings(ctx, chatID, plate, p.PlateSettings); err != nil {
		b.fail(ctx, chatID, "Speichern", err)
		return
	}
	cs, err := b.store.GetChatSettings(ctx, chatID)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	if err := b.tg.Edit(ctx, chatID, msgID, settingsText(p, cs), settingsKeyboard(p)); err != nil {
		slog.Warn("edit settings", "err", err)
		b.reply(ctx, chatID, settingsText(p, cs), settingsKeyboard(p))
	}
}

type limitKind int

const (
	limitPrice limitKind = iota
	limitDuration
)

// cmdLimit handles "/alarm [PLATE] 10" and "/dauer [PLATE] 3".
func (b *Bot) cmdLimit(ctx context.Context, chatID int64, args string, kind limitKind) {
	usage := "Bitte so: <code>/alarm HHAB123 10</code> (Euro) oder <code>/alarm HHAB123 aus</code>"
	if kind == limitDuration {
		usage = "Bitte so: <code>/dauer HHAB123 3</code> (Stunden, auch <code>1,5</code> oder <code>90m</code>) oder <code>/dauer HHAB123 aus</code>"
	}
	words := strings.Fields(args)
	if len(words) == 0 {
		b.reply(ctx, chatID, usage, nil)
		return
	}
	value := strings.ToLower(words[len(words)-1])
	p, ok := b.choosePlate(ctx, chatID, strings.Join(words[:len(words)-1], " "), "s",
		"Du hast mehrere Kennzeichen – bitte mit angeben. "+usage+"\n\nOder Einstellungen öffnen:")
	if !ok {
		return
	}
	switch kind {
	case limitPrice:
		v, ok := parsePrice(value)
		if !ok {
			b.reply(ctx, chatID, usage, nil)
			return
		}
		p.PriceLimit = v
	case limitDuration:
		d, ok := parseDurationValue(value)
		if !ok {
			b.reply(ctx, chatID, usage, nil)
			return
		}
		p.DurationLimit = d
	}
	if err := b.store.SetPlateSettings(ctx, chatID, p.Plate, p.PlateSettings); err != nil {
		b.fail(ctx, chatID, "Speichern", err)
		return
	}
	cs, _ := b.store.GetChatSettings(ctx, chatID)
	b.reply(ctx, chatID, "✅ Gespeichert.\n\n"+settingsText(p, cs), settingsKeyboard(p))
}

func (b *Bot) cmdQuiet(ctx context.Context, chatID int64, args string) {
	args = strings.ToLower(strings.TrimSpace(args))
	cs, err := b.store.GetChatSettings(ctx, chatID)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	usage := "Bitte so: <code>/ruhezeit 22-7</code> (Uhrzeiten in vollen Stunden) oder <code>/ruhezeit aus</code>.\nIn der Ruhezeit kommen Meldungen ohne Ton."
	switch {
	case args == "":
		b.reply(ctx, chatID, "🌙 Ruhezeit: "+fmtQuiet(cs)+"\n\n"+usage, nil)
		return
	case args == "aus" || args == "off":
		cs = store.ChatSettings{}
	default:
		var ok bool
		if cs, ok = parseQuiet(args); !ok {
			b.reply(ctx, chatID, usage, nil)
			return
		}
	}
	if err := b.store.SetChatSettings(ctx, chatID, cs); err != nil {
		b.fail(ctx, chatID, "Speichern", err)
		return
	}
	b.reply(ctx, chatID, "🌙 Ruhezeit: "+fmtQuiet(cs), nil)
}

func isOff(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "aus" || v == "off" || v == "0"
}

// parsePrice parses "10", "7,50" or "12 €". "aus" returns 0.
func parsePrice(v string) (float64, bool) {
	if isOff(v) {
		return 0, true
	}
	v = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "€"))
	f, err := strconv.ParseFloat(strings.Replace(v, ",", ".", 1), 64)
	if err != nil || f <= 0 || f > 10000 {
		return 0, false
	}
	return f, true
}

// parseDurationValue parses hours ("3", "1,5", "2h") or Go durations ("90m"). "aus" returns 0.
func parseDurationValue(v string) (time.Duration, bool) {
	if isOff(v) {
		return 0, true
	}
	v = strings.ToLower(strings.ReplaceAll(v, " ", ""))
	var d time.Duration
	if h, err := strconv.ParseFloat(strings.Replace(strings.TrimSuffix(v, "h"), ",", ".", 1), 64); err == nil {
		d = time.Duration(h * float64(time.Hour))
	} else if pd, err := time.ParseDuration(v); err == nil {
		d = pd
	}
	if d < time.Minute || d > 30*24*time.Hour {
		return 0, false
	}
	return d.Round(time.Minute), true
}

// parseQuiet parses "22-7" (hours). "aus" turns quiet hours off.
func parseQuiet(v string) (store.ChatSettings, bool) {
	if isOff(v) {
		return store.ChatSettings{}, true
	}
	v = strings.ToLower(strings.ReplaceAll(v, " ", ""))
	v = strings.ReplaceAll(v, "–", "-")
	from, to, ok := strings.Cut(v, "-")
	f, err1 := strconv.Atoi(strings.TrimSuffix(from, "uhr"))
	t, err2 := strconv.Atoi(strings.TrimSuffix(to, "uhr"))
	if !ok || err1 != nil || err2 != nil || f < 0 || f > 23 || t < 0 || t > 23 || f == t {
		return store.ChatSettings{}, false
	}
	return store.ChatSettings{QuietFrom: f, QuietTo: t}, true
}

// --- history ---

func (b *Bot) cmdHistory(ctx context.Context, chatID int64, args string) {
	filter := fairparken.NormalizePlate(args)
	now := time.Now().In(berlin)
	since := time.Date(now.Year(), now.Month()-5, 1, 0, 0, 0, 0, berlin)
	sessions, err := b.store.History(ctx, chatID, since)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	type month struct {
		count, free int
		sum         float64
	}
	months := map[string]*month{}
	var lines []string
	for _, s := range sessions {
		if filter != "" && s.Plate != filter {
			continue
		}
		var it fairparken.ParkingItem
		_ = json.Unmarshal([]byte(s.Item), &it)
		start := s.FirstSeen
		if in, ok := it.In(); ok {
			start = in
		}
		mk := start.In(berlin).Format("2006-01")
		m := months[mk]
		if m == nil {
			m = &month{}
			months[mk] = m
		}
		m.count++
		var price float64
		if it.Price != nil {
			price = *it.Price
		}
		state, end := "", time.Now()
		switch sessionState(s, it) {
		case stateRunning:
			state = " · <b>läuft</b>"
		case stateDue:
			m.sum += price
			state = " · " + fmtEuro(price) + " <b>offen</b>"
			end = s.LastSeen
		case store.OutcomePaid:
			m.sum += price
			state = " · " + fmtEuro(price)
			end = s.LastSeen
		default:
			m.free++
			state = " · ohne Gebühr"
			end = s.LastSeen
		}
		if len(lines) < 10 {
			line := fmt.Sprintf("• %s · %s · %s%s", start.In(berlin).Format("02.01. 15:04"), esc(s.Plate),
				fmtDuration(parkedFor(it, end)), state)
			if loc := it.Location(); loc != "" {
				line += "\n   " + esc(loc)
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		b.reply(ctx, chatID, "📒 Noch keine Parkvorgänge erfasst. Der Verlauf beginnt, sobald der Bot einen Parkvorgang eines deiner Kennzeichen sieht.", nil)
		return
	}
	var sb strings.Builder
	sb.WriteString("📒 <b>Letzte Parkvorgänge</b>\n\n")
	sb.WriteString(strings.Join(lines, "\n"))
	keys := make([]string, 0, len(months))
	for k := range months {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	sb.WriteString("\n\n💶 <b>Kosten pro Monat</b>\n")
	for _, k := range keys {
		m := months[k]
		t, _ := time.Parse("2006-01", k)
		fmt.Fprintf(&sb, "\n%s: %d × · %s", t.Format("01/2006"), m.count, fmtEuro(m.sum))
		if m.free > 0 {
			fmt.Fprintf(&sb, " (%d ohne Gebühr)", m.free)
		}
	}
	sb.WriteString("\n\n<i>„Ohne Gebühr“: ausgefahren, ohne dass etwas offen war – in der Freiparkzeit oder vorher am Automaten bezahlt. Gebühr = zuletzt vom Bot gesehener Betrag.</i>")
	b.reply(ctx, chatID, sb.String(), nil)
}

const (
	stateRunning = "running" // still parked
	stateDue     = "due"     // left, fee not paid yet
)

// sessionState is stateRunning, stateDue or the outcome of a closed session.
func sessionState(s store.Session, it fairparken.ParkingItem) string {
	if !s.Closed {
		if s.ExitedAt.IsZero() && it.Parked(s.LastSeen) {
			return stateRunning
		}
		return stateDue
	}
	if s.Outcome != "" {
		return s.Outcome
	}
	// Closed before outcomes were stored: derive it the same way.
	if it.Parked(s.LastSeen) {
		return store.OutcomeNoFee
	}
	return store.OutcomePaid
}

// --- voucher ---

func (b *Bot) cmdVoucher(ctx context.Context, chatID int64, args string) {
	code, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	if code == "" || len(code) > 64 {
		b.reply(ctx, chatID, "Bitte so: <code>/rabatt CODE</code> oder <code>/rabatt CODE HHAB123</code>\nZeigt den Preis eines offenen Parkvorgangs mit Rabattcode.", nil)
		return
	}
	plates, err := b.targetPlates(ctx, chatID, rest)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	found := false
	for _, p := range plates {
		res, err := b.fp.Search(ctx, p.Plate)
		if err != nil {
			b.fail(ctx, chatID, "Abfrage für "+p.Plate, err)
			continue
		}
		for _, r := range res {
			found = true
			it := r.AnprResult
			q, err := b.fp.QuoteVoucher(ctx, it, code)
			// The web client uses the same call; reset in case the backend remembers the code.
			if _, rerr := b.fp.QuoteVoucher(ctx, it, ""); rerr != nil {
				slog.Warn("reset voucher", "err", rerr)
			}
			if err != nil {
				slog.Info("voucher rejected", "plate", p.Plate, "err", err)
				b.reply(ctx, chatID, "❌ Code <code>"+esc(code)+"</code> wurde für "+plateTitle(p.Plate, p.Label)+" nicht angenommen.", nil)
				continue
			}
			msg := fmt.Sprintf("🎟 Rabattcode <code>%s</code> für %s\n", esc(code), plateTitle(p.Plate, p.Label))
			if it.Price != nil {
				msg += "\nOhne Code: " + fmtEuro(*it.Price)
			}
			if q.Price != nil {
				msg += "\nMit Code: <b>" + fmtEuro(*q.Price) + "</b>"
			}
			if q.VoucherValue != nil && *q.VoucherValue != 0 {
				msg += "\nRabatt: " + fmtEuro(*q.VoucherValue)
			}
			if it.Price != nil && q.Price != nil && *q.Price == *it.Price {
				msg += "\n\nDer Code ändert den Preis nicht (ungültig oder nicht für diesen Parkplatz)."
			}
			msg += "\n\nDen Code beim Bezahlen auf der Bezahlseite eingeben."
			b.reply(ctx, chatID, msg, itemKeyboard(p.Plate))
		}
	}
	if !found {
		b.reply(ctx, chatID, "Kein offener Parkvorgang – Rabattcodes lassen sich nur für einen offenen Parkvorgang prüfen.", nil)
	}
}

// --- receipt ---

func (b *Bot) cmdReceipt(ctx context.Context, chatID int64) {
	b.reply(ctx, chatID, `🧾 <b>Vertragsstrafe mit Kassenzettel anfechten</b>

Hast du von fair parken eine Zahlungsaufforderung bekommen, obwohl du z. B. im Markt eingekauft hast? Dann kannst du auf der offiziellen Seite eine Stornierungsanfrage stellen:

1. Aktenzeichen und Kennzeichen aus dem Schreiben eingeben
2. Foto vom Kassenzettel hochladen
3. Kontaktdaten angeben und absenden

Das geht nur direkt auf der Seite (Captcha), nicht über den Bot.`,
		&telegram.InlineKeyboard{InlineKeyboard: [][]telegram.InlineButton{{{Text: "🧾 Kassenzettel hochladen", URL: fairparken.ReceiptURL}}}})
}
