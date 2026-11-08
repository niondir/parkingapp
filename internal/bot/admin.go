package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/niondir/parkingapp/internal/store"
	"github.com/niondir/parkingapp/internal/telegram"
)

var adminCommands = []telegram.BotCommand{
	{Command: "users", Description: "Admin: Nutzer und Anfragen"},
	{Command: "allow", Description: "Admin: Nutzer freischalten (/allow ID)"},
	{Command: "block", Description: "Admin: Nutzer sperren (/block ID)"},
	{Command: "stats", Description: "Admin: Statistik und Abfragefehler"},
	{Command: "rundnachricht", Description: "Admin: Nachricht an alle Nutzer"},
}

func (b *Bot) isAdmin(userID int64) bool { return b.cfg.Admins[userID] }

func (b *Bot) open() bool { return len(b.cfg.Admins) == 0 && len(b.cfg.AllowedUsers) == 0 }

func (b *Bot) allowed(ctx context.Context, userID int64) (bool, error) {
	if b.open() || b.isAdmin(userID) || b.cfg.AllowedUsers[userID] {
		return true, nil
	}
	st, err := b.store.UserStatus(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return st == store.UserAllowed, err
}

// checkAccess answers users without access and turns their first message
// into an access request for the admins.
func (b *Bot) checkAccess(ctx context.Context, chatID int64, from telegram.User) bool {
	ok, err := b.allowed(ctx, from.ID)
	if err != nil {
		b.fail(ctx, chatID, "Zugriffsprüfung", err)
		return false
	}
	if ok {
		return true
	}
	slog.Info("access denied", "user", from.ID, "username", from.Username, "name", from.FirstName)
	if len(b.cfg.Admins) == 0 {
		b.reply(ctx, chatID, fmt.Sprintf("⛔ Kein Zugriff. Deine Telegram-ID ist <code>%d</code> – bitte beim Betreiber freischalten lassen.", from.ID), nil)
		return false
	}
	st, err := b.store.UserStatus(ctx, from.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if _, err := b.store.RequestAccess(ctx, from.ID, from.Username, from.FirstName); err != nil {
			b.fail(ctx, chatID, "Zugangsanfrage", err)
			return false
		}
		b.notifyAdmins(ctx, "🙋 <b>Neue Zugangsanfrage</b>\n\n"+fmtUser(store.User{ID: from.ID, Username: from.Username, Name: from.FirstName}),
			userKeyboard(from.ID, store.UserPending))
		b.reply(ctx, chatID, "📨 Zugang angefragt. Du bekommst eine Nachricht, sobald ein Admin dich freischaltet.", nil)
	case err != nil:
		b.fail(ctx, chatID, "Zugriffsprüfung", err)
	case st == store.UserBlocked:
		b.reply(ctx, chatID, "⛔ Kein Zugriff.", nil)
	default:
		b.reply(ctx, chatID, "⏳ Deine Anfrage wartet noch auf Freischaltung.", nil)
	}
	return false
}

func (b *Bot) notifyAdmins(ctx context.Context, html string, kb *telegram.InlineKeyboard) {
	for id := range b.cfg.Admins {
		b.reply(ctx, id, html, kb)
	}
}

func fmtUser(u store.User) string {
	s := fmt.Sprintf("<code>%d</code>", u.ID)
	if u.Name != "" {
		s += " " + esc(u.Name)
	}
	if u.Username != "" {
		s += " @" + esc(u.Username)
	}
	return s
}

func userKeyboard(userID int64, status string) *telegram.InlineKeyboard {
	id := strconv.FormatInt(userID, 10)
	var row []telegram.InlineButton
	if status != store.UserAllowed {
		row = append(row, telegram.InlineButton{Text: "✅ Freischalten", CallbackData: "ua:" + id})
	}
	if status != store.UserBlocked {
		label := "🚫 Sperren"
		if status == store.UserPending {
			label = "🚫 Ablehnen"
		}
		row = append(row, telegram.InlineButton{Text: label, CallbackData: "ub:" + id})
	}
	return &telegram.InlineKeyboard{InlineKeyboard: [][]telegram.InlineButton{row}}
}

// handleAdminText handles admin commands. It returns false for other commands.
func (b *Bot) handleAdminText(ctx context.Context, chatID, adminID int64, text string) bool {
	cmd, args, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(strings.ToLower(cmd), "@")
	args = strings.TrimSpace(args)
	switch cmd {
	case "/users":
		b.cmdUsers(ctx, chatID)
	case "/stats":
		b.cmdStats(ctx, chatID)
	case "/rundnachricht", "/broadcast":
		b.cmdBroadcast(ctx, chatID, adminID, args)
	case "/allow", "/block":
		id, err := strconv.ParseInt(args, 10, 64)
		if err != nil {
			b.reply(ctx, chatID, "Bitte so: <code>"+cmd+" 123456789</code> (IDs zeigt /users)", nil)
			return true
		}
		status := store.UserAllowed
		if cmd == "/block" {
			status = store.UserBlocked
		}
		b.setUserStatus(ctx, chatID, adminID, id, status)
	default:
		return false
	}
	return true
}

func (b *Bot) handleAdminCallback(ctx context.Context, chatID, adminID int64, data string) bool {
	kind, arg, _ := strings.Cut(data, ":")
	switch kind {
	case "au":
		b.cmdUsers(ctx, chatID)
		return true
	case "as":
		b.cmdStats(ctx, chatID)
		return true
	case "ab":
		b.ask(ctx, chatID, inBroadcast, "", "", "📢 Welche Nachricht soll an alle Nutzer gehen?")
		return true
	case "abx":
		if p := b.getPending(chatID); p != nil && p.kind == inBroadcastConfirm {
			b.endPending(ctx, chatID, "")
			b.cmdBroadcast(ctx, chatID, adminID, p.data)
		}
		return true
	case "ua", "ub":
	default:
		return false
	}
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return true
	}
	status := store.UserAllowed
	if kind == "ub" {
		status = store.UserBlocked
	}
	b.setUserStatus(ctx, chatID, adminID, id, status)
	return true
}

