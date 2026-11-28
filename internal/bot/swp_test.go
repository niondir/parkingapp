package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/niondir/parkingapp/internal/swp"
)

// fakeSWP answers searches from in-memory hits per city and plate.
type fakeSWP struct {
	mu       sync.Mutex
	cities   []swp.City
	hits     map[string][]swp.Hit // city|plate
	fail     map[string]bool      // city
	searched []string             // cities in call order
}

func newFakeSWP() *fakeSWP {
	return &fakeSWP{
		cities: []swp.City{
			{Name: "Berlin", Parks: []swp.CarPark{{ID: "entervo.core@41029@3520", Name: "Mall of Berlin"}}},
			{Name: "Hamburg", Parks: []swp.CarPark{{ID: "entervo.core@99589@8225", Name: "Krohnstieg Center"}}},
			{Name: "München", Parks: []swp.CarPark{{ID: "entervo.core@1@2", Name: "PH Mitte"}}},
			{Name: "Mönchengladbach"},
		},
		hits: map[string][]swp.Hit{}, fail: map[string]bool{},
	}
}

func (f *fakeSWP) Cities(context.Context) ([]swp.City, error) { return f.cities, nil }

func (f *fakeSWP) Search(_ context.Context, city, plate string) (swp.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searched = append(f.searched, city)
	if f.fail[city] {
		return swp.Result{}, errors.New("unreachable")
	}
	var hits []swp.Hit
	for _, h := range f.hits[city+"|"+plate] {
		h.City = city
		hits = append(hits, h)
	}
	return swp.Result{Hits: hits, Complete: true}, nil
}

func (f *fakeSWP) set(city, plate string, h ...swp.Hit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits[city+"|"+plate] = h
}

func (f *fakeSWP) takeSearched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.searched
	f.searched = nil
	return s
}

func krohnstieg(in time.Time, amount string) swp.Hit {
	return swp.Hit{
		CarParkName: "Krohnstieg Center", CarParkAddress: "Krohnstieg 41-43, 22415 Hamburg",
		Operator: 99589, CellComputer: 8225, SystemTenant: "entervo.core",
		Amount: amount, OriginalAmount: amount, Currency: "EUR", CurrencyDecimal: 2,
		TariffTimeStart: swpTime(in), TariffTimeEnd: swpTime(time.Now()),
		TransactionType: "present",
	}
}

// swpTime formats like Smart Web Pay: German local time without zone.
func swpTime(t time.Time) string { return t.In(berlin).Format("2006-01-02T15:04:05") }

const swpPay = "https://smart-web-pay.scheidt-bachmann.net/parking/select?city=Hamburg&country=DE&lpn=HHKF860E&parking=99589%408225"

func setupSWP(t *testing.T) (*Bot, *fakeTG, *fakeAPI, *fakeSWP) {
	b, tg, api := setup(t)
	sw := newFakeSWP()
	b.swp = sw
	return b, tg, api, sw
}

func TestSWPMonitor(t *testing.T) {
	ctx := context.Background()
	b, tg, _, sw := setupSWP(t)
	b.handleText(ctx, 1, "/add HH-KF 860E Golf")
	b.handleText(ctx, 2, "/add HH-KF 860E")
	b.setCity(ctx, 1, 0, "Hamburg")
	tg.take()
	sw.takeSearched()

	b.pollAll(ctx)
	if s := sw.takeSearched(); len(s) != 1 || s[0] != "Hamburg" {
		t.Fatalf("monitor searched %v, want only the city of chat 1", s)
	}

	in := time.Now().Add(-40 * time.Minute)
	sw.set("Hamburg", "HHKF860E", krohnstieg(in, "350"))
	b.pollAll(ctx)
	m := tg.take()
	if len(m) != 2 {
		t.Fatalf("want a push to both watchers, got %d: %v", len(m), m)
	}
	if !strings.Contains(m[0].text, "Neuer Parkvorgang") || !strings.Contains(m[0].text, "Krohnstieg Center") ||
		!strings.Contains(m[0].text, "3,50 €") || strings.Contains(m[0].text, "Karenzzeit") ||
		!strings.Contains(m[0].text, "Parkt seit: 40 min") || !strings.Contains(m[0].text, "Einfahrt: "+fmtTime(in)) {
		t.Errorf("push: %s", m[0].text)
	}
	if bs := buttons(m[0]); bs[0] != swpPay || has(bs, "t:HHKF860E") {
		t.Errorf("buttons %v: want Smart Web Pay link and no tariffs", bs)
	}

	// The plate menu pays where the session is open.
	b.showPlate(ctx, 1, 0, "HHKF860E")
	if bs := buttons(tg.take()[0]); !has(bs, swpPay) {
		t.Errorf("plate menu %v", bs)
	}

	// A failed search must not close the session.
	sw.set("Hamburg", "HHKF860E")
	sw.fail["Hamburg"] = true
	b.pollAll(ctx)
	b.pollAll(ctx)
	if m := tg.take(); len(m) != 0 {
		t.Fatalf("failed search closed the session: %v", m)
	}
	sw.fail["Hamburg"] = false

	// Gone twice: closed, paid since a fee was shown.
	b.pollAll(ctx)
	b.pollAll(ctx)
	m = tg.take()
	if len(m) != 2 || !strings.Contains(m[0].text, "bezahlt") {
		t.Fatalf("close: %v", m)
	}
	b.showPlate(ctx, 1, 0, "HHKF860E")
	if bs := buttons(tg.take()[0]); !has(bs, "https://pay.fairparken.com/plate/HHKF860E") {
		t.Errorf("after closing the fair parken link is back: %v", bs)
	}
}

