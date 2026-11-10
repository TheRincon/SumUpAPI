package bank

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const ledgerPageSize = 100

// EntryType says why a balance changed.
type EntryType string

const (
	EntryOpeningBalance EntryType = "opening_balance"
	EntryDeposit        EntryType = "deposit"
	EntryTransfer       EntryType = "transfer"
)

// LedgerEntry is one immutable change to one account's balance. Amount is
// positive for money in and negative for money out. CounterpartyID is empty
// unless the entry is part of a transfer.
type LedgerEntry struct {
	ID             int64
	TransactionID  string
	Type           EntryType
	Amount         int64
	BalanceAfter   int64
	CounterpartyID string
	CreatedAt      time.Time
}

// Ledger returns up to one page of an account's entries, newest first, starting
// below the entry ID before, or at the newest entry if before is zero. A
// non-zero nextBefore is the value that fetches the following page.
func (b *Bank) Ledger(ctx context.Context, accountID string, before int64) (entries []LedgerEntry, nextBefore int64, err error) {
	if _, err := b.Account(ctx, accountID); err != nil {
		return nil, 0, err
	}

	rows, err := b.pool.Query(ctx, `
		SELECT id, transaction_id, type, amount, balance_after,
		       COALESCE(counterparty_account_id, ''), created_at
		FROM ledger_entries
		WHERE account_id = $1 AND ($2::bigint = 0 OR id < $2)
		ORDER BY id DESC
		LIMIT $3`, accountID, before, ledgerPageSize+1)
	if err != nil {
		return nil, 0, fmt.Errorf("querying ledger: %w", err)
	}

	entries, err = pgx.CollectRows(rows, pgx.RowToStructByPos[LedgerEntry])
	if err != nil {
		return nil, 0, fmt.Errorf("reading ledger: %w", err)
	}

	if len(entries) > ledgerPageSize {
		entries = entries[:ledgerPageSize]
		nextBefore = entries[ledgerPageSize-1].ID
	}
	return entries, nextBefore, nil
}

// posting describes one balance change.
type posting struct {
	transactionID  string
	entryType      EntryType
	amount         int64
	counterpartyID string
}

// post appends a ledger entry for an account whose row tx has locked or
// created, and sets account.Balance to the result. A trigger in schema.sql
// applies the entry to the balance within this one INSERT, and rejects every
// other way of changing a balance, so the two cannot diverge.
func post(ctx context.Context, tx pgx.Tx, account *Account, p posting) error {
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger_entries
			(transaction_id, account_id, type, amount, counterparty_account_id)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''))
		RETURNING balance_after`,
		p.transactionID, account.ID, string(p.entryType), p.amount, p.counterpartyID,
	).Scan(&account.Balance)
	if err != nil {
		return fmt.Errorf("writing ledger entry: %w", err)
	}
	return nil
}
