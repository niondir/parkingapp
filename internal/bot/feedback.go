package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/niondir/parkingapp/internal/store"
	"github.com/niondir/parkingapp/internal/telegram"
)

const maxFeedbackLen = 2000

var feedbackStates = []struct{ status, label string }{
	{store.FeedbackNew, "🆕 neu"},
	{store.FeedbackProgress, "🔧 in Arbeit"},
	{store.FeedbackDone, "✅ erledigt"},
	{store.FeedbackRejected, "🚫 abgelehnt"},
}

func feedbackStatusLabel(status string) string {
	for _, s := range feedbackStates {
		if s.status == status {
			return s.label
		}
	}
	return status
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	return truncate(s, n)
}

// --- users ---

func (b *Bot) showFeedbackMenu(ctx context.Context, chatID, msgID int64) {
	b.show(ctx, chatID, msgID, "💬 <b>Feedback</b>\n\nFehler gefunden, eine Idee oder Lob? Schreib uns – jede Nachricht wird gespeichert und bearbeitet. Den Stand siehst du unter „Mein Feedback“.",
		kb(row(btn("✍️ Feedback schreiben", "fbn"), btn("📋 Mein Feedback", "fbm")), row(btn("⬅️ Hauptmenü", "m"))))
}

func (b *Bot) askFeedback(ctx context.Context, chatID int64) {
	b.ask(ctx, chatID, inFeedback, "", "", "✍️ Was möchtest du uns mitteilen? Schick mir einfach eine Nachricht.")
}

func (b *Bot) saveFeedback(ctx context.Context, chatID int64, text string) {
	text = strings.TrimSpace(text)
	if r := []rune(text); len(r) > maxFeedbackLen {
		text = string(r[:maxFeedbackLen])
	}
	u := b.knownUser(chatID)
	id, err := b.store.AddFeedback(ctx, chatID, u.Username, u.FirstName, text)
	if err != nil {
		b.fail(ctx, chatID, "Speichern", err)
		return
	}
	slog.Info("feedback", "id", id, "user", chatID)
	b.reply(ctx, chatID, fmt.Sprintf("🙏 Danke! Gespeichert als <b>#%d</b> (Status: %s). Du bekommst Bescheid, wenn sich etwas tut.", id, feedbackStatusLabel(store.FeedbackNew)),
		kb(row(btn("📋 Mein Feedback", "fbm"))))
	f, err := b.store.GetFeedback(ctx, id)
	if err != nil {
		slog.Error("load feedback", "err", err)
		return
	}
	for admin := range b.cfg.Admins {
		b.reply(ctx, admin, "📥 <b>Neues Feedback</b>\n\n"+feedbackDetail(f), feedbackKeyboard(f))
	}
}

func (b *Bot) showOwnFeedback(ctx context.Context, chatID, msgID int64) {
	list, err := b.store.ListFeedback(ctx, chatID, false, 10)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	var sb strings.Builder
	sb.WriteString("📋 <b>Mein Feedback</b>\n")
	if len(list) == 0 {
		sb.WriteString("\nNoch keins geschrieben.")
	}
	for _, f := range list {
		fmt.Fprintf(&sb, "\n<b>#%d</b> · %s · %s\n%s\n", f.ID, f.CreatedAt.In(berlin).Format("02.01."), feedbackStatusLabel(f.Status), esc(snippet(f.Text, 200)))
		if f.Reply != "" {
			fmt.Fprintf(&sb, "↳ <i>Antwort:</i> %s\n", esc(snippet(f.Reply, 300)))
		}
	}
	b.show(ctx, chatID, msgID, strings.TrimRight(sb.String(), "\n"),
		kb(row(btn("✍️ Neues Feedback", "fbn"), btn("⬅️ Hauptmenü", "m"))))
}

// --- admins ---

func feedbackDetail(f store.Feedback) string {
	from := fmtUser(store.User{ID: f.UserID, Username: f.Username, Name: f.Name})
	s := fmt.Sprintf("<b>#%d</b> · %s\nVon %s am %s\n\n%s", f.ID, feedbackStatusLabel(f.Status), from, fmtTime(f.CreatedAt), esc(f.Text))
	if f.Reply != "" {
		s += "\n\n↳ <i>Antwort:</i> " + esc(f.Reply)
	}
	return s
}

func feedbackKeyboard(f store.Feedback) *telegram.InlineKeyboard {
	id := strconv.FormatInt(f.ID, 10)
	var states []telegram.InlineButton
	for _, s := range feedbackStates {
		if s.status != f.Status {
			states = append(states, btn(s.label, "fbs:"+id+":"+s.status))
		}
	}
	return kb(states, row(btn("💬 Antworten", "fbr:"+id), btn("📥 Alle offenen", "fba")))
}

