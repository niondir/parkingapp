// Package bot implements the Telegram interface and the background monitor.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/niondir/parkingapp/internal/fairparken"
	"github.com/niondir/parkingapp/internal/store"
	"github.com/niondir/parkingapp/internal/telegram"
)

type Config struct {
	PollInterval time.Duration
	RequestGap   time.Duration // pause between two API requests while polling
	RemindBefore time.Duration // reminder before the online payment window closes, 0 = off
	MaxPlates    int           // per chat
	// After a downtime longer than this, users are informed and pending commands are dropped. 0 = off.
	OfflineNotice  time.Duration
	HealthcheckURL string         // pinged after every poll cycle, e.g. https://hc-ping.com/<uuid>
	AllowedUsers   map[int64]bool // always allowed, in addition to users approved by an admin
	Admins         map[int64]bool // may approve users; if none and AllowedUsers is empty, the bot is open
}

// messenger is the part of the Telegram API the bot uses.
type messenger interface {
	GetMe(ctx context.Context) (telegram.User, error)
	GetUpdates(ctx context.Context, offset int64, timeout int) ([]telegram.Update, error)
	Send(ctx context.Context, chatID int64, html string, kb *telegram.InlineKeyboard) error
	Push(ctx context.Context, chatID int64, html string, kb *telegram.InlineKeyboard, silent bool) error
	Edit(ctx context.Context, chatID, messageID int64, html string, kb *telegram.InlineKeyboard) error
	SendMsg(ctx context.Context, chatID int64, html string, markup any) (int64, error)
	AnswerCallback(ctx context.Context, id, text string) error
	SetCommands(ctx context.Context, cmds []telegram.BotCommand, chatID int64) error
}

type Bot struct {
	cfg   Config
	tg    messenger
	fp    *fairparken.Client
	store *store.Store
	mu    sync.Mutex // serializes state changes of the monitor

	statsMu sync.Mutex
	stats   pollStats

	pendMu sync.Mutex
	pend   map[int64]*pending // open questions per chat
}

func New(cfg Config, tg messenger, fp *fairparken.Client, st *store.Store) *Bot {
	return &Bot{cfg: cfg, tg: tg, fp: fp, store: st, stats: pollStats{Started: time.Now()}, pend: map[int64]*pending{}}
}

var commands = []telegram.BotCommand{
	{Command: "menu", Description: "Hauptmenü"},
	{Command: "check", Description: "Parkvorgänge jetzt prüfen"},
	{Command: "list", Description: "Meine Kennzeichen"},
	{Command: "add", Description: "Kennzeichen hinzufügen: /add HH-AB123 [Name]"},
	{Command: "remove", Description: "Kennzeichen entfernen"},
	{Command: "tarife", Description: "Tarife des Parkplatzes"},
	{Command: "bezahlen", Description: "Link zur Bezahlseite"},
	{Command: "verlauf", Description: "Letzte Parkvorgänge und Kosten"},
	{Command: "einstellungen", Description: "Benachrichtigungen und Alarme"},
	{Command: "rabatt", Description: "Preis mit Rabattcode: /rabatt CODE"},
	{Command: "kassenzettel", Description: "Vertragsstrafe mit Kassenzettel anfechten"},
	{Command: "help", Description: "Hilfe"},
}

// Run starts the monitor and processes Telegram updates until ctx is done.
func (b *Bot) Run(ctx context.Context) error {
	me, err := b.tg.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}
	slog.Info("bot started", "username", me.Username)
	if err := b.tg.SetCommands(ctx, commands, 0); err != nil {
		slog.Warn("setMyCommands", "err", err)
	}
	for id := range b.cfg.Admins {
		if err := b.tg.SetCommands(ctx, append(commands[:len(commands):len(commands)], adminCommands...), id); err != nil {
			slog.Warn("setMyCommands for admin", "admin", id, "err", err)
		}
	}

	offset := b.recoverDowntime(ctx)
	go b.runHeartbeat(ctx)
	go b.runMonitor(ctx)

	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	defer wg.Wait()
	for ctx.Err() == nil {
		updates, err := b.tg.GetUpdates(ctx, offset, 50)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			slog.Warn("getUpdates", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			sem <- struct{}{}
			wg.Add(1)
			go func(u telegram.Update) {
				defer func() { <-sem; wg.Done() }()
				defer func() {
					if r := recover(); r != nil {
						slog.Error("panic in handler", "panic", r)
					}
				}()
				hctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				b.handle(hctx, u)
			}(u)
		}
	}
	return nil
}

