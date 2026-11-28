package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/niondir/parkingapp/internal/fairparken"
	"github.com/niondir/parkingapp/internal/swp"
)

// Smart Web Pay (Scheidt & Bachmann) is searched in addition to fair parken.
// Its sessions are mapped onto fairparken.ParkingItem, so the monitor,
// alerts and history handle both alike. A search always needs a city; every
// chat sets one (see city.go).

// swpAPI is the part of the Smart Web Pay client the bot uses.
type swpAPI interface {
	Search(ctx context.Context, city, plate string) (swp.Result, error)
	Cities(ctx context.Context) ([]swp.City, error)
}

const providerSWP = "swp"

// swpItem maps a Smart Web Pay hit fetched at time now onto a parking item.
// Like fair parken's, the exit time is the fetch time while the car is
// parked. Only "late" sessions (left, pay afterwards) keep the calculation
// end as exit; what tariffTimeEnd means while parked is not verified.
func swpItem(h swp.Hit, plate string, now time.Time) fairparken.ParkingItem {
	out := now.UTC().Format(time.RFC3339)
	if end, ok := h.End(); ok && h.TransactionType == "late" {
		out = end.UTC().Format(time.RFC3339)
	}
	in := ""
	if start, ok := h.Start(); ok {
		in = start.UTC().Format(time.RFC3339)
	}
	it := fairparken.ParkingItem{
		Plate:        plate,
		RealUTCIn:    in,
		RealUTCOut:   out,
		SessionID:    h.TariffTimeStart,
		QuotaAddress: h.CarParkName,
		Provider:     providerSWP,
		ParkID:       h.ParkID(),
		City:         h.City,
		PayLink:      swp.PayURL(h.City, plate, &h),
	}
	if h.CarParkAddress != "" {
		it.QuotaAddition = h.CarParkAddress
	}
	if p, ok := h.Price(); ok {
		it.Price = &p
	}
	if p, ok := h.OriginalPrice(); ok {
		it.OriginalPrice = &p
	}
	raw, _ := json.Marshal(it)
	it.Raw = raw
	return it
}

// source names where a session comes from: "" for fair parken, "swp:<city>"
// for Smart Web Pay. Sessions are only closed when their source was checked.
func source(it fairparken.ParkingItem) string {
	if it.Provider == "" {
		return ""
	}
	return it.Provider + ":" + it.City
}

func swpSource(city string) string { return providerSWP + ":" + city }

// searchResult combines the answers of all providers for one plate.
type searchResult struct {
	res     []fairparken.SearchResult
	checked map[string]bool // sources that answered completely
	failed  []string        // human readable sources that failed
}

// allFailed reports whether no provider answered at all.
func (s searchResult) allFailed() bool { return len(s.checked) == 0 && len(s.failed) > 0 }

// searchAll asks fair parken and Smart Web Pay in the given cities.
func (b *Bot) searchAll(ctx context.Context, plate string, cities []string) searchResult {
	out := searchResult{checked: map[string]bool{}}
	res, err := b.fp.Search(ctx, plate)
	b.countSearch(err)
	if err != nil {
		slog.Warn("search", "plate", plate, "err", err)
		out.failed = append(out.failed, "fair parken")
	} else {
		out.res = res
		out.checked[""] = true
	}
	b.addSWP(ctx, &out, plate, cities)
	return out
}

// swpConcurrency limits parallel searches when all cities are searched.
const swpConcurrency = 6

func (b *Bot) addSWP(ctx context.Context, out *searchResult, plate string, cities []string) {
	if b.swp == nil || len(cities) == 0 {
		return
	}
	type answer struct {
		city string
		res  swp.Result
		err  error
	}
	answers := make([]answer, len(cities))
	sem := make(chan struct{}, swpConcurrency)
	var wg sync.WaitGroup
	for i, c := range cities {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r, err := b.swp.Search(ctx, c, plate)
			answers[i] = answer{c, r, err}
		}()
	}
	wg.Wait()
	for _, a := range answers {
		b.countSearch(a.err)
		if a.err != nil {
			slog.Warn("swp search", "plate", plate, "city", a.city, "err", a.err)
			out.failed = append(out.failed, "Smart Web Pay "+a.city)
			continue
		}
		if a.res.Complete {
			out.checked[swpSource(a.city)] = true
		}
		for _, h := range a.res.Hits {
			out.res = append(out.res, fairparken.SearchResult{AnprResult: swpItem(h, plate, b.clock())})
		}
	}
}

func (b *Bot) countSearch(err error) {
	b.statsMu.Lock()
	defer b.statsMu.Unlock()
	b.stats.Searches++
	if err != nil {
		b.stats.Errors++
		b.stats.LastError = b.clock().In(berlin).Format("02.01. 15:04") + " " + err.Error()
	}
}

// swpCityNames returns the names of all Smart Web Pay cities.
func (b *Bot) swpCityNames(ctx context.Context) ([]string, error) {
	if b.swp == nil {
		return nil, errors.New("smart web pay disabled")
	}
	cities, err := b.swp.Cities(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(cities))
	for i, c := range cities {
		names[i] = c.Name
	}
	return names, nil
}

// findCity resolves user input to Smart Web Pay cities: an exact match
// (ignoring case) wins, otherwise all cities containing the input.
func (b *Bot) findCity(ctx context.Context, input string) ([]swp.City, error) {
	if b.swp == nil {
		return nil, errors.New("smart web pay disabled")
	}
	cities, err := b.swp.Cities(ctx)
	if err != nil {
		return nil, err
	}
	q := foldCity(input)
	var res []swp.City
	for _, c := range cities {
		n := foldCity(c.Name)
		if n == q {
			return []swp.City{c}, nil
		}
		if q != "" && strings.Contains(n, q) {
			res = append(res, c)
		}
	}
	return res, nil
}

// foldCity makes "Muenchen", "münchen" and "MÜNCHEN" compare equal.
func foldCity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss").Replace(s)
}

// whereSearched describes the searched sources for "nothing found" messages.
func whereSearched(city string, allCities int) string {
	switch {
	case city != "":
		return "fair parken und Smart Web Pay in <b>" + esc(city) + "</b>"
	case allCities > 0:
		return fmt.Sprintf("fair parken und Smart Web Pay in allen %d Städten (keine Stadt gesetzt)", allCities)
	}
	return "fair parken"
}
