package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/niondir/parkingapp/internal/fairparken"
	"github.com/niondir/parkingapp/internal/store"
	"github.com/niondir/parkingapp/internal/telegram"
)

// menuButton is the persistent button below the chat that opens the main menu.
const menuButton = "🏠 Menü"

var replyKeyboard = &telegram.ReplyKeyboard{
	Keyboard:   [][]telegram.KeyboardButton{{{Text: menuButton}}},
	Resize:     true,
	Persistent: true,
}

func btn(text, data string) telegram.InlineButton {
	return telegram.InlineButton{Text: text, CallbackData: data}
}

func kb(rows ...[]telegram.InlineButton) *telegram.InlineKeyboard {
	return &telegram.InlineKeyboard{InlineKeyboard: rows}
}

func row(b ...telegram.InlineButton) []telegram.InlineButton { return b }

var cancelRow = row(btn("✖️ Abbrechen", "x"))

// show replaces the menu message msgID, or sends a new message if msgID is 0.
func (b *Bot) show(ctx context.Context, chatID, msgID int64, html string, k *telegram.InlineKeyboard) {
	if msgID == 0 {
		b.reply(ctx, chatID, html, k)
		return
	}
	err := b.tg.Edit(ctx, chatID, msgID, html, k)
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) && strings.Contains(apiErr.Description, "message is not modified") {
		return
	}
	if err != nil {
		slog.Warn("edit", "chat", chatID, "err", err)
		b.reply(ctx, chatID, html, k)
	}
}

// --- pending input ---

type inputKind int

const (
	inAdd inputKind = iota + 1
	inLabel
	inRename
	inVoucher
	inPrice
	inDuration
	inQuiet
	inBroadcast
	inBroadcastConfirm
	inGrace
	inFeedback
	inFeedbackReply
)

const inputTimeout = 15 * time.Minute

// pending is a question the bot asked and waits to be answered with the next message.
type pending struct {
	kind     inputKind
	plate    string
	data     string
	prompt   string
	promptID int64
	expires  time.Time
}

func (b *Bot) getPending(chatID int64) *pending {
	b.pendMu.Lock()
	defer b.pendMu.Unlock()
	p := b.pend[chatID]
	if p != nil && time.Now().After(p.expires) {
		delete(b.pend, chatID)
		return nil
	}
	return p
}

// endPending forgets the open question and removes its cancel button.
func (b *Bot) endPending(ctx context.Context, chatID int64, note string) {
	b.pendMu.Lock()
	p := b.pend[chatID]
	delete(b.pend, chatID)
	b.pendMu.Unlock()
	if p == nil || p.promptID == 0 {
		return
	}
	text := p.prompt
	if note != "" {
		text += "\n\n" + note
	}
	if err := b.tg.Edit(ctx, chatID, p.promptID, text, nil); err != nil {
		slog.Debug("edit prompt", "err", err)
	}
}

// ask sends a question with a cancel button and waits for the next text message.
func (b *Bot) ask(ctx context.Context, chatID int64, kind inputKind, plate, data, prompt string, extra ...[]telegram.InlineButton) {
	b.endPending(ctx, chatID, "")
	rows := append(extra, cancelRow)
	id, err := b.tg.SendMsg(ctx, chatID, prompt, kb(rows...))
	if err != nil {
		slog.Warn("send prompt", "chat", chatID, "err", err)
		return
	}
	b.pendMu.Lock()
	b.pend[chatID] = &pending{kind: kind, plate: plate, data: data, prompt: prompt, promptID: id, expires: time.Now().Add(inputTimeout)}
	b.pendMu.Unlock()
}

// retry answers invalid input and keeps waiting.
func (b *Bot) retry(ctx context.Context, chatID int64, msg string) {
	b.reply(ctx, chatID, "⚠️ "+msg+"\nBitte nochmal – oder abbrechen.", kb(cancelRow))
}

