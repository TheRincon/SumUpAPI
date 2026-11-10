package bank

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/jackc/pgx/v5"
)

func ledgerOf(t *testing.T, b *Bank, id string) []LedgerEntry {
	t.Helper()
	entries, _, err := b.Ledger(t.Context(), id, 0)
	if err != nil {
		t.Fatalf("Ledger(%q): %v", id, err)
	}
	return entries
}

// assertLedgerReconciles checks the accounting invariants across accounts:
// balances equal the sum of their entries, each entry follows from the one
// before it, and every transfer is two legs that net to zero.
func assertLedgerReconciles(t *testing.T, b *Bank, ids []string) {
	t.Helper()

	checks := []struct{ name, query string }{
		{"balance differs from the sum of its entries", `
			SELECT count(*) FROM accounts a
			WHERE a.id = ANY($1) AND a.balance <>
				(SELECT COALESCE(SUM(e.amount), 0) FROM ledger_entries e WHERE e.account_id = a.id)`},
		{"balance_after does not follow from the previous entry", `
			SELECT count(*) FROM (
				SELECT amount, balance_after,
				       lag(balance_after, 1, 0::bigint) OVER (PARTITION BY account_id ORDER BY id) AS previous
				FROM ledger_entries WHERE account_id = ANY($1)
			) entries WHERE previous + amount <> balance_after`},
		{"transfer does not have two legs netting to zero", `
			SELECT count(*) FROM (
				SELECT transaction_id FROM ledger_entries
				WHERE type = 'transfer' AND account_id = ANY($1)
				GROUP BY transaction_id
				HAVING SUM(amount) <> 0 OR COUNT(*) <> 2
			) unbalanced`},
	}

	for _, check := range checks {
		var violations int
		if err := b.pool.QueryRow(t.Context(), check.query, ids).Scan(&violations); err != nil {
			t.Fatalf("%s: %v", check.name, err)
		}
		if violations != 0 {
			t.Errorf("%s: %d violations", check.name, violations)
		}
	}
}

// These tests write SQL directly, because the guarantee is that nothing outside
// the ledger can move a balance, including code that bypasses package bank.
func TestBalanceCannotChangeWithoutLedgerEntry(t *testing.T) {
	b := openBank(t)
	id := newAccount(t, b, 100)

	if _, err := b.pool.Exec(t.Context(), `UPDATE accounts SET balance = balance + 1 WHERE id = $1`, id); err == nil {
		t.Error("the database allowed a direct balance update")
	}

	otherID := fmt.Sprintf("DIRECT%d", rand.Uint64())
	_, err := b.pool.Exec(t.Context(), `
		INSERT INTO accounts (id, name, address, date_of_birth, id_number, balance)
		VALUES ($1, 'n', 'a', '1985-12-10', $1, 5)`, otherID)
	if err == nil {
		t.Error("the database allowed an account to start with an unrecorded balance")
	}

	if got := balanceOf(t, b, id); got != 100 {
		t.Errorf("balance = %d, want 100", got)
	}
	assertLedgerReconciles(t, b, []string{id})
}

func TestLedgerEntryAppliesToBalanceInOneStatement(t *testing.T) {
	b := openBank(t)
	id := newAccount(t, b, 100)

	// A caller cannot supply its own balance_after.
	_, err := b.pool.Exec(t.Context(), `
		INSERT INTO ledger_entries (transaction_id, account_id, type, amount, balance_after)
		VALUES ('DIRECT', $1, 'deposit', 25, 999999)`, id)
	if err != nil {
		t.Fatalf("inserting entry: %v", err)
	}

	if got := balanceOf(t, b, id); got != 125 {
		t.Errorf("balance = %d, want 125", got)
	}
	if got := ledgerOf(t, b, id)[0].BalanceAfter; got != 125 {
		t.Errorf("balance_after = %d, want 125", got)
	}
	assertLedgerReconciles(t, b, []string{id})
}

