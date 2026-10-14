package catalogue

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
)

// Question is something the buyer is asked at checkout. AppliesToAll means
// every ticket type; otherwise only those in AppliesTo (empty then means none,
// e.g. after they were deleted, so the question is never asked).
type Question struct {
	ID           uuid.UUID   `json:"id"`
	EventID      uuid.UUID   `json:"event_id"`
	Label        string      `json:"label"`
	Kind         string      `json:"kind"` // text | select | checkbox
	Options      []string    `json:"options"`
	Required     bool        `json:"required"`
	Scope        string      `json:"scope"` // order | ticket
	AppliesToAll bool        `json:"applies_to_all"`
	AppliesTo    []uuid.UUID `json:"applies_to"`
	Position     int         `json:"position"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
}

type QuestionInput struct {
	Label, Kind, Scope string
	Options            []string
	Required           bool
	AppliesToAll       *bool // nil means true
	AppliesTo          []uuid.UUID
}

// QuestionPatch changes only what is set. A non-nil Options or AppliesTo
// (even empty) replaces the current list.
type QuestionPatch struct {
	Label, Kind, Scope *string
	Options            *[]string
	Required           *bool
	AppliesToAll       *bool
	AppliesTo          *[]uuid.UUID
	Position           *int
}

const questionCols = `id, event_id, label, kind, options, required, scope, applies_to_all,
	coalesce((select array_agg(ticket_type_id order by ticket_type_id) from question_applies_to where question_id = questions.id), '{}'),
	position, created_at, updated_at`

func scanQuestion(row pgx.Row) (Question, error) {
	var q Question
	err := row.Scan(&q.ID, &q.EventID, &q.Label, &q.Kind, &q.Options, &q.Required, &q.Scope, &q.AppliesToAll, &q.AppliesTo, &q.Position, &q.CreatedAt, &q.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Question{}, ErrNotFound
	}
	if q.Options == nil {
		q.Options = []string{}
	}
	return q, err
}

// validateQuestion holds the rules the CHECK constraints in 00011 back up. It
// trims the label and the options, and returns the first rule broken.
func validateQuestion(q *Question) error {
	q.Label = strings.TrimSpace(q.Label)
	switch {
	case utf8.RuneCountInString(q.Label) < 1 || utf8.RuneCountInString(q.Label) > 200:
		return invalid("bad_label", "Give the question a label of up to 200 characters.")
	case q.Kind != "text" && q.Kind != "select" && q.Kind != "checkbox":
		return invalid("bad_kind", "A question is a text box, a select (pick one) or a checkbox.")
	case q.Scope != "order" && q.Scope != "ticket":
		return invalid("bad_scope", "Ask once per order, or once per ticket.")
	case q.Position < 1:
		return invalid("bad_position", "Positions start at 1.")
	}
	if q.Kind != "select" {
		if len(q.Options) > 0 {
			return invalid("bad_options", "Only a select question has options. Leave them out for text and checkbox questions.")
		}
		return nil
	}
	if len(q.Options) < 2 || len(q.Options) > 30 {
		return invalid("bad_options", "A select needs 2 to 30 options.")
	}
	seen := map[string]bool{}
	for i, o := range q.Options {
		o = strings.TrimSpace(o)
		q.Options[i] = o
		switch {
		case utf8.RuneCountInString(o) < 1 || utf8.RuneCountInString(o) > 120:
			return invalid("bad_options", "Each option needs 1 to 120 characters.")
		case seen[fold(o)]:
			return invalid("bad_options", "“"+o+"” is listed twice. Options have to be different, ignoring capitals.")
		}
		seen[fold(o)] = true
	}
	return nil
}

// fold maps s and ſ, or σ, ς and Σ, to one key: upper then lower case.
func fold(s string) string { return strings.ToLower(strings.ToUpper(s)) }

// questionDBError is dbError for questions: 23514 a failed CHECK.
func questionDBError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23514" {
		return ErrInvalid{"invalid_question", "Something in that question isn't allowed. Check the label, the kind and the options."}
	}
	return err
}

// setQuestionLinks replaces the question's ticket types; the ids are checked.
func setQuestionLinks(ctx context.Context, tx pgx.Tx, org, event, id uuid.UUID, ids []uuid.UUID) error {
	if _, err := tx.Exec(ctx, `delete from question_applies_to where question_id = $1`, id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `insert into question_applies_to(organizer_id, event_id, question_id, ticket_type_id)
		select $1, $2, $3, t from unnest($4::uuid[]) t`, org, event, id, ids)
	return questionDBError(err)
}

// CreateQuestion adds a question at the end of the event's list and records
// question.created by actor. tx must be scoped to org.
func CreateQuestion(ctx context.Context, tx pgx.Tx, org, event uuid.UUID, actor audit.Actor, in QuestionInput) (Question, error) {
	q := Question{Label: in.Label, Kind: in.Kind, Scope: in.Scope, Required: in.Required, Position: 1,
		Options: slices.Clone(in.Options), AppliesToAll: in.AppliesToAll == nil || *in.AppliesToAll}
	if err := validateQuestion(&q); err != nil {
		return Question{}, err
	}
	if err := validateAppliesTo(q.AppliesToAll, dedupe(in.AppliesTo)); err != nil {
		return Question{}, err
	}
	if _, err := lockEvent(ctx, tx, event); err != nil {
		return Question{}, err
	}
	applies, err := linkSet(ctx, tx, event, in.AppliesTo)
	if err != nil {
		return Question{}, err
	}
	if q.Options == nil {
		q.Options = []string{}
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `insert into questions(organizer_id, event_id, label, kind, options, required, scope, applies_to_all, position)
		values ($1, $2, $3, $4, $5, $6, $7, $8, (select coalesce(max(position), 0) + 1 from questions where event_id = $2)) returning id`,
		org, event, q.Label, q.Kind, q.Options, q.Required, q.Scope, q.AppliesToAll).Scan(&id); err != nil {
		return Question{}, questionDBError(err)
	}
	if err := setQuestionLinks(ctx, tx, org, event, id, applies); err != nil {
		return Question{}, err
	}
	if q, err = GetQuestion(ctx, tx, event, id); err != nil {
		return Question{}, err
	}
	return q, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "question.created", EntityType: "question", EntityID: q.ID, After: q})
}

func GetQuestion(ctx context.Context, tx pgx.Tx, event, id uuid.UUID) (Question, error) {
	return scanQuestion(tx.QueryRow(ctx, `select `+questionCols+` from questions where id = $1 and event_id = $2`, id, event))
}

// ListQuestions returns the event's questions in position order.
func ListQuestions(ctx context.Context, tx pgx.Tx, event uuid.UUID) ([]Question, error) {
	if _, err := GetEvent(ctx, tx, event); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `select `+questionCols+` from questions where event_id = $1 order by position, created_at, id`, event)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Question, error) { return scanQuestion(r) })
}

// UpdateQuestion applies patch under the event lock and records
// question.updated. A patch that changes nothing writes nothing.
func UpdateQuestion(ctx context.Context, tx pgx.Tx, org, event, id uuid.UUID, actor audit.Actor, patch QuestionPatch) (Question, error) {
	if _, err := lockEvent(ctx, tx, event); err != nil {
		return Question{}, err
	}
	before, err := scanQuestion(tx.QueryRow(ctx, `select `+questionCols+` from questions where id = $1 and event_id = $2 for update`, id, event))
	if err != nil {
		return Question{}, err
	}
	q := before
	q.Options = slices.Clone(before.Options)
	if patch.Label != nil {
		q.Label = *patch.Label
	}
	if patch.Scope != nil {
		q.Scope = *patch.Scope
	}
	if patch.Required != nil {
		q.Required = *patch.Required
	}
	if patch.Position != nil {
		q.Position = *patch.Position
	}
	if patch.Kind != nil && *patch.Kind != before.Kind {
		q.Kind = *patch.Kind
		if q.Kind == "select" && patch.Options == nil {
			return Question{}, invalid("options_required_with_kind", "Send the options too when you turn a question into a select.")
		}
		if before.Kind == "select" {
			q.Options = []string{}
		}
	}
	if patch.Options != nil {
		q.Options = slices.Clone(*patch.Options)
	}
	if patch.AppliesToAll != nil {
		q.AppliesToAll = *patch.AppliesToAll
	}
	if err := validateQuestion(&q); err != nil {
		return Question{}, err
	}
	if patch.AppliesTo != nil {
		if q.AppliesTo, err = linkSet(ctx, tx, event, *patch.AppliesTo); err != nil {
			return Question{}, err
		}
	}
	// Only when the patch touches the scope: a question emptied by a deleted
	// ticket type can still have its other fields edited.
	if patch.AppliesTo != nil || patch.AppliesToAll != nil {
		if err := validateAppliesTo(q.AppliesToAll, q.AppliesTo); err != nil {
			return Question{}, err
		}
	}
	if q.Label == before.Label && q.Kind == before.Kind && q.Scope == before.Scope && q.Required == before.Required && q.Position == before.Position &&
		q.AppliesToAll == before.AppliesToAll && slices.Equal(q.Options, before.Options) && slices.Equal(q.AppliesTo, before.AppliesTo) {
		return before, nil
	}
	if _, err := tx.Exec(ctx, `update questions set label = $2, kind = $3, options = $4, required = $5, scope = $6, applies_to_all = $7,
		position = $8, updated_at = clock_timestamp() where id = $1`, id, q.Label, q.Kind, q.Options, q.Required, q.Scope, q.AppliesToAll, q.Position); err != nil {
		return Question{}, questionDBError(err)
	}
	if err := setQuestionLinks(ctx, tx, org, event, id, q.AppliesTo); err != nil {
		return Question{}, err
	}
	if q, err = GetQuestion(ctx, tx, event, id); err != nil {
		return Question{}, err
	}
	return q, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "question.updated", EntityType: "question", EntityID: id, Before: before, After: q})
}

// DeleteQuestion removes a question and records question.deleted.
func DeleteQuestion(ctx context.Context, tx pgx.Tx, org, event, id uuid.UUID, actor audit.Actor) error {
	if _, err := lockEvent(ctx, tx, event); err != nil {
		return err
	}
	before, err := scanQuestion(tx.QueryRow(ctx, `select `+questionCols+` from questions where id = $1 and event_id = $2 for update`, id, event))
	if err != nil {
		return err
	}
	// ponytail: P3 must guard this once orders hold answers to it.
	if _, err := tx.Exec(ctx, `delete from questions where id = $1`, id); err != nil {
		return err
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "question.deleted", EntityType: "question", EntityID: id, Before: before})
}

// ReorderQuestions is ReorderTicketTypes for the event's questions, audited
// as question.reordered.
func ReorderQuestions(ctx context.Context, tx pgx.Tx, org, event uuid.UUID, actor audit.Actor, ids []uuid.UUID) ([]Question, error) {
	if err := reorder(ctx, tx, org, event, actor, "questions", "question.reordered", ids); err != nil {
		return nil, err
	}
	return ListQuestions(ctx, tx, event)
}
