// Package swp is a client for the (undocumented) API behind
// https://smart-web-pay.scheidt-bachmann.net, the web payment page of
// Scheidt & Bachmann for car parks with the entervo system.
//
// The endpoints and fields were reverse engineered from the web client's
// JavaScript bundle. They are not a public API and may change without notice.
//
// A plate search is asynchronous: the client opens a SockJS/STOMP connection,
// subscribes to its user queue and then sends the search via REST. Every car
// park of the city answers on the queue, a FinishMessage ends the search.
// There is no search across all cities, so the caller always names one.
package swp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultAPIURL = "https://pm.prod.parking.scheidt-bachmann.net/smart-web-pay-service/api/"
	DefaultWSURL  = "https://pm.prod.parking.scheidt-bachmann.net/zrconnector-service/ZRConnector"
	WebURL        = "https://smart-web-pay.scheidt-bachmann.net/"
	// Country is fixed: the bot only serves German plates.
	Country = "DE"
)

// ErrCaptcha means the service asks for a reCAPTCHA, which only a browser can solve.
var ErrCaptcha = errors.New("smart web pay requires a captcha")

// ErrUnknownCity means the service has no car park in the city.
var ErrUnknownCity = errors.New("unknown city")

// City is a city with car parks that accept Smart Web Pay.
type City struct {
	Name  string    `json:"code"`
	Parks []CarPark `json:"car_parks"`
}

type CarPark struct {
	ID   string `json:"id"` // e.g. entervo.core@99589@8225
	Name string `json:"name"`
}

// Hit is an open parking session of the searched plate in one car park.
type Hit struct {
	City            string `json:"-"` // the searched city
	CarParkName     string `json:"carParkName"`
	CarParkAddress  string `json:"carParkAddress"`
	Operator        int64  `json:"operator"`
	OperatorName    string `json:"operatorName"`
	CellComputer    int64  `json:"cellComputer"` // the car park within the operator
	SystemTenant    string `json:"systemTenant"`
	Amount          string `json:"amount"` // minor units, see CurrencyDecimal
	OriginalAmount  string `json:"originalAmount"`
	Currency        string `json:"currency"`
	CurrencyDecimal int    `json:"currencyDecimal"`
	Duration        string `json:"duration"`
	TariffTimeStart string `json:"tariffTimeStart"` // entry, local time of the car park without zone (see Start)
	TariffTimeEnd   string `json:"tariffTimeEnd"`   // end of the calculation; after paying while parked equal to TariffTimeStart, the exit after leaving
	TransactionType string `json:"transactionType"` // "present" (still parked) or "late" (left, pay afterwards)
	MediumValue     string `json:"mediumValue"`
}

// Start returns the entry time. The service sends German local time without
// a zone (e.g. "2026-11-28T09:19:29" for 08:19:29 UTC); a value with a zone
// is taken as is.
func (h Hit) Start() (time.Time, bool) { return parseLocal(h.TariffTimeStart) }

// End returns the end of the calculation, see TariffTimeEnd.
func (h Hit) End() (time.Time, bool) { return parseLocal(h.TariffTimeEnd) }

// local is the time zone of all car parks (Country is fixed).
var local = mustLoad("Europe/Berlin")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func parseLocal(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Price converts Amount from minor units.
func (h Hit) Price() (float64, bool) { return minor(h.Amount, h.CurrencyDecimal) }

func (h Hit) OriginalPrice() (float64, bool) { return minor(h.OriginalAmount, h.CurrencyDecimal) }

func minor(s string, dec int) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	for range dec {
		v /= 10
	}
	return v, true
}

// ParkID identifies the car park in the payment page's "parking" parameter.
func (h Hit) ParkID() string { return fmt.Sprintf("%d@%d", h.Operator, h.CellComputer) }

// PayURL opens the payment page with country, city, car park and plate prefilled.
// The page adds the "entervo.core@" prefix to a two-part car park ID itself;
// car parks of other tenants are left for the user to pick.
func PayURL(city, plate string, h *Hit) string {
	q := url.Values{"country": {Country}, "city": {city}, "lpn": {plate}}
	if h != nil && h.SystemTenant == "entervo.core" {
		q.Set("parking", h.ParkID())
	}
	return WebURL + "parking/select?" + q.Encode()
}

// Result of a search in one city.
type Result struct {
	Hits []Hit
	// Complete is false if a car park did not answer; a session there may
	// still be open although it is missing in Hits.
	Complete bool
}

type Client struct {
	APIURL string
	WSURL  string
	HTTP   *http.Client
	// Timeout for one search; car parks answer within a few seconds.
	Timeout time.Duration

	mu       sync.Mutex
	cities   []City
	citiesAt time.Time
}

func New() *Client {
	return &Client{APIURL: DefaultAPIURL, WSURL: DefaultWSURL, HTTP: &http.Client{Timeout: 20 * time.Second}, Timeout: 30 * time.Second}
}

