package checkin_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/checkin"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

func TestScanAcceptsThenDuplicates(t *testing.T) {
	f := seed(t)
	tk := f.order(t, f.org, f.event, f.general, "TS-7K2QF", "buyer@example.com", "Ada Lovelace", "Bob")

	first := f.checkIn(t, f.event, checkin.Input{QR: tk[0].qr, Gate: "North"})
	if first.Result != "accepted" || first.Ticket == nil || first.Ticket.AttendeeName != "Ada Lovelace" ||
		first.Ticket.TicketTypeName != "General" || first.Ticket.OrderCode != "TS-7K2QF" || first.Ticket.CheckedInAt == nil ||
		first.CheckedIn != 1 || first.Sold != 2 {
		t.Fatalf("first scan: %+v %+v", first, first.Ticket)
	}
	second := f.checkIn(t, f.event, checkin.Input{QR: tk[0].qr})
	if second.Result != "duplicate" || second.Ticket == nil || second.Ticket.CheckedInAt == nil ||
		!second.Ticket.CheckedInAt.Equal(*first.Ticket.CheckedInAt) || second.CheckedIn != 1 || second.Sold != 2 {
		t.Fatalf("second scan: %+v %+v", second, second.Ticket)
	}

	hash := sha256.Sum256([]byte(tk[0].qr))
	if n := f.count(t, `select count(*) from checkins where ticket_id = $1 and value_hash = $2 and method = 'scan'
		and actor_user_id = $3 and organizer_id = $4 and event_id = $5`, tk[0].id, hash[:], f.user, f.org, f.event); n != 2 {
		t.Fatalf("checkins rows with the value's hash: %d, want 2", n)
	}
	if n := f.count(t, `select count(*) from checkins where result = 'accepted' and gate = 'North'`); n != 1 {
		t.Fatalf("accepted rows at North: %d, want 1", n)
	}
	for _, table := range []string{"checkins", "audit_log"} {
		if n := f.count(t, `select count(*) from `+table+` c where c::text like '%' || $1 || '%'`, tk[0].qr); n != 0 {
			t.Fatalf("%s rows carrying the scanned text: %d", table, n)
		}
	}
}

// Two doors scan one ticket at the same instant: exactly one lets it in.
func TestConcurrentScansAcceptOnce(t *testing.T) {
	f := seed(t)
	names := make([]string, 50)
	for i := range names {
		names[i] = fmt.Sprintf("Guest %02d", i)
	}
	tickets := f.order(t, f.org, f.event, f.general, "TS-RACE0", "race@example.com", names...)
	ctx := context.Background()
	for round, tk := range tickets {
		var ready sync.WaitGroup
		start := make(chan struct{})
		results := make([]checkin.Result, 2)
		errs := make([]error, 2)
		var done sync.WaitGroup
		for i := range 2 {
			ready.Add(1)
			done.Add(1)
			go func() {
				defer done.Done()
				// Each side holds its own connection and open transaction
				// before the barrier drops, so the two UPDATEs really race.
				errs[i] = db.WithOrganizer(ctx, f.app, f.user, f.org, func(tx pgx.Tx, _ string) (err error) {
					ready.Done()
					<-start
					results[i], err = checkin.CheckIn(ctx, tx, f.org, f.event, checkin.Input{QR: tk.qr, Actor: actorOf(f)})
					return err
				})
			}()
		}
		ready.Wait()
		close(start)
		done.Wait()
		for _, err := range errs {
			f.must(t, err)
		}
		got := []string{results[0].Result, results[1].Result}
		slices.Sort(got)
		if !slices.Equal(got, []string{"accepted", "duplicate"}) {
			t.Fatalf("round %d: %v, want one accepted and one duplicate", round, got)
		}
		winner := results[0]
		if winner.Result != "accepted" {
			winner = results[1]
		}
		var at time.Time
		f.must(t, f.owner.QueryRow(ctx, `select checked_in_at from tickets where id = $1`, tk.id).Scan(&at))
		for _, r := range results {
			if r.Ticket == nil || r.Ticket.CheckedInAt == nil || !r.Ticket.CheckedInAt.Equal(at) || !winner.Ticket.CheckedInAt.Equal(at) {
				t.Fatalf("round %d: checked_in_at %v, results %+v %+v", round, at, results[0].Ticket, results[1].Ticket)
			}
		}
	}
	if n := f.count(t, `select count(*) from checkins where result = 'accepted'`); n != 50 {
		t.Fatalf("accepted rows: %d, want 50", n)
	}
}

func TestScanOfAnythingElseIsInvalid(t *testing.T) {
	f := seed(t)
	f.order(t, f.org, f.event, f.general, "TS-AAAAA", "a@example.com", "Ada")
	otherEvent, otherGeneral := f.liveEvent(t, f.org, "matinee")
	sameOrg := f.order(t, f.org, otherEvent, otherGeneral, "TS-BBBBB", "b@example.com", "Bea")
	org2 := f.organizer(t, "rival")
	rivalEvent, rivalGeneral := f.liveEvent(t, org2, "gala")
	rival := f.order(t, org2, rivalEvent, rivalGeneral, "TS-CCCCC", "c@example.com", "Cy")

	for name, qr := range map[string]string{
		"a link":                        "https://example.com",
		"not base64url":                 "!!!!!!!!!!!!!!!!!!!!!!",
		"no ticket has it":              "AAAAAAAAAAAAAAAAAAAAAA",
		"another event, same organizer": sameOrg[0].qr,
		"another organizer":             rival[0].qr,
	} {
		r := f.checkIn(t, f.event, checkin.Input{QR: qr})
		if r.Result != "invalid" || r.Ticket != nil || r.Sold != 1 {
			t.Errorf("%s: %+v, want invalid with no ticket", name, r)
		}
	}
	// The other event's ticket still gets in at its own event.
	if r := f.checkIn(t, otherEvent, checkin.Input{QR: sameOrg[0].qr}); r.Result != "accepted" {
		t.Errorf("same ticket at its own event: %+v", r)
	}
}

