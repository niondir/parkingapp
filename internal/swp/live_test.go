package swp

import (
	"context"
	"os"
	"testing"
)

// Runs against the real API: SWP_LIVE=1 go test ./internal/swp -run Live
// SWP_PLATE and SWP_CITY search a real plate (default: a fake one in Berlin).
func TestLive(t *testing.T) {
	if os.Getenv("SWP_LIVE") == "" {
		t.Skip("set SWP_LIVE=1")
	}
	c := New()
	cities, err := c.Cities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d cities", len(cities))
	city, plate := os.Getenv("SWP_CITY"), os.Getenv("SWP_PLATE")
	if city == "" {
		city = "Berlin"
	}
	if plate == "" {
		plate = "XX0TEST0"
	}
	res, err := c.Search(context.Background(), city, plate)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("search %s in %s: complete=%v hits=%+v", plate, city, res.Complete, res.Hits)
}
