// Package fairparken is a client for the (undocumented) API behind
// https://pay.fairparken.com, the FreeFlow web client of fair parken GmbH.
//
// The endpoints and fields were reverse engineered from the web client's
// JavaScript bundle. They are not a public API and may change without notice.
package fairparken

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

const DefaultBaseURL = "https://pay.fairparken.com/"

// ParkingItem is the "anprResult" of a search hit.
type ParkingItem struct {
	Plate         string   `json:"plate"`
	AreaGkz       int64    `json:"areaGkz"`
	QuotaID       int64    `json:"quotaID"`
	QuotaAddress  string   `json:"quotaAddress"`
	TariffID      *int64   `json:"tariffId"`
	RealUTCIn     string   `json:"realUTC_IN"`
	RealUTCOut    string   `json:"realUTC_OUT"`
	Price         *float64 `json:"price"`
	OriginalPrice *float64 `json:"originalPrice"`
	VoucherValue  *float64 `json:"voucherValue"`
	VoucherCode   *string  `json:"voucherCode,omitempty"`
	OrderType     string   `json:"orderType"`

	// Raw keeps all fields as returned by the API, so unknown fields survive
	// a round trip through Quote.
	Raw json.RawMessage `json:"-"`
}

func (p *ParkingItem) UnmarshalJSON(b []byte) error {
	type plain ParkingItem
	if err := json.Unmarshal(b, (*plain)(p)); err != nil {
		return err
	}
	p.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// Key identifies a parking session.
func (p ParkingItem) Key() string {
	return fmt.Sprintf("%d|%d|%s", p.AreaGkz, p.QuotaID, p.RealUTCIn)
}

// In returns the entry time. ok is false if the API did not deliver one.
func (p ParkingItem) In() (time.Time, bool) { return parseTime(p.RealUTCIn) }

// Out returns the exit time (or the time of calculation while still parked).
func (p ParkingItem) Out() (time.Time, bool) { return parseTime(p.RealUTCOut) }

func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			if t.Year() < 2000 {
				return time.Time{}, false
			}
			return t, true
		}
	}
	return time.Time{}, false
}

// SearchResult is one element of the AnprResultSearch response.
type SearchResult struct {
	AnprResult ParkingItem       `json:"anprResult"`
	Articles   []json.RawMessage `json:"articles"`
}

// Tariff is a vehicle category of a parking area. Icon is null for cars.
type Tariff struct {
	ID   int64   `json:"id"`
	Icon *string `json:"icon"`
}

func (t Tariff) Name() string {
	if t.Icon == nil || *t.Icon == "" {
		return "Auto"
	}
	return *t.Icon
}

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func New() *Client {
	return &Client{BaseURL: DefaultBaseURL, HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// NormalizePlate turns "HH-KF 860E" into "HHKF860E".
func NormalizePlate(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// PayURL opens the official payment page with the plate prefilled.
func PayURL(plate string) string {
	return DefaultBaseURL + "plate/" + url.PathEscape(plate)
}

// Search returns the open parking sessions of a plate (empty if none).
func (c *Client) Search(ctx context.Context, plate string) ([]SearchResult, error) {
	var res []SearchResult
	err := c.do(ctx, http.MethodPost, "api/AnprResults/AnprResultSearch",
		map[string]string{"plate": plate, "voucherCode": ""}, &res)
	return res, err
}

// Tariffs returns the vehicle categories available for a quota.
func (c *Client) Tariffs(ctx context.Context, quotaID int64) ([]Tariff, error) {
	var res []Tariff
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("api/AnprResults/GetTariffs/%d", quotaID), nil, &res)
	return res, err
}

// Quote recalculates the price of a parking session for another tariff.
// The web client calls the same endpoint when the user picks a tariff.
func (c *Client) Quote(ctx context.Context, item ParkingItem, tariffID int64) (ParkingItem, error) {
	body := map[string]any{}
	if len(item.Raw) > 0 {
		if err := json.Unmarshal(item.Raw, &body); err != nil {
			return ParkingItem{}, err
		}
	}
	body["tariffId"] = tariffID
	var res struct {
		ParkingItem ParkingItem `json:"parkingItem"`
	}
	err := c.do(ctx, http.MethodPost, "api/Payment/ApplyVoucherCode", body, &res)
	return res.ParkingItem, err
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "parkingapp (+https://github.com/niondir/parkingapp)")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: HTTP %d: %.200s", method, path, resp.StatusCode, b)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}