func (b *Bot) handle(ctx context.Context, u telegram.Update) {
	switch {
	case u.Message != nil && u.Message.From != nil:
		m := u.Message
		text := strings.TrimSpace(m.Text)
		if !b.checkAccess(ctx, m.Chat.ID, *m.From) {
			return
		}
		if b.isAdmin(m.From.ID) && b.handleAdminText(ctx, m.Chat.ID, m.From.ID, text) {
			return
		}
		b.handleText(ctx, m.Chat.ID, text)
	case u.CallbackQuery != nil && u.CallbackQuery.Message != nil:
		q := u.CallbackQuery
		if ok, err := b.allowed(ctx, q.From.ID); err != nil || !ok {
			_ = b.tg.AnswerCallback(ctx, q.ID, "Kein Zugriff")
			return
		}
		_ = b.tg.AnswerCallback(ctx, q.ID, "")
		if b.isAdmin(q.From.ID) && b.handleAdminCallback(ctx, q.Message.Chat.ID, q.From.ID, q.Data) {
			return
		}
		b.handleCallback(ctx, q.Message.Chat.ID, q.Message.MessageID, q.Data)
	}
}

func (b *Bot) reply(ctx context.Context, chatID int64, html string, kb *telegram.InlineKeyboard) {
	if err := b.tg.Send(ctx, chatID, html, kb); err != nil {
		slog.Warn("send", "chat", chatID, "err", err)
	}
}

func (b *Bot) fail(ctx context.Context, chatID int64, what string, err error) {
	slog.Error(what, "chat", chatID, "err", err)
	b.reply(ctx, chatID, "❌ "+esc(what)+" fehlgeschlagen. Bitte später erneut versuchen.", nil)
}

func (b *Bot) handleText(ctx context.Context, chatID int64, text string) {
	if text == "" {
		return
	}
	if text == menuButton {
		b.endPending(ctx, chatID, "✖️ Abgebrochen.")
		b.showMain(ctx, chatID, 0)
		return
	}
	if !strings.HasPrefix(text, "/") {
		if p := b.getPending(chatID); p != nil {
			b.handleInput(ctx, chatID, p, text)
			return
		}
		// A bare plate is a shortcut for /check <plate>. Plates contain digits,
		// so words like "Hallo" open the menu instead.
		if p := fairparken.NormalizePlate(text); validPlate(p) && strings.ContainsAny(p, "0123456789") && !strings.Contains(text, "\n") {
			b.cmdCheck(ctx, chatID, p)
			return
		}
		b.showMain(ctx, chatID, 0)
		return
	}
	// A command cancels an open question.
	b.endPending(ctx, chatID, "✖️ Abgebrochen.")
	cmd, args, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(strings.ToLower(cmd), "@") // /check@MyBot
	args = strings.TrimSpace(args)
	switch cmd {
	case "/start":
		b.sendWelcome(ctx, chatID)
	case "/menu", "/menue":
		b.showMain(ctx, chatID, 0)
	case "/help", "/hilfe":
		b.cmdHelp(ctx, chatID)
	case "/add":
		b.cmdAdd(ctx, chatID, args)
	case "/remove", "/rm":
		b.cmdRemove(ctx, chatID, args)
	case "/list":
		b.cmdList(ctx, chatID)
	case "/check":
		b.cmdCheck(ctx, chatID, args)
	case "/tarife", "/tariffs":
		b.cmdTariffs(ctx, chatID, args)
	case "/bezahlen", "/pay":
		b.cmdPay(ctx, chatID, args)
	case "/verlauf", "/history":
		b.cmdHistory(ctx, chatID, args)
	case "/einstellungen", "/settings":
		b.cmdSettings(ctx, chatID, args)
	case "/alarm":
		b.cmdLimit(ctx, chatID, args, limitPrice)
	case "/dauer":
		b.cmdLimit(ctx, chatID, args, limitDuration)
	case "/ruhezeit":
		b.cmdQuiet(ctx, chatID, args)
	case "/rabatt", "/voucher":
		b.cmdVoucher(ctx, chatID, args)
	case "/kassenzettel":
		b.cmdReceipt(ctx, chatID)
	default:
		b.reply(ctx, chatID, "Unbekannter Befehl. /help zeigt alle Befehle.", nil)
	}
}