func (b *Bot) handleInput(ctx context.Context, chatID int64, p *pending, text string) {
	switch p.kind {
	case inAdd:
		plate := fairparken.NormalizePlate(text)
		if !validPlate(plate) {
			b.retry(ctx, chatID, "Das sieht nicht wie ein Kennzeichen aus (z. B. <code>HH-AB 123</code>).")
			return
		}
		if ok, err := b.store.HasPlate(ctx, chatID, plate); err == nil && ok {
			b.endPending(ctx, chatID, "")
			b.reply(ctx, chatID, "<b>"+esc(plate)+"</b> ist bereits gespeichert.", kb(row(btn("🚗 Öffnen", "p:"+plate))))
			return
		}
		b.endPending(ctx, chatID, "→ <b>"+esc(plate)+"</b>")
		b.ask(ctx, chatID, inLabel, plate, "",
			"Wie soll <b>"+esc(plate)+"</b> heißen? Schick mir einen Namen, z. B. <i>Golf</i> oder <i>Firmenwagen</i>.",
			row(btn("Ohne Namen", "lbl:")))
	case inLabel:
		b.endPending(ctx, chatID, "→ "+esc(text))
		b.finishAdd(ctx, chatID, p.plate, text)
	case inRename:
		label := cleanLabel(text)
		if err := b.store.SetLabel(ctx, chatID, p.plate, label); err != nil {
			b.endPending(ctx, chatID, "")
			b.fail(ctx, chatID, "Speichern", err)
			return
		}
		b.endPending(ctx, chatID, "→ "+esc(label))
		b.showPlate(ctx, chatID, 0, p.plate)
	case inVoucher:
		code := strings.TrimSpace(text)
		if code == "" || len(code) > 64 || strings.ContainsAny(code, " \n") {
			b.retry(ctx, chatID, "Bitte nur den Code ohne Leerzeichen schicken.")
			return
		}
		b.endPending(ctx, chatID, "→ <code>"+esc(code)+"</code>")
		b.cmdVoucher(ctx, chatID, code+" "+p.plate)
	case inPrice:
		v, ok := parsePrice(text)
		if !ok || v == 0 {
			b.retry(ctx, chatID, "Bitte einen Betrag in Euro schicken, z. B. <code>12,50</code>.")
			return
		}
		b.endPending(ctx, chatID, "→ "+fmtEuro(v))
		b.updatePlate(ctx, chatID, 0, p.plate, func(s *store.PlateSettings) { s.PriceLimit = v })
	case inDuration:
		d, ok := parseDurationValue(text)
		if !ok || d == 0 {
			b.retry(ctx, chatID, "Bitte eine Dauer in Stunden schicken, z. B. <code>2,5</code> oder <code>90m</code>.")
			return
		}
		b.endPending(ctx, chatID, "→ "+fmtDuration(d))
		b.updatePlate(ctx, chatID, 0, p.plate, func(s *store.PlateSettings) { s.DurationLimit = d })
	case inQuiet:
		cs, ok := parseQuiet(text)
		if !ok || cs.QuietFrom == cs.QuietTo {
			b.retry(ctx, chatID, "Bitte so: <code>22-7</code> (von–bis in vollen Stunden).")
			return
		}
		b.endPending(ctx, chatID, "→ "+fmtQuiet(cs))
		b.setQuiet(ctx, chatID, 0, p.plate, cs)
	case inBroadcast:
		if !b.isAdmin(chatID) {
			b.endPending(ctx, chatID, "")
			return
		}
		b.endPending(ctx, chatID, "")
		n := len(b.allChats(ctx))
		b.ask(ctx, chatID, inBroadcastConfirm, "", text,
			fmt.Sprintf("Vorschau:\n\n📢 %s\n\nAn %d Chats senden?", esc(text), n),
			row(btn("✅ Senden", "abx")))
	case inGrace:
		if !b.isAdmin(chatID) {
			b.endPending(ctx, chatID, "")
			return
		}
		m, ok := parseMinutes(text)
		if !ok {
			b.retry(ctx, chatID, "Bitte Minuten von 0 bis 1440 schicken, z. B. <code>15</code>.")
			return
		}
		b.endPending(ctx, chatID, fmt.Sprintf("→ %d min", m))
		b.setGrace(ctx, chatID, 0, chatID, m)
	case inFeedback:
		if strings.TrimSpace(text) == "" {
			b.retry(ctx, chatID, "Bitte eine Nachricht schicken.")
			return
		}
		b.endPending(ctx, chatID, "")
		b.saveFeedback(ctx, chatID, text)
	case inFeedbackReply:
		if !b.isAdmin(chatID) {
			b.endPending(ctx, chatID, "")
			return
		}
		id, err := strconv.ParseInt(p.data, 10, 64)
		b.endPending(ctx, chatID, "")
		if err == nil {
			b.saveFeedbackReply(ctx, chatID, id, text)
		}
	case inBroadcastConfirm:
		b.retry(ctx, chatID, "Bitte mit „Senden“ bestätigen.")
	}
}

