// Package bank stores accounts in PostgreSQL, moves money between them, and
// records every movement in a ledger. All methods are safe for concurrent use.
// Amounts are integers in the currency's minor unit (for example, cents), so
// arithmetic is exact.
package bank

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Errors reported for invalid operations. Any other error is an infrastructure
// failure.
var (
	ErrAccountNotFound         = errors.New("account not found")
	ErrInvalidHolder           = errors.New("invalid account holder")
	ErrHolderAlreadyRegistered = errors.New("an account already exists for this ID number")
	ErrSimilarHolderExists     = errors.New("an account already exists for a similar holder")
	ErrNegativeInitialBalance  = errors.New("initial balance must not be negative")
	ErrInvalidAmount           = errors.New("amount must be positive")
	ErrSameAccount             = errors.New("source and destination accounts must differ")
	ErrInsufficientFunds       = errors.New("insufficient funds")
	ErrBalanceLimitExceeded    = errors.New("balance limit exceeded")
)

// Account is a point-in-time view of an account.
type Account struct {
	ID      string
	Balance int64
}

// Bank is a collection of accounts persisted in PostgreSQL.
type Bank struct {
	pool *pgxpool.Pool
}

// Open connects to the database at databaseURL, whose schema must already
// exist. The caller must Close the returned Bank.
func Open(ctx context.Context, databaseURL string) (*Bank, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("configuring database pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	return &Bank{pool: pool}, nil
}

// Close releases all database connections.
func (b *Bank) Close() {
	b.pool.Close()
}

// CreateAccount opens an account for holder with initialBalance, which may be
// zero. Each ID number can hold only one account, and a holder too similar to
// an existing one is rejected. Concurrent attempts cannot get around either
// rule.
func (b *Bank) CreateAccount(ctx context.Context, holder Holder, initialBalance int64) (Account, error) {
	holder = holder.normalized()
	if err := holder.validate(time.Now()); err != nil {
		return Account{}, err
	}
	if initialBalance < 0 {
		return Account{}, ErrNegativeInitialBalance
	}

	account := Account{ID: rand.Text()}
	err := pgx.BeginFunc(ctx, b.pool, func(tx pgx.Tx) error {
		if err := rejectSimilarHolder(ctx, tx, holder); err != nil {
			return err
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO accounts (id, name, address, date_of_birth, id_number)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (id_number) DO NOTHING`,
			account.ID, holder.Name, holder.Address, holder.DateOfBirth, holder.IDNumber)
		if err != nil {
			return fmt.Errorf("inserting account: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrHolderAlreadyRegistered
		}

		if initialBalance == 0 {
			return nil
		}
		return post(ctx, tx, &account, posting{
			transactionID: rand.Text(),
			entryType:     EntryOpeningBalance,
			amount:        initialBalance,
		})
	})
	if err != nil {
		return Account{}, err
	}
	return account, nil
}

// Account returns the current state of the account with the given ID.
func (b *Bank) Account(ctx context.Context, id string) (Account, error) {
	if !storable(id) {
		return Account{}, ErrAccountNotFound
	}

	account := Account{ID: id}
	err := b.pool.QueryRow(ctx, `SELECT balance FROM accounts WHERE id = $1`, id).Scan(&account.Balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("loading account: %w", err)
	}
	return account, nil
}

// Deposit adds amount to an account and returns its new state.
func (b *Bank) Deposit(ctx context.Context, id string, amount int64) (Account, error) {
	if amount <= 0 {
		return Account{}, ErrInvalidAmount
	}

	var updated Account
	err := pgx.BeginFunc(ctx, b.pool, func(tx pgx.Tx) error {
		locked, err := lockAccounts(ctx, tx, id)
		if err != nil {
			return err
		}

		account := locked[0]
		if !canReceive(account, amount) {
			return ErrBalanceLimitExceeded
		}

		err = post(ctx, tx, &account, posting{
			transactionID: rand.Text(),
			entryType:     EntryDeposit,
			amount:        amount,
		})
		updated = account
		return err
	})
	if err != nil {
		return Account{}, err
	}
	return updated, nil
}

// Transfer atomically moves amount between two accounts and returns the new
// state of both. It fails without side effects if the source cannot cover the
// amount. Only transfers that share an account wait for each other.
func (b *Bank) Transfer(ctx context.Context, fromID, toID string, amount int64) (from, to Account, err error) {
	if amount <= 0 {
		return Account{}, Account{}, ErrInvalidAmount
	}
	if fromID == toID {
		return Account{}, Account{}, ErrSameAccount
	}

	err = pgx.BeginFunc(ctx, b.pool, func(tx pgx.Tx) error {
		locked, err := lockAccounts(ctx, tx, fromID, toID)
		if err != nil {
			return err
		}

		source, destination := locked[0], locked[1]
		if source.ID != fromID {
			source, destination = destination, source
		}

		if source.Balance < amount {
			return ErrInsufficientFunds
		}
		if !canReceive(destination, amount) {
			return ErrBalanceLimitExceeded
		}

		transactionID := rand.Text()
		err = post(ctx, tx, &source, posting{
			transactionID:  transactionID,
			entryType:      EntryTransfer,
			amount:         -amount,
			counterpartyID: destination.ID,
		})
		if err != nil {
			return err
		}
		err = post(ctx, tx, &destination, posting{
			transactionID:  transactionID,
			entryType:      EntryTransfer,
			amount:         amount,
			counterpartyID: source.ID,
		})
		from, to = source, destination
		return err
	})
	if err != nil {
		return Account{}, Account{}, err
	}
	return from, to, nil
}

// lockAccounts locks the rows until tx ends and returns them in ID order. A
// consistent lock order means opposing transfers (A to B and B to A) cannot
// deadlock.
func lockAccounts(ctx context.Context, tx pgx.Tx, ids ...string) ([]Account, error) {
	if slices.ContainsFunc(ids, func(id string) bool { return !storable(id) }) {
		return nil, ErrAccountNotFound
	}

	rows, err := tx.Query(ctx,
		`SELECT id, balance FROM accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, fmt.Errorf("locking accounts: %w", err)
	}

	accounts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Account])
	if err != nil {
		return nil, fmt.Errorf("reading locked accounts: %w", err)
	}
	if len(accounts) != len(ids) {
		return nil, ErrAccountNotFound
	}
	return accounts, nil
}

func canReceive(account Account, amount int64) bool {
	return account.Balance <= math.MaxInt64-amount
}