func (b *Bot) handleCallback(ctx context.Context, chatID, msgID int64, data string) {
	kind, plate, _ := strings.Cut(data, ":")
	if b.handleMenuCallback(ctx, chatID, msgID, kind, plate) {
		return
	}
	switch kind {
	case "sp", "sr", "sc":
		b.settingsCallback(ctx, chatID, msgID, kind, plate)
	case "c":
		b.cmdCheck(ctx, chatID, plate)
	case "t":
		b.cmdTariffs(ctx, chatID, plate)
	case "a":
		b.cmdAdd(ctx, chatID, plate)
	}
}

func validPlate(p string) bool {
	n := len([]rune(p))
	if n < 3 || n > 12 {
		return false
	}
	hasLetter := false
	for _, r := range p {
		if r < '0' || r > '9' {
			hasLetter = true
		}
	}
	return hasLetter
}

func (b *Bot) cmdHelp(ctx context.Context, chatID int64) {
	b.reply(ctx, chatID, `🅿️ <b>fair parken Bot</b>

Ich prüfe alle paar Minuten, ob für deine Kennzeichen ein offener Parkvorgang auf einem fair-parken-Parkplatz (Kennzeichenerkennung, ohne Ticket) existiert, und schicke dir eine Nachricht mit Bezahl-Button.

<b>Bedienung:</b> Alles geht über „`+menuButton+`“ unten im Chat. Unter <i>Meine Kennzeichen</i> → Kennzeichen antippen findest du Prüfen, Bezahlen, Tarife, Rabattcode, Verlauf und die Einstellungen (Pause, Preis- und Dauer-Alarm, Ruhezeit).

Ein Kennzeichen einfach als Nachricht schicken prüft es sofort.
Bezahlt wird auf der offiziellen Seite pay.fairparken.com – der Bot sieht keine Zahlungsdaten.

<i>Für Tipp-Fans gibt es auch Befehle: /add, /check, /tarife, /verlauf, /alarm, /dauer, /ruhezeit, /rabatt, /kassenzettel.</i>`, kb(row(btn("🏠 Hauptmenü", "m"))))
}

func (b *Bot) cmdAdd(ctx context.Context, chatID int64, args string) {
	plate, label := splitPlateLabel(args)
	if len([]rune(label)) > 40 {
		label = string([]rune(label)[:40])
	}
	if !validPlate(plate) {
		b.reply(ctx, chatID, "Bitte so: <code>/add HH-AB123 Mein Auto</code>", nil)
		return
	}
	exists, err := b.store.HasPlate(ctx, chatID, plate)
	if err != nil {
		b.fail(ctx, chatID, "Speichern", err)
		return
	}
	if !exists {
		n, err := b.store.CountPlates(ctx, chatID)
		if err != nil {
			b.fail(ctx, chatID, "Speichern", err)
			return
		}
		if n >= b.cfg.MaxPlates {
			b.reply(ctx, chatID, fmt.Sprintf("Maximal %d Kennzeichen pro Nutzer.", b.cfg.MaxPlates), nil)
			return
		}
	}
	if _, err := b.store.AddPlate(ctx, chatID, plate, label); err != nil {
		b.fail(ctx, chatID, "Speichern", err)
		return
	}
	if exists {
		b.reply(ctx, chatID, "✏️ "+plateTitle(plate, label)+" ist bereits gespeichert.", nil)
		return
	}
	b.reply(ctx, chatID, "✅ "+plateTitle(plate, label)+" hinzugefügt. Ich melde mich, sobald ein Parkvorgang erkannt wird. Aktueller Stand:", nil)
	b.cmdCheck(ctx, chatID, plate)
}

// splitPlateLabel splits "HH-KF 860E Mein Auto" into plate and label. Plates
// may contain spaces, so the plate ends with the first word containing a digit.
func splitPlateLabel(args string) (plate, label string) {
	words := strings.Fields(args)
	for i, w := range words {
		if strings.ContainsAny(w, "0123456789") {
			return fairparken.NormalizePlate(strings.Join(words[:i+1], "")), strings.Join(words[i+1:], " ")
		}
	}
	if len(words) == 0 {
		return "", ""
	}
	return fairparken.NormalizePlate(words[0]), strings.Join(words[1:], " ")
}