const userAgent = "parkingapp (+https://github.com/niondir/parkingapp)"

// citiesTTL keeps the city list cached; it changes rarely.
const citiesTTL = time.Hour

// Cities returns all German cities with Smart Web Pay car parks, sorted by name.
func (c *Client) Cities(ctx context.Context) ([]City, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cities != nil && time.Since(c.citiesAt) < citiesTTL {
		return c.cities, nil
	}
	var res struct {
		IsCaptcha string `json:"isCaptcha"`
		Cities    []City `json:"cities"`
	}
	u := c.APIURL + "cities-list?" + url.Values{"code": {Country}, "page": {"1"}, "limit": {"1000"}, "search": {""}}.Encode()
	if err := c.get(ctx, u, &res); err != nil {
		return nil, err
	}
	if res.IsCaptcha == "true" {
		return nil, ErrCaptcha
	}
	c.cities, c.citiesAt = res.Cities, time.Now()
	return c.cities, nil
}

// FindCity returns the city with the given name (case insensitive).
func (c *Client) FindCity(ctx context.Context, name string) (City, error) {
	cities, err := c.Cities(ctx)
	if err != nil {
		return City{}, err
	}
	for _, x := range cities {
		if strings.EqualFold(x.Name, strings.TrimSpace(name)) {
			return x, nil
		}
	}
	return City{}, ErrUnknownCity
}

func (c *Client) get(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Search looks for open sessions of plate in all car parks of city.
func (c *Client) Search(ctx context.Context, city, plate string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	clientID := newUUID()
	conn, err := dialSockJS(ctx, c.WSURL)
	if err != nil {
		return Result{}, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	if err := conn.sendSTOMP("CONNECT", map[string]string{"accept-version": "1.1,1.0", "heart-beat": "0,0", "username": clientID}, ""); err != nil {
		return Result{}, err
	}
	if f, err := conn.readSTOMP(); err != nil {
		return Result{}, err
	} else if f.command != "CONNECTED" {
		return Result{}, fmt.Errorf("stomp: got %s instead of CONNECTED: %s", f.command, f.body)
	}
	if err := conn.sendSTOMP("SUBSCRIBE", map[string]string{"id": "sub-0", "destination": "/users/queue/messages"}, ""); err != nil {
		return Result{}, err
	}
	// The subscription has no receipt; the web client sends right away too.
	if err := c.sendSearch(ctx, clientID, city, plate); err != nil {
		return Result{}, err
	}
	res := Result{Complete: true}
	answered := 0
	for {
		f, err := conn.readSTOMP()
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return Result{}, fmt.Errorf("waiting for answers (%d so far): %w", answered, err)
		}
		if f.command != "MESSAGE" {
			continue
		}
		done, err := res.add(f.body, city, &answered)
		if err != nil {
			return Result{}, err
		}
		if done {
			return res, nil
		}
	}
}

// message is one answer on the user queue.
type message struct {
	DataAction         string          `json:"dataAction"` // ResponseData per car park, FinishMessage at the end
	Data               json.RawMessage `json:"data"`
	ErrorCode          bool            `json:"errorCode"`
	ParkingUnreachable bool            `json:"parkingUnreachable"`
}

// add processes one queue message. It reports true for the final message.
func (r *Result) add(body, city string, answered *int) (bool, error) {
	var m message
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return false, fmt.Errorf("decode answer: %w", err)
	}
	if m.ParkingUnreachable {
		r.Complete = false
	}
	switch m.DataAction {
	case "FinishMessage":
		var s string
		_ = json.Unmarshal(m.Data, &s)
		switch {
		case strings.Contains(s, "tenant not found"), s == "CAR_PARK_NOT_FOUND":
			return true, fmt.Errorf("%w: %s", ErrUnknownCity, city)
		case *answered == 0 && m.ParkingUnreachable:
			return true, fmt.Errorf("no car park reachable: %s", s)
		}
		return true, nil
	case "ResponseData":
		*answered++
		var h Hit
		if err := json.Unmarshal(m.Data, &h); err != nil {
			return false, fmt.Errorf("decode hit: %w", err)
		}
		// Every car park answers, also without a session. The web client
		// only shows answers with an entry time (assumed, see README).
		if strings.TrimSpace(h.TariffTimeStart) == "" {
			return false, nil
		}
		h.City = city
		r.Hits = append(r.Hits, h)
	}
	return false, nil
}

func (c *Client) sendSearch(ctx context.Context, clientID, city, plate string) error {
	body, err := json.Marshal(map[string]any{
		"country": Country, "city": city, "parking": "ALL",
		"mediumValue": plate, "mediumType": "lpn", "clientId": clientID,
		"captchaToken": "", "latePayment": false, "present": true, "from": "navigation",
		"locales": "de-DE", "vouchers": []any{}, "prepayment": "",
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.APIURL+"v1/information?timeout=15000", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("search: %s", resp.Status)
	}
	return nil
}
