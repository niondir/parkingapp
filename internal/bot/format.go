package bot

import (
	"fmt"
	"html"
	"math"
	"strings"
	"time"

	"github.com/niondir/parkingapp/internal/fairparken"
	"github.com/niondir/parkingapp/internal/store"
	"github.com/niondir/parkingapp/internal/telegram"
)

// PayWindow is how long a session can be paid online after leaving.
const PayWindow = 48 * time.Hour

var berlin = mustLoad("Europe/Berlin")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func esc(s string) string { return html.EscapeString(s) }

func fmtTime(t time.Time) string { return t.In(berlin).Format("02.01.2006 15:04") }

func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Minute)
	days := int(d / (24 * time.Hour))
	h := int(d % (24 * time.Hour) / time.Hour)
	m := int(d % time.Hour / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%d d %d h %d min", days, h, m)
	case h > 0:
		return fmt.Sprintf("%d h %d min", h, m)
	default:
		return fmt.Sprintf("%d min", m)
	}
}

// fmtEuro formats a price as delivered by the API (in Euro, see README).
func fmtEuro(v float64) string {
	s := fmt.Sprintf("%.2f", math.Abs(v))
	s = strings.Replace(s, ".", ",", 1)
	if v < 0 {
		s = "-" + s
	}
	return s + " €"
}

func plateTitle(plate, label string) string {
	if label != "" {
		return fmt.Sprintf("<b>%s</b> (%s)", esc(plate), esc(label))
	}
	return "<b>" + esc(plate) + "</b>"
}

// fmtItem renders a parking session as fetched at time at.
func fmtItem(it fairparken.ParkingItem, label string, at time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🚗 %s\n", plateTitle(it.Plate, label))
	if loc := it.Location(); loc != "" {
		fmt.Fprintf(&b, "📍 %s\n", esc(loc))
	}
	in, okIn := it.In()
	out, okOut := it.Out()
	parked := it.Parked(at)
	if okIn {
		fmt.Fprintf(&b, "⏵ Einfahrt: %s\n", fmtTime(in))
	}
	if okOut && !parked {
		fmt.Fprintf(&b, "⏹ Ausfahrt: %s\n", fmtTime(out))
	}
	if okIn && okOut {
		if parked {
			fmt.Fprintf(&b, "⏱ Parkt seit: %s\n", fmtDuration(out.Sub(in)))
		} else {
			fmt.Fprintf(&b, "⏱ Dauer: %s\n", fmtDuration(out.Sub(in)))
		}
	}
	if it.Price != nil {
		if parked {
			fmt.Fprintf(&b, "💶 Bei Zahlung jetzt: <b>%s</b>", fmtEuro(*it.Price))
			if paid, ok := it.Paid(); ok {
				fmt.Fprintf(&b, " (deckt bis %s)", paid.In(berlin).Format("15:04"))
			}
		} else {
			fmt.Fprintf(&b, "💶 Offen: <b>%s</b>", fmtEuro(*it.Price))
		}
		if it.OriginalPrice != nil && *it.OriginalPrice != *it.Price {
			fmt.Fprintf(&b, " (regulär %s)", fmtEuro(*it.OriginalPrice))
		}
		b.WriteString("\n")
	}
	if parked {
		b.WriteString("ℹ️ Fährst du in einer Freiparkzeit aus, fällt nichts an.\n")
	} else if okOut {
		fmt.Fprintf(&b, "⌛ Online zahlbar bis ca. %s\n", fmtTime(out.Add(PayWindow)))
	}
	if it.HasVoucher {
		b.WriteString("🎟 Rabattcode möglich\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// fmtClosed renders a session that disappeared from the API. lastSeen is
// when the bot saw it the last time.
func fmtClosed(it fairparken.ParkingItem, label, outcome string, lastSeen time.Time) string {
	var b strings.Builder
	title := plateTitle(it.Plate, label)
	switch outcome {
	case store.OutcomePaid:
		fmt.Fprintf(&b, "✅ Parkvorgang von %s ist bezahlt.", title)
	default:
		fmt.Fprintf(&b, "✅ %s ist ausgefahren, ohne dass eine Gebühr offen ist – kostenlos (z. B. Freiparkzeit) oder vor der Ausfahrt bezahlt.", title)
	}
	if loc := it.Location(); loc != "" {
		fmt.Fprintf(&b, "\n📍 %s", esc(loc))
	}
	if in, ok := it.In(); ok {
		end := lastSeen
		if out, ok := it.Out(); ok && outcome == store.OutcomePaid {
			end = out
		}
		fmt.Fprintf(&b, "\n⏱ %s – %s, ca. %s", fmtTime(in), end.In(berlin).Format("15:04"), fmtDuration(end.Sub(in)))
	}
	if outcome == store.OutcomePaid && it.Price != nil {
		fmt.Fprintf(&b, "\n💶 %s", fmtEuro(*it.Price))
	}
	return b.String()
}

// fmtResult renders a search hit including day tickets offered at the area.
func fmtResult(r fairparken.SearchResult, label string, at time.Time) string {
	s := fmtItem(r.AnprResult, label, at)
	if len(r.Articles) == 0 {
		return s
	}
	s += "\n\n🎫 <b>Zusätzlich buchbar</b> (auf der Bezahlseite):"
	for _, a := range r.Articles {
		s += "\n• " + esc(a.DisplayName())
		if a.Price.Amount > 0 {
			s += ": " + fmtEuro(a.Price.Amount/100)
		}
	}
	return s
}

func itemKeyboard(plate string) *telegram.InlineKeyboard {
	return &telegram.InlineKeyboard{InlineKeyboard: [][]telegram.InlineButton{
		{{Text: "💳 Jetzt bezahlen", URL: fairparken.PayURL(plate)}},
		{{Text: "🔄 Aktualisieren", CallbackData: "c:" + plate}, {Text: "🏷 Tarife", CallbackData: "t:" + plate}},
	}}
}