func TestSWPCityChangeKeepsSession(t *testing.T) {
	ctx := context.Background()
	b, tg, _, sw := setupSWP(t)
	b.handleText(ctx, 1, "/add HHKF860E")
	b.setCity(ctx, 1, 0, "Hamburg")
	sw.set("Hamburg", "HHKF860E", krohnstieg(time.Now().Add(-time.Hour), "0"))
	b.pollAll(ctx)
	tg.take()
	// Hamburg is no longer searched, so its session can't be seen as gone.
	b.setCity(ctx, 1, 0, "Berlin")
	b.pollAll(ctx)
	b.pollAll(ctx)
	tg.take()
	open, _ := b.store.OpenSessions(ctx, "HHKF860E")
	if len(open) != 1 {
		t.Fatalf("open sessions %d, want the Hamburg one kept", len(open))
	}
}

func TestSWPCheckWithCity(t *testing.T) {
	ctx := context.Background()
	b, tg, _, sw := setupSWP(t)
	b.setCity(ctx, 1, 0, "Hamburg")
	tg.take()
	b.handleText(ctx, 1, "HH-KF 860E")
	m := tg.take()
	if len(m) != 1 || !strings.Contains(m[0].text, "kein offener Parkvorgang") || !strings.Contains(m[0].text, "Smart Web Pay in <b>Hamburg</b>") {
		t.Fatalf("check: %v", m)
	}
	if bs := buttons(m[0]); bs[0] != "cty" || !strings.Contains(m[0].kb.InlineKeyboard[0][0].Text, "Hamburg") {
		t.Errorf("want a button to change the city: %v", bs)
	}
	if s := sw.takeSearched(); len(s) != 1 {
		t.Errorf("searched %v", s)
	}

	sw.fail["Hamburg"] = true
	b.handleText(ctx, 1, "HHKF860E")
	if m := tg.take(); !strings.Contains(m[0].text, "Nicht erreichbar: Smart Web Pay Hamburg") {
		t.Errorf("want a warning: %v", m[0].text)
	}
}

func TestSWPCheckAllCities(t *testing.T) {
	ctx := context.Background()
	b, tg, _, sw := setupSWP(t)
	sw.set("Hamburg", "HHKF860E", krohnstieg(time.Now().Add(-time.Hour), "200"))
	b.handleText(ctx, 1, "HHKF860E")
	m := tg.take()
	if len(m) != 2 || !strings.Contains(m[0].text, "allen 4 Smart-Web-Pay-Städten") {
		t.Fatalf("check: %v", m)
	}
	if bs := buttons(m[1]); bs[0] != swpPay || !has(bs, "cs:Hamburg") || !has(bs, "a:HHKF860E") {
		t.Errorf("hit buttons %v", bs)
	}
	if s := sw.takeSearched(); len(s) != 4 {
		t.Errorf("searched %v", s)
	}

	sw.set("Hamburg", "HHKF860E")
	b.handleText(ctx, 1, "HHKF860E")
	m = tg.take()
	if !strings.Contains(m[1].text, "allen 4 Städten (keine Stadt gesetzt)") || buttons(m[1])[0] != "cty" {
		t.Errorf("nothing found: %v", m[1])
	}
}

