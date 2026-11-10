package bank

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const (
	// maxHolderDistance is how many edits apart two names or two ID numbers can
	// be and still count as similar.
	maxHolderDistance = 3

	// maxHolderFieldLength is the longest text PostgreSQL's levenshtein accepts.
	maxHolderFieldLength = 255
)

var earliestDateOfBirth = time.Date(1900, time.January, 1, 0, 0, 0, 0, time.UTC)

// Holder is the person an account belongs to.
type Holder struct {
	Name        string
	Address     string
	DateOfBirth time.Time
	IDNumber    string
}

// normalized trims the text fields and puts IDNumber in canonical form, so
// that variants such as "ab-123" and "AB 123" count as the same document.
func (h Holder) normalized() Holder {
	h.Name = strings.TrimSpace(h.Name)
	h.Address = strings.TrimSpace(h.Address)
	h.IDNumber = strings.ToUpper(strings.Map(func(r rune) rune {
		if r == '-' || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, h.IDNumber))
	return h
}

func (h Holder) validate(now time.Time) error {
	fields := []struct{ label, value string }{
		{"name", h.Name},
		{"address", h.Address},
		{"ID number", h.IDNumber},
	}
	for _, field := range fields {
		if field.value == "" {
			return fmt.Errorf("%w: %s is required", ErrInvalidHolder, field.label)
		}
		if !storable(field.value) {
			return fmt.Errorf("%w: %s contains invalid characters", ErrInvalidHolder, field.label)
		}
		if utf8.RuneCountInString(field.value) > maxHolderFieldLength {
			return fmt.Errorf("%w: %s is too long", ErrInvalidHolder, field.label)
		}
	}

	if h.DateOfBirth.IsZero() {
		return fmt.Errorf("%w: date of birth is required", ErrInvalidHolder)
	}
	if h.DateOfBirth.Before(earliestDateOfBirth) || h.DateOfBirth.After(now) {
		return fmt.Errorf("%w: date of birth must be between 1900 and today", ErrInvalidHolder)
	}
	return nil
}

// rejectSimilarHolder fails if an account has the same date of birth and both
// a similar name and a similar ID number, which suggests one person signing up
// again with altered details. An identical ID number is left to the unique
// constraint.
//
// A unique constraint cannot express similarity, so signups sharing a date of
// birth take turns until tx ends. Without that, two similar signups could each
// miss the other's uncommitted row. Other signups still run in parallel.
func rejectSimilarHolder(ctx context.Context, tx pgx.Tx, holder Holder) error {
	_, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(($1::date - DATE '1900-01-01')::bigint)`, holder.DateOfBirth)
	if err != nil {
		return fmt.Errorf("locking date of birth: %w", err)
	}

	var similar bool
	err = tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM accounts
			WHERE date_of_birth = $1
			  AND levenshtein(id_number, $2) BETWEEN 1 AND $4
			  AND levenshtein(lower(name), lower($3)) <= $4
		)`, holder.DateOfBirth, holder.IDNumber, holder.Name, maxHolderDistance).Scan(&similar)
	if err != nil {
		return fmt.Errorf("searching for similar holders: %w", err)
	}
	if similar {
		return ErrSimilarHolderExists
	}
	return nil
}

// storable reports whether PostgreSQL can hold s as text.
func storable(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
