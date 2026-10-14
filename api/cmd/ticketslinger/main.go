// Command ticketslinger runs the TicketSlinger API and its maintenance tasks.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/config"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/httpapi"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "serve":
		if err := serve(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "migrate":
		cfg, err := config.Load()
		if err == nil && cfg.MigrateDatabaseURL == "" {
			err = errors.New("TS_MIGRATE_DATABASE_URL is required for migrate")
		}
		if err == nil {
			err = db.Migrate(context.Background(), cfg.MigrateDatabaseURL)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ticketslinger <serve|migrate|version>")
	os.Exit(2)
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.SecretKey == nil {
		return errors.New("TS_SECRET_KEY is required (base64 of 32 random bytes, e.g. `openssl rand -base64 32`)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.New(httpapi.Deps{Pool: pool, Auth: auth.NewService(pool, mailer.Log{}, auth.Options{RPID: cfg.WebAuthnRPID, Origin: cfg.DashboardOrigin, SecretKey: cfg.SecretKey}), Config: cfg}),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
