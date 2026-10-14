package catalogue

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // zone names must resolve on a host with no zoneinfo
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
)

type Event struct {
	ID               uuid.UUID `json:"id"`
	Slug             string    `json:"slug"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	VenueName        string    `json:"venue_name"`
	VenueAddress     string    `json:"venue_address"`
	Timezone         string    `json:"timezone"`
	StartsAt         time.Time `json:"starts_at"`
	EndsAt           time.Time `json:"ends_at"`
	Status           string    `json:"status"`
	FeeMode          string    `json:"fee_mode"`
	Currency         string    `json:"currency"`
	TaxRateBP        int       `json:"tax_rate_bp"`
	PricesIncludeTax bool      `json:"prices_include_tax"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// EventInput is a new event. FeeMode "" means pass.
type EventInput struct {
	Slug, Name, Description, VenueName, VenueAddress, Timezone, FeeMode string
	StartsAt, EndsAt                                                    time.Time
	TaxRateBP                                                           int
	PricesIncludeTax                                                    bool
}

// EventPatch changes only the fields that are set. The slug can change only
// while the event is a draft.
type EventPatch struct {
	Slug, Name, Description, VenueName, VenueAddress, Timezone, FeeMode, Status *string
	StartsAt, EndsAt                                                            *time.Time
	TaxRateBP                                                                   *int
	PricesIncludeTax                                                            *bool
}

const cols = `id, slug, name, description, venue_name, venue_address, timezone, starts_at, ends_at,
	status, fee_mode, currency, tax_rate_bp, prices_include_tax, created_at, updated_at`

func scan(row pgx.Row) (Event, error) {
	var e Event
	err := row.Scan(&e.ID, &e.Slug, &e.Name, &e.Description, &e.VenueName, &e.VenueAddress, &e.Timezone, &e.StartsAt, &e.EndsAt,
		&e.Status, &e.FeeMode, &e.Currency, &e.TaxRateBP, &e.PricesIncludeTax, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	return e, err
}

var slugRE = regexp.MustCompile(`^[a-z0-9](-?[a-z0-9])*$`)

func invalid(code, field, msg string) error { return ErrInvalid{code, field, msg} }

// validate holds the rules the CHECK constraints in 00007 back up. It trims
// the name and returns the first rule broken.
func validate(e *Event) error {
	e.Name = strings.TrimSpace(e.Name)
	switch {
	case len(e.Slug) < 3 || len(e.Slug) > 60 || !slugRE.MatchString(e.Slug):
		return invalid("invalid_slug", "slug", "Use 3 to 60 lowercase letters, numbers and single hyphens for the web address.")
	case utf8.RuneCountInString(e.Name) < 1 || utf8.RuneCountInString(e.Name) > 160:
		return invalid("invalid_name", "name", "Give the event a name of up to 160 characters.")
	case utf8.RuneCountInString(e.Description) > 20000:
		return invalid("invalid_description", "description", "The description can run to 20,000 characters. This one is longer.")
	case utf8.RuneCountInString(e.VenueName) > 160:
		return invalid("invalid_venue", "venue_name", "Keep the venue name to 160 characters and the address to 500.")
	case utf8.RuneCountInString(e.VenueAddress) > 500:
		return invalid("invalid_venue", "venue_address", "Keep the venue name to 160 characters and the address to 500.")
	case !knownZone(e.Timezone):
		return invalid("bad_timezone", "timezone", "We don't know that time zone. Use a name like Europe/London or America/Chicago.")
	case !e.EndsAt.After(e.StartsAt):
		return invalid("bad_times", "ends_at", "The event has to end after it starts.")
	case e.TaxRateBP < 0 || e.TaxRateBP > 10000:
		return invalid("invalid_tax_rate", "tax_rate_bp", "Tax is in basis points, from 0 to 10000. For 8.25%, send 825.")
	case e.FeeMode != "absorb" && e.FeeMode != "pass":
		return invalid("invalid_fee_mode", "fee_mode", "Fee mode is either pass (the buyer pays our fee) or absorb (you do).")
	}
	return nil
}

// knownZone: LoadLocation quietly accepts "" and "Local" as UTC and the
// server's zone; neither is a place.
func knownZone(name string) bool {
	if name == "" || name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// CreateEvent adds a draft event to org and records event.created by actor.
// tx must be scoped to org.
func CreateEvent(ctx context.Context, tx pgx.Tx, org uuid.UUID, actor audit.Actor, in EventInput) (Event, error) {
	e := Event{Slug: in.Slug, Name: in.Name, Description: in.Description, VenueName: in.VenueName, VenueAddress: in.VenueAddress,
		Timezone: in.Timezone, StartsAt: in.StartsAt, EndsAt: in.EndsAt, FeeMode: in.FeeMode, TaxRateBP: in.TaxRateBP, PricesIncludeTax: in.PricesIncludeTax}
	if e.FeeMode == "" {
		e.FeeMode = "pass"
	}
	if err := validate(&e); err != nil {
		return Event{}, err
	}
	e, err := scan(tx.QueryRow(ctx, `insert into events(organizer_id, slug, name, description, venue_name, venue_address, timezone,
			starts_at, ends_at, fee_mode, tax_rate_bp, prices_include_tax)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) returning `+cols,
		org, e.Slug, e.Name, e.Description, e.VenueName, e.VenueAddress, e.Timezone, e.StartsAt, e.EndsAt, e.FeeMode, e.TaxRateBP, e.PricesIncludeTax))
	if err != nil {
		return Event{}, dbError(err)
	}
	return e, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "event.created", EntityType: "event", EntityID: e.ID, After: e})
}

func GetEvent(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Event, error) {
	return scan(tx.QueryRow(ctx, `select `+cols+` from events where id = $1`, id))
}

// ListEvents returns the scoped organizer's events, soonest first.
func ListEvents(ctx context.Context, tx pgx.Tx) ([]Event, error) {
	rows, err := tx.Query(ctx, `select `+cols+` from events order by starts_at, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) { return scan(r) })
}

// UpdateEvent applies patch, records event.updated with the row before and
// after, and returns the result. Status moves between draft and live only.
//
// The row is locked FOR NO KEY UPDATE, like lockEvent: a checkout holding the
// quotas inserts rows whose foreign keys take FOR KEY SHARE on it, and the
// sales guard below then waits on those quotas, so FOR UPDATE would deadlock.
// Changing the slug (draft only) upgrades the lock when the row is written.
func UpdateEvent(ctx context.Context, tx pgx.Tx, org, id uuid.UUID, actor audit.Actor, patch EventPatch) (Event, error) {
	before, err := lockEvent(ctx, tx, id)
	if err != nil {
		return Event{}, err
	}
	e := before
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	set(&e.Slug, patch.Slug)
	set(&e.Name, patch.Name)
	set(&e.Description, patch.Description)
	set(&e.VenueName, patch.VenueName)
	set(&e.VenueAddress, patch.VenueAddress)
	set(&e.Timezone, patch.Timezone)
	set(&e.FeeMode, patch.FeeMode)
	if patch.StartsAt != nil {
		e.StartsAt = *patch.StartsAt
	}
	if patch.EndsAt != nil {
		e.EndsAt = *patch.EndsAt
	}
	if patch.TaxRateBP != nil {
		e.TaxRateBP = *patch.TaxRateBP
	}
	if patch.PricesIncludeTax != nil {
		e.PricesIncludeTax = *patch.PricesIncludeTax
	}
	if err := validate(&e); err != nil {
		return Event{}, err
	}
	if e.Slug != before.Slug && before.Status != "draft" {
		return Event{}, ErrConflict{"slug_locked", "slug", "The web address can only change while the event is a draft."}
	}
	if patch.Status != nil && *patch.Status != before.Status {
		if before.Status != "draft" && before.Status != "live" {
			return Event{}, invalid("status_not_allowed", "status", "This event has finished its run, so its status can't change.")
		}
		switch *patch.Status {
		case "draft":
			e.Status = "draft"
		case "live":
			if err := goLive(ctx, tx, e); err != nil {
				return Event{}, err
			}
			e.Status = "live"
		default:
			return Event{}, invalid("status_not_allowed", "status", "Events can move between draft and live for now. Ending or cancelling one isn't available yet.")
		}
	}
	if sameEvent(e, before) {
		return before, nil
	}
	if field := saleTermsChange(e, before); field != "" {
		if err := guardSales(ctx, tx, id, nil, field, "tickets to this event"); err != nil {
			return Event{}, err
		}
	}
	e, err = scan(tx.QueryRow(ctx, `update events set slug = $13, name = $2, description = $3, venue_name = $4, venue_address = $5, timezone = $6,
			starts_at = $7, ends_at = $8, status = $9, fee_mode = $10, tax_rate_bp = $11, prices_include_tax = $12, updated_at = clock_timestamp()
		where id = $1 returning `+cols,
		id, e.Name, e.Description, e.VenueName, e.VenueAddress, e.Timezone, e.StartsAt, e.EndsAt, e.Status, e.FeeMode, e.TaxRateBP, e.PricesIncludeTax, e.Slug))
	if err != nil {
		return Event{}, dbError(err)
	}
	return e, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "event.updated", EntityType: "event", EntityID: id, Before: before, After: e})
}

// saleTermsChange names the first field a buyer's order depends on that
// differs: tax, fee mode, the zone its times are shown in, or going back to
// draft. "" when none does.
func saleTermsChange(a, b Event) string {
	switch {
	case a.TaxRateBP != b.TaxRateBP:
		return "tax_rate_bp"
	case a.PricesIncludeTax != b.PricesIncludeTax:
		return "prices_include_tax"
	case a.FeeMode != b.FeeMode:
		return "fee_mode"
	case a.Timezone != b.Timezone:
		return "timezone"
	case a.Status == "draft" && b.Status == "live":
		return "status"
	}
	return ""
}

// sameEvent: a patch that changed nothing. Instants compare by Equal, since
// the same moment can arrive in another zone.
func sameEvent(a, b Event) bool {
	if a.StartsAt.Equal(b.StartsAt) && a.EndsAt.Equal(b.EndsAt) {
		a.StartsAt, a.EndsAt = b.StartsAt, b.EndsAt
	}
	return a == b
}

// goLive decides whether a draft may go on sale: it must not have ended yet
// (one that has started is fine), it needs at least one ticket type, and every
// ticket type needs a quota. It runs in the transaction that holds the event's
// row lock, so a ticket type or quota can't change between the check and the
// status change.
func goLive(ctx context.Context, tx pgx.Tx, e Event) error {
	if !e.EndsAt.After(time.Now()) {
		return invalid("event_over", "status", "This event has already ended, so it can't go on sale.")
	}
	var n int
	if err := tx.QueryRow(ctx, `select count(*) from ticket_types where event_id = $1`, e.ID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return invalid("no_ticket_types", "status", "Add at least one ticket type before you put the event on sale.")
	}
	var name string
	err := tx.QueryRow(ctx, `select name from ticket_types t where event_id = $1
		and not exists (select 1 from quota_ticket_types where ticket_type_id = t.id) order by position, created_at, id limit 1`, e.ID).Scan(&name)
	if err == nil {
		return invalid("unquotaed_ticket_type", "status", "Give \u201c"+name+"\u201d a capacity before going live.")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return nil
}

// DeleteEvent removes a draft event and records event.deleted. Anything past
// draft is ErrConflict event_not_draft.
func DeleteEvent(ctx context.Context, tx pgx.Tx, org, id uuid.UUID, actor audit.Actor) error {
	before, err := scan(tx.QueryRow(ctx, `select `+cols+` from events where id = $1 for update`, id))
	if err != nil {
		return err
	}
	if before.Status != "draft" {
		return ErrConflict{"event_not_draft", "", "Only a draft can be deleted. This event has already gone on sale."}
	}
	if _, err := tx.Exec(ctx, `delete from events where id = $1`, id); err != nil {
		return err
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "event.deleted", EntityType: "event", EntityID: id, Before: before})
}