func cleanLabel(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 40 {
		s = string(r[:40])
	}
	return s
}

func (b *Bot) finishAdd(ctx context.Context, chatID int64, plate, label string) {
	b.cmdAdd(ctx, chatID, plate+" "+cleanLabel(label))
}

// --- menus ---

func (b *Bot) sendWelcome(ctx context.Context, chatID int64) {
	if _, err := b.tg.SendMsg(ctx, chatID,
		"👋 Hallo! Ich melde mich, sobald für deine Kennzeichen ein Parkvorgang auf einem fair-parken-Parkplatz erkannt wird.\n\nAlles geht über die Buttons – unten findest du jederzeit „"+menuButton+"“.",
		replyKeyboard); err != nil {
		slog.Warn("send welcome", "chat", chatID, "err", err)
	}
	b.showMain(ctx, chatID, 0)
}

func (b *Bot) showMain(ctx context.Context, chatID, msgID int64) {
	plates, err := b.store.Plates(ctx, chatID)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	text := "🅿️ <b>Hauptmenü</b>\n\n"
	paused := 0
	for _, p := range plates {
		if p.Paused {
			paused++
		}
	}
	switch {
	case len(plates) == 0:
		text += "Du überwachst noch kein Kennzeichen. Leg los mit „Hinzufügen“."
	case paused > 0:
		text += fmt.Sprintf("Du überwachst %d Kennzeichen, davon %d pausiert.", len(plates), paused)
	default:
		text += fmt.Sprintf("Du überwachst %d Kennzeichen.", len(plates))
	}
	k := kb(
		row(btn("🚗 Meine Kennzeichen", "ml"), btn("➕ Hinzufügen", "add")),
		row(btn("🔄 Jetzt prüfen", "chk"), btn("🔔 Alarme", "al")),
		row(btn("📒 Verlauf", "hist"), btn("🧾 Kassenzettel", "rc")),
		row(btn("💬 Feedback", "fb"), btn("❓ Hilfe", "help")),
	)
	if b.isAdmin(chatID) {
		inbox := "📥 Feedback"
		if n, err := b.store.CountOpenFeedback(ctx); err == nil && n > 0 {
			inbox = fmt.Sprintf("📥 Feedback (%d)", n)
		}
		k.InlineKeyboard = append(k.InlineKeyboard,
			row(btn("👥 Nutzer", "au"), btn("📊 Statistik", "as"), btn("📈 Nutzung", "us:30")),
			row(btn(inbox, "fba"), btn("📢 Rundnachricht", "ab"), btn("⏳ Karenzzeit", "gr")))
	}
	b.show(ctx, chatID, msgID, text, k)
}

func plateButtonText(p store.Plate) string {
	t := "🚗 " + p.Plate
	if p.Paused {
		t = "⏸ " + p.Plate
	}
	if p.Label != "" {
		t += " · " + p.Label
	}
	return t
}