func (b *Bot) cmdRemove(ctx context.Context, chatID int64, args string) {
	plate := fairparken.NormalizePlate(args)
	if plate == "" {
		plates, err := b.store.Plates(ctx, chatID)
		if err != nil {
			b.fail(ctx, chatID, "Laden", err)
			return
		}
		if len(plates) == 0 {
			b.reply(ctx, chatID, "Du hast noch keine Kennzeichen gespeichert.", nil)
			return
		}
		kb := &telegram.InlineKeyboard{}
		for _, p := range plates {
			kb.InlineKeyboard = append(kb.InlineKeyboard, []telegram.InlineButton{{Text: "🗑 " + p.Plate, CallbackData: "r:" + p.Plate}})
		}
		b.reply(ctx, chatID, "Welches Kennzeichen entfernen?", kb)
		return
	}
	err := b.store.RemovePlate(ctx, chatID, plate)
	switch {
	case errors.Is(err, store.ErrNotFound):
		b.reply(ctx, chatID, "<b>"+esc(plate)+"</b> ist nicht gespeichert.", nil)
	case err != nil:
		b.fail(ctx, chatID, "Entfernen", err)
	default:
		b.reply(ctx, chatID, "🗑 <b>"+esc(plate)+"</b> entfernt.", nil)
	}
}

func (b *Bot) cmdList(ctx context.Context, chatID int64) {
	b.showPlates(ctx, chatID, 0)
}

// targetPlates resolves the argument of /check and friends: one plate or all of the chat.
func (b *Bot) targetPlates(ctx context.Context, chatID int64, args string) ([]store.Plate, error) {
	if p := fairparken.NormalizePlate(args); p != "" {
		label := ""
		plates, err := b.store.Plates(ctx, chatID)
		if err != nil {
			return nil, err
		}
		for _, x := range plates {
			if x.Plate == p {
				label = x.Label
			}
		}
		return []store.Plate{{ChatID: chatID, Plate: p, Label: label}}, nil
	}
	return b.store.Plates(ctx, chatID)
}

func (b *Bot) cmdCheck(ctx context.Context, chatID int64, args string) {
	plates, err := b.targetPlates(ctx, chatID, args)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	if len(plates) == 0 {
		b.reply(ctx, chatID, "Noch keine Kennzeichen. Hinzufügen mit <code>/add HH-AB123 Name</code> – oder schick mir einfach ein Kennzeichen.", nil)
		return
	}
	for _, p := range plates {
		if !validPlate(p.Plate) {
			b.reply(ctx, chatID, "Ungültiges Kennzeichen.", nil)
			continue
		}
		res, err := b.fp.Search(ctx, p.Plate)
		if err != nil {
			b.fail(ctx, chatID, "Abfrage für "+p.Plate, err)
			continue
		}
		watched, err := b.store.HasPlate(ctx, chatID, p.Plate)
		if err != nil {
			b.fail(ctx, chatID, "Laden", err)
			continue
		}
		if watched {
			// Keep the monitor state in sync, so this session isn't pushed again.
			if err := b.process(ctx, p.Plate, res, chatID); err != nil {
				slog.Error("process", "plate", p.Plate, "err", err)
			}
		}
		if len(res) == 0 {
			var kb *telegram.InlineKeyboard
			if !watched {
				kb = &telegram.InlineKeyboard{InlineKeyboard: [][]telegram.InlineButton{{{Text: "➕ Überwachen", CallbackData: "a:" + p.Plate}}}}
			}
			b.reply(ctx, chatID, "✔️ "+plateTitle(p.Plate, p.Label)+": kein offener Parkvorgang.", kb)
			continue
		}
		for _, r := range res {
			kb := itemKeyboard(p.Plate)
			if !watched {
				kb.InlineKeyboard = append(kb.InlineKeyboard, []telegram.InlineButton{{Text: "➕ Überwachen", CallbackData: "a:" + p.Plate}})
			}
			b.reply(ctx, chatID, "🅿️ <b>Offener Parkvorgang</b>\n\n"+fmtResult(r, p.Label), kb)
		}
	}
}

func (b *Bot) cmdPay(ctx context.Context, chatID int64, args string) {
	plates, err := b.targetPlates(ctx, chatID, args)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	if len(plates) == 0 {
		b.reply(ctx, chatID, "Bitte so: <code>/bezahlen HHAB123</code>", nil)
		return
	}
	kb := &telegram.InlineKeyboard{}
	for _, p := range plates {
		kb.InlineKeyboard = append(kb.InlineKeyboard, []telegram.InlineButton{{Text: "💳 " + p.Plate, URL: fairparken.PayURL(p.Plate)}})
	}
	b.reply(ctx, chatID, "Bezahlseite öffnen (Kennzeichen ist vorausgefüllt, dort auf „Weiter“ tippen):", kb)
}