func TestCityMenu(t *testing.T) {
	ctx := context.Background()
	b, tg, _, _ := setupSWP(t)

	b.showMain(ctx, 1, 0)
	if bs := buttons(tg.take()[0]); !has(bs, "cty") {
		t.Fatalf("main menu %v", bs)
	}
	press(b, 1, 5, "cti")
	tg.take()
	b.handleText(ctx, 1, "Atlantis")
	if m := tg.take(); !strings.Contains(m[0].text, "keine Parkhäuser") || b.getPending(1) == nil {
		t.Fatalf("unknown city: %v", m)
	}
	b.handleText(ctx, 1, "muenchen")
	m := tg.take()
	last := m[len(m)-1]
	if !strings.Contains(last.text, "Stadt: <b>München</b>") || !strings.Contains(last.text, "PH Mitte") {
		t.Fatalf("set by typing: %v", last.text)
	}
	if c, _ := b.store.City(ctx, 1); c != "München" {
		t.Fatalf("city %q", c)
	}

	b.handleText(ctx, 1, "/stadt mön")
	m = tg.take()
	if !has(buttons(m[len(m)-1]), "cs:Mönchengladbach") || !has(buttons(m[len(m)-1]), "cs:München") {
		t.Fatalf("choice: %v", m)
	}
	press(b, 1, 7, "cs:Hamburg")
	m = tg.take()
	if bs := buttons(m[0]); !has(bs, "cs:München") || !has(bs, "cs:") || m[0].edit != 7 {
		t.Fatalf("menu after setting: %v", m)
	}
	if !strings.Contains(m[0].kb.InlineKeyboard[0][0].Text, "✅ Hamburg") {
		t.Errorf("current city first and marked: %+v", m[0].kb.InlineKeyboard[0])
	}

	press(b, 1, 7, "cs:Atlantis")
	if m := tg.take(); !strings.Contains(m[0].text, "keine Smart-Web-Pay-Stadt") {
		t.Errorf("unknown city by button: %v", m)
	}
	press(b, 1, 7, "cs:")
	if c, _ := b.store.City(ctx, 1); c != "" {
		t.Fatalf("city %q, want none", c)
	}
	tg.take()
}

func TestSWPLateSessionExits(t *testing.T) {
	ctx := context.Background()
	b, tg, _, sw := setupSWP(t)
	b.handleText(ctx, 1, "/add HHKF860E")
	b.setCity(ctx, 1, 0, "Hamburg")
	h := krohnstieg(time.Now().Add(-3*time.Hour), "500")
	h.TransactionType = "late"
	h.TariffTimeEnd = swpTime(time.Now().Add(-time.Hour))
	sw.set("Hamburg", "HHKF860E", h)
	tg.take()
	b.pollAll(ctx)
	m := tg.take()
	if len(m) != 1 || !strings.Contains(m[0].text, "Ausfahrt:") || !strings.Contains(m[0].text, "Offen: <b>5,00 €</b>") ||
		strings.Contains(m[0].text, "Online zahlbar bis") {
		t.Fatalf("late session: %v", m)
	}
	open, _ := b.store.OpenSessions(ctx, "HHKF860E")
	if len(open) != 1 || open[0].ExitedAt.IsZero() {
		t.Fatalf("want exit recorded: %+v", open)
	}
}

func TestRecentCities(t *testing.T) {
	ctx := context.Background()
	b, _, _, _ := setupSWP(t)
	for i := range 12 {
		if err := b.store.SetCity(ctx, 1, fmt.Sprintf("C%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.store.SetCity(ctx, 1, "C5"); err != nil {
		t.Fatal(err)
	}
	r, err := b.store.RecentCities(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(r) != 10 || r[0] != "C5" || r[1] != "C11" || has(r, "C1") {
		t.Fatalf("recent %v", r)
	}
	// Quiet hours and city share a row and must not overwrite each other.
	b.handleText(ctx, 1, "/ruhezeit 22-7")
	if c, _ := b.store.City(ctx, 1); c != "C5" {
		t.Fatalf("city %q after quiet hours", c)
	}
	if cs, _ := b.store.GetChatSettings(ctx, 1); cs.QuietFrom != 22 {
		t.Fatalf("quiet %+v", cs)
	}
}

func TestWatchedCitiesSkipsPaused(t *testing.T) {
	ctx := context.Background()
	b, _, _, _ := setupSWP(t)
	b.handleText(ctx, 1, "/add HHKF860E")
	b.handleText(ctx, 2, "/add HHKF860E")
	b.store.SetCity(ctx, 1, "Hamburg")
	b.store.SetCity(ctx, 2, "Berlin")
	p, _ := b.store.GetPlate(ctx, 2, "HHKF860E")
	p.Paused = true
	b.store.SetPlateSettings(ctx, 2, "HHKF860E", p.PlateSettings)
	c, err := b.store.WatchedCities(ctx, "HHKF860E")
	if err != nil || len(c) != 1 || c[0] != "Hamburg" {
		t.Fatalf("cities %v err %v", c, err)
	}
}