func (b *Bot) showPlates(ctx context.Context, chatID, msgID int64) {
	plates, err := b.store.Plates(ctx, chatID)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	k := kb()
	text := "🚗 <b>Deine Kennzeichen</b> – tippe eins an."
	if len(plates) == 0 {
		text = "🚗 Noch keine Kennzeichen gespeichert."
	}
	for _, p := range plates {
		k.InlineKeyboard = append(k.InlineKeyboard, row(btn(plateButtonText(p), "p:"+p.Plate)))
	}
	k.InlineKeyboard = append(k.InlineKeyboard, row(btn("➕ Hinzufügen", "add"), btn("⬅️ Hauptmenü", "m")))
	b.show(ctx, chatID, msgID, text, k)
}

func fmtPriceLimit(p store.Plate) string {
	if p.PriceLimit > 0 {
		return "ab " + fmtEuro(p.PriceLimit)
	}
	return "aus"
}

func fmtDurationLimit(p store.Plate) string {
	if p.DurationLimit > 0 {
		return "ab " + fmtDuration(p.DurationLimit)
	}
	return "aus"
}

func plateSummary(p store.Plate) string {
	status := "▶️ aktiv"
	if p.Paused {
		status = "⏸ pausiert"
	}
	return status + "\n💸 Preisalarm: " + fmtPriceLimit(p) + "\n⏱ Dauer-Alarm: " + fmtDurationLimit(p)
}

// showAlarms shows the alarms of a plate. Without plate it asks which one.
func (b *Bot) showAlarms(ctx context.Context, chatID, msgID int64, plate string) {
	if plate == "" {
		plates, err := b.store.Plates(ctx, chatID)
		if err != nil {
			b.fail(ctx, chatID, "Laden", err)
			return
		}
		switch len(plates) {
		case 0:
			b.show(ctx, chatID, msgID, "🔔 Noch keine Kennzeichen – erst eins hinzufügen.",
				kb(row(btn("➕ Hinzufügen", "add"), btn("⬅️ Hauptmenü", "m"))))
			return
		case 1:
			plate = plates[0].Plate
		default:
			k := kb()
			var sb strings.Builder
			sb.WriteString("🔔 <b>Alarme</b> – für welches Kennzeichen?\n")
			for _, p := range plates {
				fmt.Fprintf(&sb, "\n%s: Preis %s, Dauer %s", plateTitle(p.Plate, p.Label), fmtPriceLimit(p), fmtDurationLimit(p))
				k.InlineKeyboard = append(k.InlineKeyboard, row(btn(plateButtonText(p), "al:"+p.Plate)))
			}
			k.InlineKeyboard = append(k.InlineKeyboard, row(btn("⬅️ Hauptmenü", "m")))
			b.show(ctx, chatID, msgID, sb.String(), k)
			return
		}
	}
	p, err := b.store.GetPlate(ctx, chatID, plate)
	if err != nil {
		b.showPlate(ctx, chatID, msgID, plate)
		return
	}
	text := fmt.Sprintf("🔔 <b>Alarme</b> für %s\n\n💸 Preisalarm: %s\n⏱ Dauer-Alarm: %s\n📈 Jede Preiserhöhung melden: %s\n🔔 Erinnerung vor Ablauf der Zahlungsfrist: %s",
		plateTitle(p.Plate, p.Label), fmtPriceLimit(p), fmtDurationLimit(p), onOff(p.PriceChange), onOff(p.Remind))
	b.show(ctx, chatID, msgID, text, kb(
		row(btn("💸 Preisalarm", "pl:"+p.Plate), btn("⏱ Dauer-Alarm", "dl:"+p.Plate)),
		row(btn("📈 Preiserhöhungen "+onOff(!p.PriceChange)+"schalten", "alc:"+p.Plate)),
		row(btn("🔔 Frist-Erinnerung "+onOff(!p.Remind)+"schalten", "alr:"+p.Plate)),
		row(btn("⬅️ Zum Kennzeichen", "p:"+p.Plate)),
	))
}

