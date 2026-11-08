package bot

import (
	"fmt"
	"html"
	"math"
	"strings"
	"time"

	"github.com/niondir/parkingapp/internal/fairparken"
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

// fmtItem renders a parking session.
func fmtItem(it fairparken.ParkingItem, label string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🚗 %s\n", plateTitle(it.Plate, label))
	if it.QuotaAddress != "" {
		fmt.Fprintf(&b, "📍 %s\n", esc(it.QuotaAddress))
	}
	in, okIn := it.In()
	out, okOut := it.Out()
	if okIn {
		fmt.Fprintf(&b, "⏵ Einfahrt: %s\n", fmtTime(in))
	}
	if okOut {
		fmt.Fprintf(&b, "⏹ Bis: %s\n", fmtTime(out))
	}
	if okIn && okOut {
		fmt.Fprintf(&b, "⏱ Dauer: %s\n", fmtDuration(out.Sub(in)))
	}
	if it.Price != nil {
		fmt.Fprintf(&b, "💶 Gebühr: <b>%s</b>", fmtEuro(*it.Price))
		if it.OriginalPrice != nil && *it.OriginalPrice != *it.Price {
			fmt.Fprintf(&b, " (regulär %s)", fmtEuro(*it.OriginalPrice))
		}
		b.WriteString("\n")
	}
	if okOut {
		fmt.Fprintf(&b, "⌛ Online zahlbar bis ca. %s\n", fmtTime(out.Add(PayWindow)))
	}
	return strings.TrimRight(b.String(), "\n")
}

func itemKeyboard(plate string) *telegram.InlineKeyboard {
	return &telegram.InlineKeyboard{InlineKeyboard: [][]telegram.InlineButton{
		{{Text: "💳 Jetzt bezahlen", URL: fairparken.PayURL(plate)}},
		{{Text: "🔄 Aktualisieren", CallbackData: "c:" + plate}, {Text: "🏷 Tarife", CallbackData: "t:" + plate}},
	}}
}