func TestRejectedLedgerEntryLeavesBalanceUnchanged(t *testing.T) {
	b := openBank(t)
	id := newAccount(t, b, 100)

	tests := []struct{ name, entryType, amount string }{
		{"would overdraw", "deposit", "-101"},
		{"zero amount", "deposit", "0"},
		{"unknown type", "bogus", "5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := b.pool.Exec(t.Context(), `
				INSERT INTO ledger_entries (transaction_id, account_id, type, amount)
				VALUES ('DIRECT', $1, $2, `+tt.amount+`)`, id, tt.entryType)
			if err == nil {
				t.Fatal("the database accepted the entry")
			}
			if got := balanceOf(t, b, id); got != 100 {
				t.Errorf("balance = %d, want 100", got)
			}
			if got := len(ledgerOf(t, b, id)); got != 1 {
				t.Errorf("ledger has %d entries, want 1", got)
			}
		})
	}
}

func TestFailedSecondLegRollsBackFirstLeg(t *testing.T) {
	b := openBank(t)
	from, to := newAccount(t, b, 100), newAccount(t, b, 0)

	err := pgx.BeginFunc(t.Context(), b.pool, func(tx pgx.Tx) error {
		source := Account{ID: from}
		err := post(t.Context(), tx, &source, posting{
			transactionID: "T", entryType: EntryTransfer, amount: -30, counterpartyID: to,
		})
		if err != nil {
			t.Errorf("first leg: %v", err)
		}
		if source.Balance != 70 {
			t.Errorf("balance after first leg = %d, want 70", source.Balance)
		}

		destination := Account{ID: to}
		return post(t.Context(), tx, &destination, posting{
			transactionID: "T", entryType: EntryType("bogus"), amount: 30, counterpartyID: from,
		})
	})
	if err == nil {
		t.Fatal("the transaction succeeded despite an invalid second leg")
	}

	if got := balanceOf(t, b, from); got != 100 {
		t.Errorf("from balance = %d, want 100", got)
	}
	if got := balanceOf(t, b, to); got != 0 {
		t.Errorf("to balance = %d, want 0", got)
	}
	if got := len(ledgerOf(t, b, from)); got != 1 {
		t.Errorf("source has %d entries, want only the opening balance", got)
	}
	if got := len(ledgerOf(t, b, to)); got != 0 {
		t.Errorf("destination has %d entries, want 0", got)
	}
}

func TestLedgerCannotBeTruncated(t *testing.T) {
	b := openBank(t)

	tx, err := b.pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// Rolled back so that a regression cannot wipe the shared test database.
	defer tx.Rollback(t.Context())

	if _, err := tx.Exec(t.Context(), `TRUNCATE ledger_entries`); err == nil {
		t.Error("the database allowed the ledger to be truncated")
	}
}

func TestLedgerRecordsEveryMovement(t *testing.T) {
	b := openBank(t)
	from := newAccount(t, b, 100)
	to := newAccount(t, b, 0)

	if _, err := b.Deposit(t.Context(), from, 50); err != nil {
		t.Fatalf("Deposit: %v", err)
	}
	if _, _, err := b.Transfer(t.Context(), from, to, 30); err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	sent := ledgerOf(t, b, from)
	if len(sent) != 3 {
		t.Fatalf("source has %d entries, want 3", len(sent))
	}
	want := []struct {
		entryType    EntryType
		amount       int64
		balanceAfter int64
		counterparty string
	}{
		{EntryTransfer, -30, 120, to},
		{EntryDeposit, 50, 150, ""},
		{EntryOpeningBalance, 100, 100, ""},
	}
	for i, w := range want {
		got := sent[i]
		if got.Type != w.entryType || got.Amount != w.amount || got.BalanceAfter != w.balanceAfter || got.CounterpartyID != w.counterparty {
			t.Errorf("entry %d = %+v, want %+v", i, got, w)
		}
		if got.TransactionID == "" || got.CreatedAt.IsZero() {
			t.Errorf("entry %d is missing its transaction ID or timestamp: %+v", i, got)
		}
	}

	received := ledgerOf(t, b, to)
	if len(received) != 1 {
		t.Fatalf("destination has %d entries, want 1", len(received))
	}
	credit := received[0]
	if credit.Amount != 30 || credit.BalanceAfter != 30 || credit.CounterpartyID != from {
		t.Errorf("credit = %+v, want +30 from %s leaving 30", credit, from)
	}
	if credit.TransactionID != sent[0].TransactionID {
		t.Errorf("transfer legs have different transaction IDs: %s and %s", credit.TransactionID, sent[0].TransactionID)
	}

	assertLedgerReconciles(t, b, []string{from, to})
}

func TestZeroOpeningBalanceWritesNoEntry(t *testing.T) {
	b := openBank(t)

	if entries := ledgerOf(t, b, newAccount(t, b, 0)); len(entries) != 0 {
		t.Errorf("ledger has %d entries, want 0: %+v", len(entries), entries)
	}
}

func TestRejectedOperationsWriteNoEntries(t *testing.T) {
	b := openBank(t)
	from := newAccount(t, b, 10)
	to := newAccount(t, b, 0)

	if _, _, err := b.Transfer(t.Context(), from, to, 11); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("err = %v, want %v", err, ErrInsufficientFunds)
	}

	if got := len(ledgerOf(t, b, from)); got != 1 {
		t.Errorf("source has %d entries, want only the opening balance", got)
	}
	if got := len(ledgerOf(t, b, to)); got != 0 {
		t.Errorf("destination has %d entries, want 0", got)
	}
}

func TestLedgerPagesThroughAllEntries(t *testing.T) {
	const deposits = ledgerPageSize + 5

	b := openBank(t)
	id := newAccount(t, b, 0)
	for range deposits {
		if _, err := b.Deposit(t.Context(), id, 1); err != nil {
			t.Fatalf("Deposit: %v", err)
		}
	}

	first, next, err := b.Ledger(t.Context(), id, 0)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first) != ledgerPageSize || next != first[ledgerPageSize-1].ID {
		t.Fatalf("first page has %d entries and cursor %d", len(first), next)
	}
	if first[0].BalanceAfter != deposits {
		t.Errorf("newest balance_after = %d, want %d", first[0].BalanceAfter, deposits)
	}

	second, next, err := b.Ledger(t.Context(), id, next)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second) != 5 || next != 0 {
		t.Fatalf("second page has %d entries and cursor %d, want 5 and 0", len(second), next)
	}
	if second[len(second)-1].BalanceAfter != 1 {
		t.Errorf("oldest balance_after = %d, want 1", second[len(second)-1].BalanceAfter)
	}
}

func TestLedgerOfExactlyOnePageHasNoCursor(t *testing.T) {
	b := openBank(t)
	id := newAccount(t, b, 0)
	for range ledgerPageSize {
		if _, err := b.Deposit(t.Context(), id, 1); err != nil {
			t.Fatalf("Deposit: %v", err)
		}
	}

	if _, next, err := b.Ledger(t.Context(), id, 0); err != nil || next != 0 {
		t.Errorf("cursor = %d, err = %v, want 0 and nil", next, err)
	}
}

func TestLedgerOfUnknownAccount(t *testing.T) {
	if _, _, err := openBank(t).Ledger(t.Context(), "missing", 0); !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("err = %v, want %v", err, ErrAccountNotFound)
	}
}

func TestLedgerIsAppendOnly(t *testing.T) {
	b := openBank(t)
	id := newAccount(t, b, 100)

	for _, statement := range []string{
		`UPDATE ledger_entries SET amount = 1 WHERE account_id = $1`,
		`DELETE FROM ledger_entries WHERE account_id = $1`,
	} {
		if _, err := b.pool.Exec(t.Context(), statement, id); err == nil {
			t.Errorf("the database allowed %q", statement)
		}
	}

	if got := len(ledgerOf(t, b, id)); got != 1 {
		t.Errorf("ledger has %d entries, want 1", got)
	}
}