func (b *Bot) showPlate(ctx context.Context, chatID, msgID int64, plate string) {
	p, err := b.store.GetPlate(ctx, chatID, plate)
	if errors.Is(err, store.ErrNotFound) {
		b.show(ctx, chatID, msgID, "<b>"+esc(plate)+"</b> ist nicht (mehr) gespeichert.", kb(row(btn("⬅️ Kennzeichen", "ml"))))
		return
	}
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	text := "🚗 " + plateTitle(p.Plate, p.Label) + "\n" + plateSummary(p)
	b.show(ctx, chatID, msgID, text, kb(
		row(btn("🔄 Prüfen", "c:"+p.Plate), telegram.InlineButton{Text: "💳 Bezahlen", URL: fairparken.PayURL(p.Plate)}),
		row(btn("🔔 Alarme", "al:"+p.Plate)),
		row(btn("🏷 Tarife", "t:"+p.Plate), btn("🎟 Rabattcode", "v:"+p.Plate)),
		row(btn("⚙️ Einstellungen", "s:"+p.Plate), btn("📒 Verlauf", "hist:"+p.Plate)),
		row(btn("✏️ Umbenennen", "rn:"+p.Plate), btn("🗑 Entfernen", "r:"+p.Plate)),
		row(btn("⬅️ Kennzeichen", "ml")),
	))
}

func (b *Bot) showSettings(ctx context.Context, chatID, msgID int64, plate, note string) {
	p, err := b.store.GetPlate(ctx, chatID, plate)
	if err != nil {
		b.showPlate(ctx, chatID, msgID, plate)
		return
	}
	cs, err := b.store.GetChatSettings(ctx, chatID)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	text := settingsText(p, cs)
	if note != "" {
		text = note + "\n\n" + text
	}
	b.show(ctx, chatID, msgID, text, settingsKeyboard(p))
}

func (b *Bot) updatePlate(ctx context.Context, chatID, msgID int64, plate string, change func(*store.PlateSettings)) {
	p, err := b.store.GetPlate(ctx, chatID, plate)
	if err != nil {
		b.showPlate(ctx, chatID, msgID, plate)
		return
	}
	change(&p.PlateSettings)
	if err := b.store.SetPlateSettings(ctx, chatID, plate, p.PlateSettings); err != nil {
		b.fail(ctx, chatID, "Speichern", err)
		return
	}
	b.showAlarms(ctx, chatID, msgID, plate)
}

func (b *Bot) setQuiet(ctx context.Context, chatID, msgID int64, plate string, cs store.ChatSettings) {
	if err := b.store.SetChatSettings(ctx, chatID, cs); err != nil {
		b.fail(ctx, chatID, "Speichern", err)
		return
	}
	if plate == "" {
		b.show(ctx, chatID, msgID, "🌙 Ruhezeit: "+fmtQuiet(cs), nil)
		return
	}
	b.showSettings(ctx, chatID, msgID, plate, "✅ Gespeichert.")
}

var (
	pricePresets    = []float64{5, 10, 15, 20, 30, 50}
	durationPresets = []int{1, 2, 3, 4, 6, 8} // hours
	quietPresets    = [][2]int{{22, 7}, {23, 6}, {21, 8}, {0, 6}}
)

func (b *Bot) showPriceMenu(ctx context.Context, chatID, msgID int64, plate string) {
	p, err := b.store.GetPlate(ctx, chatID, plate)
	if err != nil {
		b.showPlate(ctx, chatID, msgID, plate)
		return
	}
	cur := "aus"
	if p.PriceLimit > 0 {
		cur = fmtEuro(p.PriceLimit)
	}
	k := kb()
	var r []telegram.InlineButton
	for i, v := range pricePresets {
		r = append(r, btn(fmtEuro(v), fmt.Sprintf("plv:%s:%g", plate, v)))
		if i%3 == 2 {
			k.InlineKeyboard = append(k.InlineKeyboard, r)
			r = nil
		}
	}
	k.InlineKeyboard = append(k.InlineKeyboard,
		row(btn("🚫 Aus", "plv:"+plate+":0"), btn("✏️ Anderer Betrag", "pli:"+plate)),
		row(btn("⬅️ Zurück", "al:"+plate)))
	b.show(ctx, chatID, msgID, fmt.Sprintf("💸 <b>Preisalarm</b> für %s\nAktuell: %s\n\nIch melde mich, sobald die Parkgebühr diesen Betrag erreicht.",
		plateTitle(p.Plate, p.Label), cur), k)
}

