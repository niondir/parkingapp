package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/niondir/parkingapp/internal/fairparken"
	"github.com/niondir/parkingapp/internal/telegram"
)

// track counts one use of a feature by a user (aggregated per day).
func (b *Bot) track(ctx context.Context, userID int64, feature string) {
	if feature == "" {
		return
	}
	day := b.clock().In(berlin).Format("2006-01-02")
	if err := b.store.CountUsage(ctx, day, userID, feature); err != nil {
		slog.Warn("count usage", "feature", feature, "err", err)
	}
}

func (b *Bot) remember(u telegram.User) {
	b.usersMu.Lock()
	b.users[u.ID] = u
	b.usersMu.Unlock()
}

func (b *Bot) knownUser(id int64) telegram.User {
	b.usersMu.Lock()
	defer b.usersMu.Unlock()
	if u, ok := b.users[id]; ok {
		return u
	}
	return telegram.User{ID: id}
}

// textFeature names the feature a text message uses.
func (b *Bot) textFeature(chatID int64, text string) string {
	switch {
	case text == menuButton:
		return "menu-button"
	case strings.HasPrefix(text, "/"):
		cmd, _, _ := strings.Cut(text, " ")
		cmd, _, _ = strings.Cut(strings.ToLower(cmd), "@")
		return cmd
	}
	if p := b.getPending(chatID); p != nil {
		return "input:" + inputNames[p.kind]
	}
	if p := fairparken.NormalizePlate(text); validPlate(p) && strings.ContainsAny(p, "0123456789") {
		return "plate-text"
	}
	return "text"
}

var inputNames = map[inputKind]string{
	inAdd: "add", inLabel: "label", inRename: "rename", inVoucher: "voucher", inPrice: "price",
	inDuration: "duration", inQuiet: "quiet", inBroadcast: "broadcast", inBroadcastConfirm: "broadcast",
	inGrace: "grace", inFeedback: "feedback", inFeedbackReply: "feedback-reply",
}

// featureLabels groups commands, buttons and inputs that belong to the same feature.
var featureLabels = map[string]string{
	"/start": "Start", "/menu": "Hauptmenü", "btn:m": "Hauptmenü", "menu-button": "Hauptmenü (Button unten)",
	"/help": "Hilfe", "btn:help": "Hilfe",
	"/list": "Kennzeichen-Liste", "btn:ml": "Kennzeichen-Liste", "btn:p": "Kennzeichen-Menü",
	"/add": "Kennzeichen hinzufügen", "btn:add": "Kennzeichen hinzufügen", "input:add": "Kennzeichen hinzufügen",
	"input:label": "Kennzeichen hinzufügen", "btn:lbl": "Kennzeichen hinzufügen", "btn:a": "Kennzeichen hinzufügen",
	"btn:rn": "Umbenennen", "btn:rnx": "Umbenennen", "input:rename": "Umbenennen",
	"/remove": "Entfernen", "btn:r": "Entfernen", "btn:rx": "Entfernen",
	"/check": "Prüfen", "btn:c": "Prüfen", "btn:chk": "Prüfen", "plate-text": "Prüfen (Kennzeichen geschickt)",
	"/bezahlen": "Bezahl-Link", "/tarife": "Tarife", "btn:t": "Tarife",
	"/rabatt": "Rabattcode", "btn:v": "Rabattcode", "input:voucher": "Rabattcode",
	"/verlauf": "Verlauf", "btn:hist": "Verlauf",
	"/kassenzettel": "Kassenzettel", "btn:rc": "Kassenzettel",
	"/einstellungen": "Einstellungen", "btn:s": "Einstellungen", "btn:sp": "Pausieren",
	"btn:sr": "Frist-Erinnerung umschalten", "btn:alr": "Frist-Erinnerung umschalten",
	"btn:sc": "Preiserhöhungen umschalten", "btn:alc": "Preiserhöhungen umschalten",
	"btn:al": "Alarme", "/alarm": "Preisalarm", "btn:pl": "Preisalarm", "btn:plv": "Preisalarm", "btn:pli": "Preisalarm", "input:price": "Preisalarm",
	"/dauer": "Dauer-Alarm", "btn:dl": "Dauer-Alarm", "btn:dlv": "Dauer-Alarm", "btn:dli": "Dauer-Alarm", "input:duration": "Dauer-Alarm",
	"/ruhezeit": "Ruhezeit", "btn:q": "Ruhezeit", "btn:qv": "Ruhezeit", "btn:qi": "Ruhezeit", "input:quiet": "Ruhezeit",
	"/feedback": "Feedback", "btn:fb": "Feedback", "btn:fbn": "Feedback", "input:feedback": "Feedback", "btn:fbm": "Eigenes Feedback ansehen",
	"btn:x": "Abbrechen", "text": "Freitext ohne Bedeutung",
	"push:new": "Push: neuer Parkvorgang", "push:exit": "Push: Ausfahrt mit Gebühr", "push:closed": "Push: Vorgang beendet",
	"push:reopened": "Push: Korrektur", "push:price-alarm": "Push: Preisalarm", "push:price-change": "Push: Preiserhöhung",
	"push:duration-alarm": "Push: Dauer-Alarm", "push:reminder": "Push: Zahlungserinnerung",
}