func (b *Bot) cmdTariffs(ctx context.Context, chatID int64, args string) {
	// A plain number is a quota (parking area) ID.
	if id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64); err == nil && id > 0 {
		b.showTariffs(ctx, chatID, id, "", nil)
		return
	}
	plates, err := b.targetPlates(ctx, chatID, args)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	if len(plates) == 0 {
		b.reply(ctx, chatID, "Bitte so: <code>/tarife HHAB123</code> oder <code>/tarife &lt;Parkplatz-ID&gt;</code>", nil)
		return
	}
	shown := map[int64]bool{}
	for _, p := range plates {
		res, err := b.fp.Search(ctx, p.Plate)
		if err != nil {
			b.fail(ctx, chatID, "Abfrage für "+p.Plate, err)
			continue
		}
		if watched, err := b.store.HasPlate(ctx, chatID, p.Plate); err == nil && watched {
			if err := b.process(ctx, p.Plate, res, chatID); err != nil {
				slog.Error("process", "plate", p.Plate, "err", err)
			}
		}
		if len(res) > 0 {
			it := res[0].AnprResult
			shown[it.QuotaID] = true
			b.showTariffs(ctx, chatID, it.QuotaID, it.QuotaAddress, &it)
			continue
		}
		quota, addr, err := b.store.PlateInfo(ctx, p.Plate)
		if errors.Is(err, store.ErrNotFound) {
			b.reply(ctx, chatID, plateTitle(p.Plate, p.Label)+": kein offener Parkvorgang und noch kein Parkplatz bekannt. Tarife gibt es, sobald einmal ein Parkvorgang erkannt wurde.", nil)
			continue
		}
		if err != nil {
			b.fail(ctx, chatID, "Laden", err)
			continue
		}
		if shown[quota] {
			continue
		}
		shown[quota] = true
		b.showTariffs(ctx, chatID, quota, addr, nil)
	}
}

// showTariffs lists the tariffs of a parking area. With an open session the
// price of that session is calculated for every tariff.
func (b *Bot) showTariffs(ctx context.Context, chatID int64, quotaID int64, address string, item *fairparken.ParkingItem) {
	tariffs, err := b.fp.Tariffs(ctx, quotaID)
	if err != nil {
		b.fail(ctx, chatID, "Tarifabfrage", err)
		return
	}
	var sb strings.Builder
	sb.WriteString("🏷 <b>Tarife</b>")
	if address != "" {
		sb.WriteString(" – " + esc(address))
	}
	fmt.Fprintf(&sb, "\n<i>Parkplatz-ID %d</i>\n", quotaID)
	if len(tariffs) == 0 {
		sb.WriteString("\nKeine Tarife hinterlegt.")
		b.reply(ctx, chatID, sb.String(), nil)
		return
	}
	for _, t := range tariffs {
		fmt.Fprintf(&sb, "\n• %s", esc(t.Name()))
		if item != nil && item.TariffID != nil && *item.TariffID == t.ID {
			sb.WriteString(" (aktuell)")
		}
		if item == nil {
			continue
		}
		q, err := b.fp.Quote(ctx, *item, t.ID)
		if err != nil {
			slog.Warn("quote", "quota", quotaID, "tariff", t.ID, "err", err)
			sb.WriteString(": Preis nicht verfügbar")
			continue
		}
		if q.Price != nil {
			sb.WriteString(": <b>" + fmtEuro(*q.Price) + "</b>")
		}
	}
	if item != nil {
		// The web client uses the same call when a tariff is picked. Restore the
		// original selection in case the backend remembers the last one.
		if item.TariffID != nil && len(tariffs) > 1 {
			if _, err := b.fp.Quote(ctx, *item, *item.TariffID); err != nil {
				slog.Warn("restore tariff", "err", err)
			}
		}
		sb.WriteString("\n\nPreise für den aktuellen Parkvorgang von <b>" + esc(item.Plate) + "</b>.")
	} else {
		sb.WriteString("\n\nPreise berechnet fair parken nur für einen konkreten Parkvorgang – sie erscheinen hier, solange einer offen ist.")
	}
	var kb *telegram.InlineKeyboard
	if item != nil {
		kb = itemKeyboard(item.Plate)
	}
	b.reply(ctx, chatID, sb.String(), kb)
}
