// Package legalhold records decisions to preserve audit evidence from the
// retention sweep (#373).
//
// # What a hold is
//
// A row in public.legal_holds naming a date range. While it is ACTIVE, the
// audit retention sweep will not delete any audit entry whose created_at falls
// inside that range -- the exemption is rendered by the shared module's
// store.WithLegalHolds and applied inside the sweep's batch selection, so held
// rows are never selected in the first place.
//
// # Why the table name is schema-qualified everywhere in this package
//
// The sweep runs on the IDENTITY connection pool, whose DSN carries
// search_path=identity,public. An unqualified "legal_holds" in the exemption
// would therefore resolve to identity.legal_holds if such a table ever existed,
// and to public.legal_holds otherwise.
//
// That is the failure this feature exists to prevent, wearing a different hat:
// this package would write one table while the sweep read another, every hold
// would look placed, and the sweep would delete the rows anyway and report
// success. Qualifying the name makes it unreachable rather than unlikely.
//
// # Release is not deletion
//
// Releasing a hold sets active=false and stamps released_by/released_at. The
// row stays, because the hold is the RECORD of a decision and deleting it would
// erase the fact that someone made one. The shared module reads `active` alone,
// so a released hold stops protecting immediately -- which is intended: the
// evidence was preserved for as long as the hold stood.
package legalhold

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Table is the name migration 000035 creates, and the name passed to
// store.WithLegalHolds. Schema-qualified -- see the package comment.
const Table = "public.legal_holds"

// ErrNotFound is returned when no hold has the given id.
var ErrNotFound = errors.New("legalhold: no such hold")

// ErrInvalidRange is returned when end_date precedes start_date. The database
// also refuses it via legal_holds_range; checked here so the caller gets a 400
// naming the problem rather than a constraint violation.
var ErrInvalidRange = errors.New("legalhold: end_date is before start_date")

// Hold is one recorded preservation decision.
type Hold struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Reason     string     `json:"reason"`
	StartDate  time.Time  `json:"start_date"`
	EndDate    time.Time  `json:"end_date"`
	Active     bool       `json:"active"`
	PlacedBy   *string    `json:"placed_by,omitempty"`
	PlacedAt   time.Time  `json:"placed_at"`
	ReleasedBy *string    `json:"released_by,omitempty"`
	ReleasedAt *time.Time `json:"released_at,omitempty"`
}

// Repository reads and writes holds.
//
// It takes the connection the SWEEP uses, not the application's own. A hold
// written on a different pool than the sweep reads is invisible to it, which is
// the whole hazard -- so the type makes the two the same by construction rather
// than by convention.
type Repository struct{ db *sql.DB }

// New builds a Repository over the connection the audit sweep runs on.
func New(sweepDB *sql.DB) (*Repository, error) {
	if sweepDB == nil {
		return nil, errors.New("legalhold: no database connection")
	}
	return &Repository{db: sweepDB}, nil
}

const columns = `id, name, reason, start_date, end_date, active, placed_by, placed_at, released_by, released_at`

// Place records a new hold.
func (r *Repository) Place(ctx context.Context, name, reason string, start, end time.Time, placedBy string) (*Hold, error) {
	if end.Before(start) {
		return nil, fmt.Errorf("%w: %s .. %s", ErrInvalidRange, start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	h := &Hold{
		ID: uuid.NewString(), Name: name, Reason: reason,
		StartDate: start, EndDate: end, Active: true, PlacedAt: time.Now().UTC(),
	}
	var by any
	if placedBy != "" {
		by = placedBy
		h.PlacedBy = &placedBy
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO "public"."legal_holds" (id, name, reason, start_date, end_date, active, placed_by, placed_at)
		 VALUES ($1, $2, $3, $4, $5, TRUE, $6, $7)`,
		h.ID, h.Name, h.Reason, h.StartDate, h.EndDate, by, h.PlacedAt)
	if err != nil {
		return nil, fmt.Errorf("legalhold: place: %w", err)
	}
	return h, nil
}

// Release deactivates a hold. The row remains.
//
// Idempotent by design: releasing an already-released hold reports success. A
// second release is not an error worth failing a request over, and treating it
// as one would make a retried call look like a failure.
func (r *Repository) Release(ctx context.Context, id, releasedBy string) (*Hold, error) {
	var by any
	if releasedBy != "" {
		by = releasedBy
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE "public"."legal_holds"
		 SET active = FALSE, released_by = COALESCE($2, released_by), released_at = COALESCE(released_at, now())
		 WHERE id = $1`, id, by)
	if err != nil {
		return nil, fmt.Errorf("legalhold: release: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return r.Get(ctx, id)
}

// Get returns one hold.
func (r *Repository) Get(ctx context.Context, id string) (*Hold, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+columns+` FROM "public"."legal_holds" WHERE id = $1`, id)
	h, err := scanHold(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return h, err
}

// List returns holds newest-first.
func (r *Repository) List(ctx context.Context) ([]Hold, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+columns+` FROM "public"."legal_holds" ORDER BY placed_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("legalhold: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Hold{}
	for rows.Next() {
		h, scanErr := scanHold(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, *h)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanHold(s scanner) (*Hold, error) {
	var h Hold
	var placedBy, releasedBy sql.NullString
	var releasedAt sql.NullTime
	if err := s.Scan(&h.ID, &h.Name, &h.Reason, &h.StartDate, &h.EndDate, &h.Active,
		&placedBy, &h.PlacedAt, &releasedBy, &releasedAt); err != nil {
		return nil, err
	}
	if placedBy.Valid {
		h.PlacedBy = &placedBy.String
	}
	if releasedBy.Valid {
		h.ReleasedBy = &releasedBy.String
	}
	if releasedAt.Valid {
		t := releasedAt.Time
		h.ReleasedAt = &t
	}
	return &h, nil
}
