package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

type Config struct {
	ListenAddr       string
	DBPath           string
	APIToken         string
	Redeemer         string // "amazon" or "fake"
	CookiesPath      string
	ChromeProfileDir string
	ChromePath       string
	ChromeNoSandbox  bool
	RedeemDelay      time.Duration
	PauseDuration    time.Duration
	MaxFailures      int
}

func loadConfig() Config {
	return Config{
		ListenAddr:       env("LISTEN_ADDR", ":8080"),
		DBPath:           env("DB_PATH", "data/donobox.db"),
		APIToken:         os.Getenv("API_TOKEN"),
		Redeemer:         env("REDEEMER", "amazon"),
		CookiesPath:      env("AMAZON_COOKIES", "cookies.json"),
		ChromeProfileDir: env("CHROME_PROFILE_DIR", "data/chrome"),
		ChromePath:       os.Getenv("CHROME_PATH"),
		ChromeNoSandbox:  os.Getenv("CHROME_NO_SANDBOX") == "1",
		RedeemDelay:      envDuration("REDEEM_DELAY", 30*time.Second),
		PauseDuration:    envDuration("PAUSE_DURATION", 30*time.Minute),
		MaxFailures:      envInt("MAX_CONSECUTIVE_FAILURES", 3),
	}
}

func main() {
	cfg := loadConfig()
	if cfg.APIToken == "" {
		log.Print("warning: API_TOKEN is not set, the API is unauthenticated")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer store.Close()
	if n, err := store.RecoverInterrupted(ctx); err != nil {
		log.Fatalf("recover interrupted cards: %v", err)
	} else if n > 0 {
		log.Printf("%d card(s) were interrupted mid-redemption and need review", n)
	}

	var redeemer Redeemer
	switch cfg.Redeemer {
	case "fake":
		log.Print("using fake redeemer, nothing is sent to Amazon")
		redeemer = FakeRedeemer{}
	case "amazon":
		ar, err := NewAmazonRedeemer(cfg)
		if err != nil {
			log.Fatalf("start amazon redeemer: %v", err)
		}
		defer ar.Close()
		redeemer = ar
	default:
		log.Fatalf("unknown REDEEMER %q (want amazon or fake)", cfg.Redeemer)
	}

	worker := NewWorker(store, redeemer, cfg.RedeemDelay, cfg.PauseDuration, cfg.MaxFailures)
	go worker.Run(ctx)

	api := &API{store: store, worker: worker, token: cfg.APIToken}
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: api.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("listening on %s", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server: %v", err)
	}

	log.Print("waiting for the current redemption to finish")
	<-worker.Done()
	log.Print("shut down")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("%s: %v", key, err)
		}
		return d
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			log.Fatalf("%s: %v", key, err)
		}
		return n
	}
	return def
}