// adminFeatures are left out of the usage overview.
var adminFeatures = map[string]bool{
	"/users": true, "/allow": true, "/block": true, "/stats": true, "/rundnachricht": true, "/broadcast": true,
	"/karenz": true, "/kulanz": true, "/grace": true, "/nutzung": true,
	"btn:au": true, "btn:as": true, "btn:ab": true, "btn:abx": true, "btn:ua": true, "btn:ub": true,
	"btn:gr": true, "btn:grv": true, "btn:gri": true, "btn:us": true,
	"btn:fba": true, "btn:fbd": true, "btn:fbs": true, "btn:fbr": true,
	"input:grace": true, "input:broadcast": true, "input:feedback-reply": true,
}

func featureLabel(f string) string {
	if l, ok := featureLabels[f]; ok {
		return l
	}
	return f
}

func (b *Bot) showUsage(ctx context.Context, chatID, msgID int64, days int) {
	now := b.clock().In(berlin)
	since := func(d int) string { return now.AddDate(0, 0, -(d - 1)).Format("2006-01-02") }
	from := since(max(days, 30))
	all, err := b.store.UsageSince(ctx, from)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	active7, active30 := map[int64]bool{}, map[int64]bool{}
	byLabel := map[string]*usageRow{}
	var rows []*usageRow
	for _, u := range all {
		if adminFeatures[u.Feature] {
			continue
		}
		push := strings.HasPrefix(u.Feature, "push:")
		if !push {
			if u.Day >= since(7) {
				active7[u.UserID] = true
			}
			if u.Day >= since(30) {
				active30[u.UserID] = true
			}
		}
		if u.Day < since(days) {
			continue
		}
		// Commands, buttons and inputs of the same feature are merged.
		l := featureLabel(u.Feature)
		r := byLabel[l]
		if r == nil {
			r = &usageRow{label: l, push: push, users: map[int64]bool{}}
			byLabel[l] = r
			rows = append(rows, r)
		}
		r.count += u.Count
		r.users[u.UserID] = true
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		return rows[i].label < rows[j].label
	})
	var sb strings.Builder
	fmt.Fprintf(&sb, "📈 <b>Nutzung</b> – letzte %d Tage\n\nAktive Nutzer: %d (7 Tage), %d (30 Tage)\n", days, len(active7), len(active30))
	for _, push := range []bool{false, true} {
		if push {
			sb.WriteString("\n<b>Push-Nachrichten</b> (Anzahl · Empfänger)\n")
		} else {
			sb.WriteString("\n<b>Funktionen</b> (Interaktionen · Nutzer)\n")
		}
		n := 0
		for _, r := range rows {
			if r.push != push {
				continue
			}
			fmt.Fprintf(&sb, "%s: %d · %d\n", esc(r.label), r.count, len(r.users))
			n++
		}
		if n == 0 {
			sb.WriteString("–\n")
		}
	}
	sb.WriteString("\n<i>Gezählt pro Nutzer und Tag; Admin-Funktionen ausgenommen. Eine Interaktion ist ein Befehl, Button oder eine Eingabe.</i>")
	k := kb(row(btn("7 Tage", "us:7"), btn("30 Tage", "us:30"), btn("90 Tage", "us:90")), row(btn("⬅️ Hauptmenü", "m")))
	b.show(ctx, chatID, msgID, sb.String(), k)
}

type usageRow struct {
	label string
	count int
	users map[int64]bool
	push  bool
}
