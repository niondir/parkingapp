package fairparken

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNormalizePlate(t *testing.T) {
	for in, want := range map[string]string{
		"HH-KF 860E": "HHKF860E",
		"hhkf860e":   "HHKF860E",
		" m-ü 12 ":   "MÜ12",
	} {
		if got := NormalizePlate(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

// Field names as used by the web client (mock data in its bundle).
const sample = `{"orderType":"","areaGkz":0,"duration":540,"originalPrice":5,"plate":"LH-K2505","price":4.5,
"quotaAddress":"Sentruper Str. 315 48161 Münster [Allwetterzoo]","quotaID":1,
"realUTC_IN":"2025-06-24 10:16:00","realUTC_OUT":"2025-06-24 10:25:00","voucherValue":0,"tariffId":2}`

func TestParkingItem(t *testing.T) {
	var it ParkingItem
	if err := json.Unmarshal([]byte(sample), &it); err != nil {
		t.Fatal(err)
	}
	in, ok := it.In()
	if !ok || !in.Equal(time.Date(2025, 6, 24, 10, 16, 0, 0, time.UTC)) {
		t.Errorf("in: %v %v", in, ok)
	}
	if *it.Price != 4.5 || *it.TariffID != 2 || it.QuotaID != 1 {
		t.Errorf("unexpected: %+v", it)
	}
	if len(it.Raw) == 0 {
		t.Error("raw not kept")
	}
	if _, ok := parseTime("0001-01-01T00:00:00"); ok {
		t.Error("zero time must be treated as missing")
	}
	if _, ok := parseTime("2025-06-24T10:16:00Z"); !ok {
		t.Error("RFC3339 not parsed")
	}
}
