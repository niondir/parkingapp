package fairparken

import (
	"context"
	"os"
	"testing"
)

// Runs against the real API: FAIRPARKEN_LIVE=1 go test ./internal/fairparken -run Live
func TestLive(t *testing.T) {
	if os.Getenv("FAIRPARKEN_LIVE") == "" {
		t.Skip("set FAIRPARKEN_LIVE=1")
	}
	c := New()
	res, err := c.Search(context.Background(), "XX0TEST0")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("search: %d results", len(res))
	tariffs, err := c.Tariffs(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tariffs of quota 1: %+v", tariffs)
}