func TestVoidedTicketScansVoid(t *testing.T) {
	f := seed(t)
	tk := f.order(t, f.org, f.event, f.general, "TS-VVVVV", "v@example.com", "Ada", "Bob")
	if r := f.checkIn(t, f.event, checkin.Input{QR: tk[0].qr}); r.Result != "accepted" {
		t.Fatalf("first scan: %+v", r)
	}
	f.must(t, system.Run(context.Background(), f.app, f.org, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `update tickets set status = 'void' where id = $1`, tk[0].id)
		return err
	}))
	r := f.checkIn(t, f.event, checkin.Input{QR: tk[0].qr})
	if r.Result != "void" || r.Ticket == nil || r.Ticket.Status != "void" || r.CheckedIn != 0 || r.Sold != 1 {
		t.Fatalf("voided ticket: %+v %+v, want void, 0 of 1", r, r.Ticket)
	}
}

func TestManualCheckIn(t *testing.T) {
	f := seed(t)
	tk := f.order(t, f.org, f.event, f.general, "TS-MMMMM", "m@example.com", "Ada")
	if r := f.checkIn(t, f.event, checkin.Input{TicketID: tk[0].id}); r.Result != "accepted" || r.Ticket == nil || r.Ticket.ID != tk[0].id {
		t.Fatalf("manual: %+v", r)
	}
	if r := f.checkIn(t, f.event, checkin.Input{TicketID: tk[0].id}); r.Result != "duplicate" {
		t.Fatalf("manual again: %+v", r)
	}
	if r := f.checkIn(t, f.event, checkin.Input{TicketID: uuid.New()}); r.Result != "invalid" || r.Ticket != nil {
		t.Fatalf("unknown id: %+v", r)
	}
	if n := f.count(t, `select count(*) from checkins where ticket_id = $1 and method = 'manual' and value_hash is null`, tk[0].id); n != 2 {
		t.Fatalf("manual rows: %d, want 2", n)
	}
}

// ts_app moves checked_in_at one way: from nothing to a time, on a valid ticket.
func TestAppCannotUndoACheckIn(t *testing.T) {
	f := seed(t)
	tk := f.order(t, f.org, f.event, f.general, "TS-WWWWW", "u@example.com", "Ada", "Bob")
	f.checkIn(t, f.event, checkin.Input{TicketID: tk[0].id})
	f.exec(t, `update tickets set status = 'void' where id = $1`, tk[1].id)
	ctx := context.Background()
	for name, c := range map[string]struct {
		sql string
		id  uuid.UUID
	}{
		"undo":              {`update tickets set checked_in_at = null where id = $1`, tk[0].id},
		"move the time":     {`update tickets set checked_in_at = now() where id = $1`, tk[0].id},
		"check in void row": {`update tickets set checked_in_at = now() where id = $1`, tk[1].id},
	} {
		err := db.WithOrganizer(ctx, f.app, f.user, f.org, func(tx pgx.Tx, _ string) error {
			_, err := tx.Exec(ctx, c.sql, c.id)
			return err
		})
		if pgCode(err) != "42501" {
			t.Errorf("%s: %v, want insufficient_privilege", name, err)
		}
	}
}

func TestSearchFindsByNameEmailAndCode(t *testing.T) {
	f := seed(t)
	ada := f.order(t, f.org, f.event, f.general, "TS-7K2QF", "buyer@example.com", "Ada Lovelace", "Bob")
	f.order(t, f.org, f.event, f.general, "TS-HHHHH", "other@example.com", "Hal")
	zeds := make([]string, 25)
	for i := range zeds {
		zeds[i] = fmt.Sprintf("Zed %02d", i)
	}
	f.order(t, f.org, f.event, f.general, "TS-ZZZZZ", "zed@example.com", zeds...)
	ctx := context.Background()
	search := func(q string) (ids []uuid.UUID) {
		t.Helper()
		f.must(t, db.WithOrganizer(ctx, f.app, f.user, f.org, func(tx pgx.Tx, _ string) error {
			got, err := checkin.Search(ctx, tx, f.event, q)
			for _, tk := range got {
				ids = append(ids, tk.ID)
			}
			return err
		}))
		return ids
	}
	both := []uuid.UUID{ada[0].id, ada[1].id}
	for q, want := range map[string][]uuid.UUID{
		"ada":      {ada[0].id},
		"buyer@ex": both,
		"7k2qf":    both,
		"TS-7K2QF": both,
		"%":        nil,
		"_":        nil,
	} {
		if got := search(q); !slices.Equal(got, want) {
			t.Errorf("search %q: %v, want %v", q, got, want)
		}
	}
	if got := search("zed"); len(got) != 20 {
		t.Errorf("search zed: %d results, want 20", len(got))
	}
}