func (b *Bot) showDurationMenu(ctx context.Context, chatID, msgID int64, plate string) {
	p, err := b.store.GetPlate(ctx, chatID, plate)
	if err != nil {
		b.showPlate(ctx, chatID, msgID, plate)
		return
	}
	cur := "aus"
	if p.DurationLimit > 0 {
		cur = fmtDuration(p.DurationLimit)
	}
	k := kb()
	var r []telegram.InlineButton
	for i, h := range durationPresets {
		r = append(r, btn(fmt.Sprintf("%d h", h), fmt.Sprintf("dlv:%s:%d", plate, h*60)))
		if i%3 == 2 {
			k.InlineKeyboard = append(k.InlineKeyboard, r)
			r = nil
		}
	}
	k.InlineKeyboard = append(k.InlineKeyboard,
		row(btn("🚫 Aus", "dlv:"+plate+":0"), btn("✏️ Andere Dauer", "dli:"+plate)),
		row(btn("⬅️ Zurück", "al:"+plate)))
	b.show(ctx, chatID, msgID, fmt.Sprintf("⏱ <b>Parkdauer-Alarm</b> für %s\nAktuell: %s\n\nIch melde mich, wenn der Wagen länger als diese Zeit parkt.",
		plateTitle(p.Plate, p.Label), cur), k)
}

func (b *Bot) showQuietMenu(ctx context.Context, chatID, msgID int64, plate string) {
	cs, err := b.store.GetChatSettings(ctx, chatID)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	k := kb()
	var r []telegram.InlineButton
	for _, q := range quietPresets {
		r = append(r, btn(fmt.Sprintf("%d–%d Uhr", q[0], q[1]), fmt.Sprintf("qv:%s:%d-%d", plate, q[0], q[1])))
	}
	k.InlineKeyboard = append(k.InlineKeyboard, r[:2], r[2:],
		row(btn("🚫 Aus", "qv:"+plate+":aus"), btn("✏️ Andere Zeit", "qi:"+plate)),
		row(btn("⬅️ Zurück", "s:"+plate)))
	b.show(ctx, chatID, msgID, "🌙 <b>Ruhezeit</b> (gilt für alle Kennzeichen)\nAktuell: "+fmtQuiet(cs)+
		"\n\nIn der Ruhezeit kommen Meldungen ohne Ton.", k)
}

// splitLast splits "PLATE:value" at the last colon.
func splitLast(s string) (string, string) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