func (b *Bot) showFeedbackList(ctx context.Context, chatID, msgID int64, all bool) {
	list, err := b.store.ListFeedback(ctx, 0, !all, 15)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	title := "📥 <b>Offenes Feedback</b>"
	toggle := btn("Alle anzeigen", "fba:all")
	if all {
		title = "📥 <b>Feedback</b> (neueste 15)"
		toggle = btn("Nur offene", "fba")
	}
	var sb strings.Builder
	sb.WriteString(title + "\n")
	k := kb()
	if len(list) == 0 {
		sb.WriteString("\nNichts da.")
	}
	for _, f := range list {
		name := f.Name
		if name == "" {
			name = strconv.FormatInt(f.UserID, 10)
		}
		fmt.Fprintf(&sb, "\n<b>#%d</b> · %s · %s · %s\n%s\n", f.ID, feedbackStatusLabel(f.Status), esc(name),
			f.CreatedAt.In(berlin).Format("02.01."), esc(snippet(f.Text, 120)))
		k.InlineKeyboard = append(k.InlineKeyboard, row(btn(fmt.Sprintf("#%d %s", f.ID, snippet(f.Text, 30)), fmt.Sprintf("fbd:%d", f.ID))))
	}
	k.InlineKeyboard = append(k.InlineKeyboard, row(toggle, btn("⬅️ Hauptmenü", "m")))
	b.show(ctx, chatID, msgID, strings.TrimRight(sb.String(), "\n"), k)
}

func (b *Bot) showFeedbackDetail(ctx context.Context, chatID, msgID, id int64, note string) {
	f, err := b.store.GetFeedback(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		b.show(ctx, chatID, msgID, fmt.Sprintf("Feedback #%d gibt es nicht.", id), nil)
		return
	}
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	text := feedbackDetail(f)
	if note != "" {
		text = note + "\n\n" + text
	}
	b.show(ctx, chatID, msgID, text, feedbackKeyboard(f))
}

func (b *Bot) setFeedbackStatus(ctx context.Context, chatID, msgID, adminID, id int64, status string) {
	f, err := b.store.GetFeedback(ctx, id)
	if err != nil {
		b.showFeedbackDetail(ctx, chatID, msgID, id, "")
		return
	}
	if f.Status == status {
		b.showFeedbackDetail(ctx, chatID, msgID, id, "")
		return
	}
	if err := b.store.SetFeedbackStatus(ctx, id, status); err != nil {
		b.fail(ctx, chatID, "Speichern", err)
		return
	}
	slog.Info("feedback status", "id", id, "status", status, "admin", adminID)
	b.reply(ctx, f.UserID, fmt.Sprintf("📋 Dein Feedback <b>#%d</b> ist jetzt: %s\n„%s“", id, feedbackStatusLabel(status), esc(snippet(f.Text, 150))),
		kb(row(btn("📋 Mein Feedback", "fbm"))))
	b.showFeedbackDetail(ctx, chatID, msgID, id, "✅ Status geändert, Nutzer informiert.")
}

func (b *Bot) saveFeedbackReply(ctx context.Context, chatID, id int64, reply string) {
	f, err := b.store.GetFeedback(ctx, id)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	if err := b.store.SetFeedbackReply(ctx, id, strings.TrimSpace(reply)); err != nil {
		b.fail(ctx, chatID, "Speichern", err)
		return
	}
	b.reply(ctx, f.UserID, fmt.Sprintf("💬 <b>Antwort auf dein Feedback #%d</b>\n\n%s\n\n<i>Dein Feedback:</i> „%s“", id, esc(reply), esc(snippet(f.Text, 150))),
		kb(row(btn("📋 Mein Feedback", "fbm"))))
	b.showFeedbackDetail(ctx, chatID, 0, id, "✅ Antwort gesendet.")
}

// handleFeedbackAdminCallback handles the admin buttons of the feedback inbox.
func (b *Bot) handleFeedbackAdminCallback(ctx context.Context, chatID, msgID, adminID int64, kind, arg string) bool {
	switch kind {
	case "fba":
		b.showFeedbackList(ctx, chatID, msgID, arg == "all")
	case "fbd":
		if id, err := strconv.ParseInt(arg, 10, 64); err == nil {
			b.showFeedbackDetail(ctx, chatID, msgID, id, "")
		}
	case "fbs":
		idStr, status, _ := strings.Cut(arg, ":")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || feedbackStatusLabel(status) == status {
			return true
		}
		b.setFeedbackStatus(ctx, chatID, msgID, adminID, id, status)
	case "fbr":
		if _, err := strconv.ParseInt(arg, 10, 64); err == nil {
			b.ask(ctx, chatID, inFeedbackReply, "", arg, "💬 Antwort auf Feedback <b>#"+esc(arg)+"</b>? Sie geht direkt an den Nutzer.")
		}
	default:
		return false
	}
	return true
}
