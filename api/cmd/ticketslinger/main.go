// Command ticketslinger runs the TicketSlinger API and its maintenance tasks.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: ticketslinger <serve|migrate|version>")
	os.Exit(2)
}
