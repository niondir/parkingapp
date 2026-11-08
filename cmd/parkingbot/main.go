// Command parkingbot is a Telegram bot that watches license plates for open
// parking sessions on fair parken FreeFlow car parks.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/niondir/parkingapp/internal/bot"
	"github.com/niondir/parkingapp/internal/fairparken"
	"github.com/niondir/parkingapp/internal/store"
	"github.com/niondir/parkingapp/internal/telegram"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN is not set")
	}
	cfg := bot.Config{
		PollInterval: envDuration("POLL_INTERVAL", 3*time.Minute),
		RequestGap:   envDuration("REQUEST_GAP", time.Second),
		RemindBefore: envDuration("REMIND_BEFORE", 12*time.Hour),
		MaxPlates:    envInt("MAX_PLATES", 10),
	}
	if cfg.PollInterval < 30*time.Second {
		return fmt.Errorf("POLL_INTERVAL must be at least 30s")
	}
	var err error
	if cfg.AllowedUsers, err = envIDs("ALLOWED_USER_IDS"); err != nil {
		return err
	}
	if cfg.Admins, err = envIDs("ADMIN_USER_IDS"); err != nil {
		return err
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "parkingapp.db"
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("starting", "poll", cfg.PollInterval, "admins", len(cfg.Admins), "allowed_users", len(cfg.AllowedUsers), "db", dbPath)
	return bot.New(cfg, telegram.New(token), fairparken.New(), st).Run(ctx)
}

func envIDs(key string) (map[int64]bool, error) {
	ids := map[int64]bool{}
	for _, s := range strings.FieldsFunc(os.Getenv(key), func(r rune) bool { return r == ',' || r == ' ' }) {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		if id != 0 {
			ids[id] = true
		}
	}
	return ids, nil
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
		slog.Warn("invalid duration, using default", "key", key, "value", v)
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
		slog.Warn("invalid number, using default", "key", key, "value", v)
	}
	return def
}
