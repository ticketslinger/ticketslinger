// Command ticketslinger runs the TicketSlinger API and its maintenance tasks.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/ticketslinger/ticketslinger/api/internal/config"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
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
