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

// EventPatch changes only the fields that are set. The slug is part of the
// event's address and stays put.
type EventPatch struct {
	Name, Description, VenueName, VenueAddress, Timezone, FeeMode, Status *string
	StartsAt, EndsAt                                                      *time.Time
	TaxRateBP                                                             *int
	PricesIncludeTax                                                      *bool
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

func invalid(code, msg string) error { return ErrInvalid{code, msg} }

// validate holds the rules the CHECK constraints in 00007 back up. It trims
// the name and returns the first rule broken.
func validate(e *Event) error {
	e.Name = strings.TrimSpace(e.Name)
	switch {
	case len(e.Slug) < 3 || len(e.Slug) > 60 || !slugRE.MatchString(e.Slug):
		return invalid("invalid_slug", "Use 3 to 60 lowercase letters, numbers and single hyphens for the web address.")
	case utf8.RuneCountInString(e.Name) < 1 || utf8.RuneCountInString(e.Name) > 160:
		return invalid("invalid_name", "Give the event a name of up to 160 characters.")
	case utf8.RuneCountInString(e.Description) > 20000:
		return invalid("invalid_description", "The description can run to 20,000 characters. This one is longer.")
	case utf8.RuneCountInString(e.VenueName) > 160 || utf8.RuneCountInString(e.VenueAddress) > 500:
		return invalid("invalid_venue", "Keep the venue name to 160 characters and the address to 500.")
	case !knownZone(e.Timezone):
		return invalid("bad_timezone", "We don't know that time zone. Use a name like Europe/London or America/Chicago.")
	case !e.EndsAt.After(e.StartsAt):
		return invalid("bad_times", "The event has to end after it starts.")
	case e.TaxRateBP < 0 || e.TaxRateBP > 10000:
		return invalid("invalid_tax_rate", "Tax is in basis points, from 0 to 10000. For 8.25%, send 825.")
	case e.FeeMode != "absorb" && e.FeeMode != "pass":
		return invalid("invalid_fee_mode", "Fee mode is either pass (the buyer pays our fee) or absorb (you do).")
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
func UpdateEvent(ctx context.Context, tx pgx.Tx, org, id uuid.UUID, actor audit.Actor, patch EventPatch) (Event, error) {
	before, err := scan(tx.QueryRow(ctx, `select `+cols+` from events where id = $1 for update`, id))
	if err != nil {
		return Event{}, err
	}
	e := before
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
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
	if patch.Status != nil && *patch.Status != before.Status {
		switch *patch.Status {
		case "draft":
			e.Status = "draft"
		case "live":
			if err := goLive(); err != nil {
				return Event{}, err
			}
			e.Status = "live"
		default:
			return Event{}, invalid("status_not_allowed", "Events can move between draft and live for now. Ending or cancelling one isn't available yet.")
		}
	}
	e, err = scan(tx.QueryRow(ctx, `update events set name = $2, description = $3, venue_name = $4, venue_address = $5, timezone = $6,
			starts_at = $7, ends_at = $8, status = $9, fee_mode = $10, tax_rate_bp = $11, prices_include_tax = $12, updated_at = clock_timestamp()
		where id = $1 returning `+cols,
		id, e.Name, e.Description, e.VenueName, e.VenueAddress, e.Timezone, e.StartsAt, e.EndsAt, e.Status, e.FeeMode, e.TaxRateBP, e.PricesIncludeTax))
	if err != nil {
		return Event{}, dbError(err)
	}
	return e, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "event.updated", EntityType: "event", EntityID: id, Before: before, After: e})
}

// goLive decides whether a draft may go on sale. The rule is that it has at
// least one ticket type, and ticket types arrive with the next task, which
// replaces this body and passes the event. Until then nothing can go live.
func goLive() error {
	return invalid("no_ticket_types", "Add at least one ticket type before you put the event on sale.")
}

// DeleteEvent removes a draft event and records event.deleted. Anything past
// draft is ErrConflict event_not_draft.
func DeleteEvent(ctx context.Context, tx pgx.Tx, org, id uuid.UUID, actor audit.Actor) error {
	before, err := scan(tx.QueryRow(ctx, `select `+cols+` from events where id = $1 for update`, id))
	if err != nil {
		return err
	}
	if before.Status != "draft" {
		return ErrConflict{"event_not_draft", "Only a draft can be deleted. This event has already gone on sale."}
	}
	if _, err := tx.Exec(ctx, `delete from events where id = $1`, id); err != nil {
		return err
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "event.deleted", EntityType: "event", EntityID: id, Before: before})
}