func (b *Bot) setUserStatus(ctx context.Context, chatID, adminID, userID int64, status string) {
	if b.isAdmin(userID) {
		b.reply(ctx, chatID, "Admins werden über <code>ADMIN_USER_IDS</code> verwaltet.", nil)
		return
	}
	old, err := b.store.GetUser(ctx, userID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	if err == nil && old.Status == status {
		b.reply(ctx, chatID, fmtUser(old)+" ist bereits "+statusText(status)+".", nil)
		return
	}
	if err := b.store.SetUserStatus(ctx, userID, status); err != nil {
		b.fail(ctx, chatID, "Speichern", err)
		return
	}
	if old.ID == 0 {
		old.ID = userID
	}
	slog.Info("user status changed", "user", userID, "status", status, "admin", adminID)
	switch status {
	case store.UserAllowed:
		b.reply(ctx, userID, "✅ Du wurdest freigeschaltet.", nil)
		b.sendWelcome(ctx, userID)
	case store.UserBlocked:
		// Private chats have the user's ID as chat ID, so this stops all pushes.
		if err := b.store.RemovePlates(ctx, userID); err != nil {
			slog.Error("remove plates", "user", userID, "err", err)
		}
		if old.Status == store.UserAllowed {
			b.reply(ctx, userID, "⛔ Dein Zugang wurde entzogen.", nil)
		}
	}
	// Inform all admins, so a request handled by one isn't handled again by another.
	b.notifyAdmins(ctx, fmtUser(old)+" ist jetzt "+statusText(status)+".", nil)
	if b.cfg.AllowedUsers[userID] && status == store.UserBlocked {
		b.reply(ctx, chatID, "⚠️ Die ID steht auch in <code>ALLOWED_USER_IDS</code> und hat darüber weiter Zugriff.", nil)
	}
}

func statusText(status string) string {
	switch status {
	case store.UserAllowed:
		return "freigeschaltet"
	case store.UserBlocked:
		return "gesperrt"
	default:
		return "angefragt"
	}
}

func (b *Bot) cmdUsers(ctx context.Context, chatID int64) {
	users, err := b.store.Users(ctx)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	if len(users) == 0 {
		b.reply(ctx, chatID, "Noch keine Nutzer oder Anfragen.", nil)
		return
	}
	icons := map[string]string{store.UserPending: "⏳", store.UserAllowed: "✅", store.UserBlocked: "🚫"}
	var sb strings.Builder
	sb.WriteString("👥 <b>Nutzer</b>\n")
	kb := &telegram.InlineKeyboard{}
	for _, u := range users {
		n, _ := b.store.CountPlates(ctx, u.ID)
		fmt.Fprintf(&sb, "\n%s %s – %d Kennzeichen", icons[u.Status], fmtUser(u), n)
		label := u.Name
		if label == "" {
			label = strconv.FormatInt(u.ID, 10)
		}
		for _, btn := range userKeyboard(u.ID, u.Status).InlineKeyboard[0] {
			btn.Text += " " + label
			kb.InlineKeyboard = append(kb.InlineKeyboard, []telegram.InlineButton{btn})
		}
	}
	b.reply(ctx, chatID, sb.String(), kb)
}

func (b *Bot) cmdStats(ctx context.Context, chatID int64) {
	st, err := b.store.Stats(ctx)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	b.statsMu.Lock()
	ps := b.stats
	b.statsMu.Unlock()
	var sb strings.Builder
	sb.WriteString("📊 <b>Statistik</b>\n\n")
	fmt.Fprintf(&sb, "Nutzer: %d freigeschaltet, %d angefragt, %d gesperrt, %d Admins\n",
		st.Users[store.UserAllowed], st.Users[store.UserPending], st.Users[store.UserBlocked], len(b.cfg.Admins))
	fmt.Fprintf(&sb, "Chats mit Kennzeichen: %d, Kennzeichen: %d\n", st.Chats, st.Plates)
	fmt.Fprintf(&sb, "Offene Parkvorgänge: %d, Parkvorgänge 30 Tage: %d\n\n", st.OpenSessions, st.Sessions30d)
	fmt.Fprintf(&sb, "Läuft seit: %s (%s)\n", fmtTime(ps.Started), fmtGap(time.Since(ps.Started)))
	if ps.LastPoll.IsZero() {
		sb.WriteString("Letzte Abfragerunde: noch keine\n")
	} else {
		fmt.Fprintf(&sb, "Letzte Abfragerunde: %s, Dauer %s\n", fmtTime(ps.LastPoll), ps.LastDuration.Round(time.Millisecond))
	}
	fmt.Fprintf(&sb, "Abfragen seit Start: %d, Fehler: %d\n", ps.Searches, ps.Errors)
	if ps.LastError != "" {
		fmt.Fprintf(&sb, "Letzter Fehler: <code>%s</code>\n", esc(truncate(ps.LastError, 300)))
	}
	fmt.Fprintf(&sb, "Abfrageintervall: %s, Healthcheck: %s", b.cfg.PollInterval, onOff(b.cfg.HealthcheckURL != ""))
	b.reply(ctx, chatID, sb.String(), nil)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// allChats returns every chat that should get general announcements.
func (b *Bot) allChats(ctx context.Context) []int64 {
	chats, err := b.store.Chats(ctx)
	if err != nil {
		slog.Error("list chats", "err", err)
	}
	seen := map[int64]bool{}
	var res []int64
	for _, ids := range [][]int64{chats, keys(b.cfg.Admins), keys(b.cfg.AllowedUsers)} {
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				res = append(res, id)
			}
		}
	}
	return res
}

func (b *Bot) cmdBroadcast(ctx context.Context, chatID, adminID int64, text string) {
	if text == "" {
		b.reply(ctx, chatID, "Bitte so: <code>/rundnachricht Wartung heute ab 20 Uhr</code>", nil)
		return
	}
	var sent, failed int
	for _, id := range b.allChats(ctx) {
		if err := b.tg.Send(ctx, id, "📢 "+esc(text), nil); err != nil {
			slog.Warn("broadcast", "chat", id, "err", err)
			failed++
			continue
		}
		sent++
	}
	slog.Info("broadcast", "admin", adminID, "sent", sent, "failed", failed)
	msg := fmt.Sprintf("📢 An %d Chats gesendet.", sent)
	if failed > 0 {
		msg += fmt.Sprintf(" %d fehlgeschlagen (z. B. Bot blockiert).", failed)
	}
	b.reply(ctx, chatID, msg, nil)
}
