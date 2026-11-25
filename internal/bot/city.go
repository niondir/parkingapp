package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/niondir/parkingapp/internal/telegram"
)

// Every chat has one Smart Web Pay city (or none). The monitor searches the
// chat's plates there; a manual check without a city searches all cities.

// cityRow is the button to change the city, shown where the city matters.
func cityRow(city string) []telegram.InlineButton {
	if city == "" {
		return row(btn("📍 Stadt setzen", "cty"))
	}
	return row(btn("📍 Stadt wechseln ("+city+")", "cty"))
}

// cityButton is the city entry of the main menu.
func (b *Bot) cityButton(ctx context.Context, chatID int64) telegram.InlineButton {
	city, err := b.store.City(ctx, chatID)
	if err != nil {
		slog.Warn("load city", "chat", chatID, "err", err)
	}
	if city == "" {
		return btn("📍 Stadt setzen", "cty")
	}
	return btn("📍 "+city, "cty")
}

// maxCallbackData is Telegram's limit for callback_data in bytes.
const maxCallbackData = 64

func (b *Bot) showCity(ctx context.Context, chatID, msgID int64, note string) {
	if b.swp == nil {
		b.show(ctx, chatID, msgID, "Smart Web Pay ist abgeschaltet.", kb(row(btn("⬅️ Hauptmenü", "m"))))
		return
	}
	city, err := b.store.City(ctx, chatID)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	recent, err := b.store.RecentCities(ctx, chatID)
	if err != nil {
		b.fail(ctx, chatID, "Laden", err)
		return
	}
	var sb strings.Builder
	if note != "" {
		sb.WriteString(note + "\n\n")
	}
	sb.WriteString("📍 <b>Deine Stadt</b>\n\n")
	if city == "" {
		sb.WriteString("Aktuell: <b>keine</b> – ich überwache nur fair parken. „Prüfen“ sucht dann in allen Städten und dauert länger.")
	} else {
		fmt.Fprintf(&sb, "Aktuell: <b>%s</b>", esc(city))
		if parks := b.cityParks(ctx, city); parks != "" {
			sb.WriteString("\n🅿️ " + parks)
		}
	}
	sb.WriteString("\n\nIn dieser Stadt suche ich zusätzlich in den Parkhäusern, die über Smart Web Pay (Scheidt &amp; Bachmann) bezahlt werden. Es gilt immer eine Stadt für alle deine Kennzeichen.")

	k := kb()
	var r []telegram.InlineButton
	for _, c := range recent {
		if len("cs:"+c) > maxCallbackData {
			continue
		}
		label := c
		if c == city {
			label = "✅ " + c
		}
		r = append(r, btn(label, "cs:"+c))
		if len(r) == 2 {
			k.InlineKeyboard = append(k.InlineKeyboard, r)
			r = nil
		}
	}
	if len(r) > 0 {
		k.InlineKeyboard = append(k.InlineKeyboard, r)
	}
	last := row(btn("🔎 Andere Stadt", "cti"))
	if city != "" {
		last = append(last, btn("🚫 Keine Stadt", "cs:"))
	}
	k.InlineKeyboard = append(k.InlineKeyboard, last, row(btn("⬅️ Hauptmenü", "m")))
	b.show(ctx, chatID, msgID, sb.String(), k)
}

// cityParks lists the car parks of a city, or "" if unknown.
func (b *Bot) cityParks(ctx context.Context, city string) string {
	cities, err := b.swp.Cities(ctx)
	if err != nil {
		slog.Warn("swp cities", "err", err)
		return ""
	}
	for _, c := range cities {
		if c.Name != city {
			continue
		}
		names := make([]string, len(c.Parks))
		for i, p := range c.Parks {
			names[i] = esc(p.Name)
		}
		return strings.Join(names, ", ")
	}
	return ""
}

func (b *Bot) askCity(ctx context.Context, chatID int64) {
	b.ask(ctx, chatID, inCity, "", "", "🔎 In welcher Stadt parkst du? Schick mir den Namen, z. B. <i>Hamburg</i> – ein Teil reicht.")
}

// handleCityInput resolves a typed city: one match is set, several are
// offered. Without a match it asks again (asking = an open question exists).
func (b *Bot) handleCityInput(ctx context.Context, chatID int64, text string, asking bool) {
	again := func(msg string) {
		if asking {
			b.retry(ctx, chatID, msg)
			return
		}
		b.reply(ctx, chatID, "⚠️ "+msg, nil)
		b.askCity(ctx, chatID)
	}
	matches, err := b.findCity(ctx, text)
	if err != nil {
		b.endPending(ctx, chatID, "")
		b.fail(ctx, chatID, "Städteliste laden", err)
		return
	}
	switch {
	case len(matches) == 1:
		b.endPending(ctx, chatID, "→ "+esc(matches[0].Name))
		b.setCity(ctx, chatID, 0, matches[0].Name)
	case len(matches) == 0:
		again("In „" + esc(strings.TrimSpace(text)) + "“ gibt es keine Parkhäuser mit Smart Web Pay.")
	case len(matches) > 12:
		again(fmt.Sprintf("%d Städte passen – bitte genauer.", len(matches)))
	default:
		b.endPending(ctx, chatID, "")
		k := kb()
		for _, c := range matches {
			if len("cs:"+c.Name) <= maxCallbackData {
				k.InlineKeyboard = append(k.InlineKeyboard, row(btn(c.Name, "cs:"+c.Name)))
			}
		}
		k.InlineKeyboard = append(k.InlineKeyboard, row(btn("⬅️ Zurück", "cty")))
		b.reply(ctx, chatID, "Welche Stadt meinst du?", k)
	}
}

// setCity stores the city after checking it exists ("" = none).
func (b *Bot) setCity(ctx context.Context, chatID, msgID int64, city string) {
	if city != "" {
		matches, err := b.findCity(ctx, city)
		if err != nil {
			b.fail(ctx, chatID, "Städteliste laden", err)
			return
		}
		if len(matches) != 1 || !strings.EqualFold(matches[0].Name, city) {
			b.reply(ctx, chatID, "„"+esc(city)+"“ ist keine Smart-Web-Pay-Stadt.", kb(row(btn("📍 Stadt wählen", "cty"))))
			return
		}
		city = matches[0].Name
	}
	if err := b.store.SetCity(ctx, chatID, city); err != nil {
		b.fail(ctx, chatID, "Speichern", err)
		return
	}
	note := "✅ Keine Stadt mehr – ich überwache nur fair parken."
	if city != "" {
		note = "✅ Stadt: <b>" + esc(city) + "</b>. Ich überwache deine Kennzeichen jetzt auch dort."
	}
	b.showCity(ctx, chatID, msgID, note)
}

// cmdCity handles /stadt [Name].
func (b *Bot) cmdCity(ctx context.Context, chatID int64, args string) {
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "":
		b.showCity(ctx, chatID, 0, "")
	case "aus", "keine":
		b.setCity(ctx, chatID, 0, "")
	default:
		b.handleCityInput(ctx, chatID, args, false)
	}
}