// handleMenuCallback handles the menu buttons. It returns false for unknown data.
func (b *Bot) handleMenuCallback(ctx context.Context, chatID, msgID int64, kind, arg string) bool {
	switch kind {
	case "m":
		b.showMain(ctx, chatID, msgID)
	case "ml":
		b.showPlates(ctx, chatID, msgID)
	case "p":
		b.showPlate(ctx, chatID, msgID, arg)
	case "s":
		if arg == "" {
			b.cmdSettings(ctx, chatID, "")
		} else {
			b.showSettings(ctx, chatID, msgID, arg, "")
		}
	case "add":
		n, err := b.store.CountPlates(ctx, chatID)
		if err == nil && n >= b.cfg.MaxPlates {
			b.reply(ctx, chatID, fmt.Sprintf("Maximal %d Kennzeichen pro Nutzer. Entferne erst eins.", b.cfg.MaxPlates), nil)
			return true
		}
		b.ask(ctx, chatID, inAdd, "", "", "➕ Schick mir das Kennzeichen, z. B. <code>HH-AB 123</code>.")
	case "lbl":
		if p := b.getPending(chatID); p != nil && p.kind == inLabel {
			b.endPending(ctx, chatID, "→ ohne Namen")
			b.finishAdd(ctx, chatID, p.plate, "")
		}
	case "rn":
		b.ask(ctx, chatID, inRename, arg, "", "✏️ Neuer Name für <b>"+esc(arg)+"</b>?", row(btn("Namen entfernen", "rnx:"+arg)))
	case "rnx":
		b.endPending(ctx, chatID, "→ ohne Namen")
		if err := b.store.SetLabel(ctx, chatID, arg, ""); err != nil {
			b.fail(ctx, chatID, "Speichern", err)
			return true
		}
		b.showPlate(ctx, chatID, 0, arg)
	case "r":
		b.show(ctx, chatID, msgID, "🗑 <b>"+esc(arg)+"</b> wirklich entfernen? Du bekommst dann keine Meldungen mehr dafür.",
			kb(row(btn("🗑 Ja, entfernen", "rx:"+arg), btn("✖️ Abbrechen", "p:"+arg))))
	case "rx":
		err := b.store.RemovePlate(ctx, chatID, arg)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			b.fail(ctx, chatID, "Entfernen", err)
			return true
		}
		b.showPlates(ctx, chatID, msgID)
	case "v":
		b.ask(ctx, chatID, inVoucher, arg, "", "🎟 Schick mir den Rabattcode für <b>"+esc(arg)+"</b>.")
	case "al":
		b.showAlarms(ctx, chatID, msgID, arg)
	case "alc", "alr":
		b.updatePlate(ctx, chatID, msgID, arg, func(s *store.PlateSettings) {
			if kind == "alc" {
				s.PriceChange = !s.PriceChange
			} else {
				s.Remind = !s.Remind
			}
		})
	case "pl":
		b.showPriceMenu(ctx, chatID, msgID, arg)
	case "plv":
		plate, v := splitLast(arg)
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return true
		}
		b.updatePlate(ctx, chatID, msgID, plate, func(s *store.PlateSettings) { s.PriceLimit = f })
	case "pli":
		b.ask(ctx, chatID, inPrice, arg, "", "💸 Ab welchem Betrag soll ich mich melden? Z. B. <code>12,50</code>")
	case "dl":
		b.showDurationMenu(ctx, chatID, msgID, arg)
	case "dlv":
		plate, v := splitLast(arg)
		m, err := strconv.Atoi(v)
		if err != nil || m < 0 {
			return true
		}
		b.updatePlate(ctx, chatID, msgID, plate, func(s *store.PlateSettings) { s.DurationLimit = time.Duration(m) * time.Minute })
	case "dli":
		b.ask(ctx, chatID, inDuration, arg, "", "⏱ Nach wie vielen Stunden soll ich mich melden? Z. B. <code>2,5</code> oder <code>90m</code>")
	case "q":
		b.showQuietMenu(ctx, chatID, msgID, arg)
	case "qv":
		plate, v := splitLast(arg)
		cs, ok := parseQuiet(v)
		if !ok {
			return true
		}
		b.setQuiet(ctx, chatID, msgID, plate, cs)
	case "qi":
		b.ask(ctx, chatID, inQuiet, arg, "", "🌙 Von wann bis wann? Z. B. <code>22-7</code>")
	case "chk":
		b.cmdCheck(ctx, chatID, "")
	case "hist":
		b.cmdHistory(ctx, chatID, arg)
	case "rc":
		b.cmdReceipt(ctx, chatID)
	case "help":
		b.cmdHelp(ctx, chatID)
	case "fb":
		b.showFeedbackMenu(ctx, chatID, msgID)
	case "fbn":
		b.askFeedback(ctx, chatID)
	case "fbm":
		b.showOwnFeedback(ctx, chatID, msgID)
	case "x":
		if b.getPending(chatID) != nil {
			b.endPending(ctx, chatID, "✖️ Abgebrochen.")
		} else {
			b.show(ctx, chatID, msgID, "✖️ Abgebrochen.", nil)
		}
	default:
		return false
	}
	return true
}
