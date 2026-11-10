package bank

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// openBank connects to the test database. Account IDs are random, so tests
// never see each other's rows and need no cleanup.
func openBank(t *testing.T) *Bank {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Fatal("DATABASE_URL is not set; the README explains how to run the tests")
	}
	b, err := Open(t.Context(), url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

// validHolder returns a holder with a unique ID number.
func validHolder() Holder {
	return Holder{
		Name:        "Ada Lovelace",
		Address:     "12 St James Square, London",
		DateOfBirth: time.Date(1985, time.December, 10, 0, 0, 0, 0, time.UTC),
		IDNumber:    fmt.Sprintf("ID%d", rand.Uint64()),
	}
}

func newAccount(t *testing.T, b *Bank, balance int64) string {
	t.Helper()
	account, err := b.CreateAccount(t.Context(), validHolder(), balance)
	if err != nil {
		t.Fatalf("CreateAccount(%d): %v", balance, err)
	}
	return account.ID
}

func balanceOf(t *testing.T, b *Bank, id string) int64 {
	t.Helper()
	account, err := b.Account(t.Context(), id)
	if err != nil {
		t.Fatalf("Account(%q): %v", id, err)
	}
	return account.Balance
}

func TestCreateAccount(t *testing.T) {
	b := openBank(t)

	tests := []struct {
		name    string
		initial int64
		wantErr error
	}{
		{name: "zero balance", initial: 0},
		{name: "positive balance", initial: 1050},
		{name: "negative balance", initial: -1, wantErr: ErrNegativeInitialBalance},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account, err := b.CreateAccount(t.Context(), validHolder(), tt.initial)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && account.Balance != tt.initial {
				t.Errorf("Balance = %d, want %d", account.Balance, tt.initial)
			}
		})
	}
}

func TestCreateAccountRejectsInvalidHolder(t *testing.T) {
	b := openBank(t)

	tests := []struct {
		name   string
		modify func(*Holder)
	}{
		{"empty name", func(h *Holder) { h.Name = "" }},
		{"whitespace name", func(h *Holder) { h.Name = "  \t" }},
		{"name with NUL", func(h *Holder) { h.Name = "a\x00b" }},
		{"name with invalid UTF-8", func(h *Holder) { h.Name = "a\xffb" }},
		{"empty address", func(h *Holder) { h.Address = "" }},
		{"empty ID number", func(h *Holder) { h.IDNumber = "" }},
		{"ID number of only separators", func(h *Holder) { h.IDNumber = " - " }},
		{"missing date of birth", func(h *Holder) { h.DateOfBirth = time.Time{} }},
		{"future date of birth", func(h *Holder) { h.DateOfBirth = time.Now().AddDate(0, 0, 1) }},
		{"date of birth before 1900", func(h *Holder) { h.DateOfBirth = time.Date(1899, time.December, 31, 0, 0, 0, 0, time.UTC) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			holder := validHolder()
			tt.modify(&holder)
			if _, err := b.CreateAccount(t.Context(), holder, 0); !errors.Is(err, ErrInvalidHolder) {
				t.Errorf("err = %v, want %v", err, ErrInvalidHolder)
			}
		})
	}
}

func TestCreateAccountStoresNormalizedHolder(t *testing.T) {
	b := openBank(t)
	holder := validHolder()
	holder.Name = "  Ada Lovelace "
	holder.IDNumber = " ab-" + holder.IDNumber + " 9"

	account, err := b.CreateAccount(t.Context(), holder, 0)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	var name, address, idNumber string
	var dateOfBirth time.Time
	err = b.pool.QueryRow(t.Context(),
		`SELECT name, address, date_of_birth, id_number FROM accounts WHERE id = $1`, account.ID,
	).Scan(&name, &address, &dateOfBirth, &idNumber)
	if err != nil {
		t.Fatalf("reading holder: %v", err)
	}

	want := holder.normalized()
	if name != "Ada Lovelace" || address != want.Address || idNumber != want.IDNumber || !strings.HasPrefix(idNumber, "AB") {
		t.Errorf("stored %q, %q, %q; want %+v", name, address, idNumber, want)
	}
	if !dateOfBirth.Equal(holder.DateOfBirth) {
		t.Errorf("date of birth = %v, want %v", dateOfBirth, holder.DateOfBirth)
	}
}

func TestCreateAccountAllowsOneAccountPerIDNumber(t *testing.T) {
	b := openBank(t)
	holder := validHolder()

	if _, err := b.CreateAccount(t.Context(), holder, 0); err != nil {
		t.Fatalf("first account: %v", err)
	}

	variant := holder
	variant.IDNumber = strings.ToLower(holder.IDNumber[:2]) + "-" + holder.IDNumber[2:]
	for _, duplicate := range []Holder{holder, variant} {
		if _, err := b.CreateAccount(t.Context(), duplicate, 100); !errors.Is(err, ErrHolderAlreadyRegistered) {
			t.Errorf("ID number %q: err = %v, want %v", duplicate.IDNumber, err, ErrHolderAlreadyRegistered)
		}
	}
}

// alterEnd replaces the last n characters of a validHolder ID number, which are
// digits, so the result is exactly n edits away.
func alterEnd(idNumber string, n int) string {
	return idNumber[:len(idNumber)-n] + strings.Repeat("Z", n)
}

func TestCreateAccountRejectsSimilarHolder(t *testing.T) {
	b := openBank(t)
	otherDate := time.Date(1985, time.December, 11, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		modify  func(*Holder)
		wantErr error
	}{
		{"one edit in each", func(h *Holder) { h.Name = "Ada Lovelacf"; h.IDNumber = alterEnd(h.IDNumber, 1) }, ErrSimilarHolderExists},
		{"three edits in each", func(h *Holder) { h.Name = "Ada Lovelxyz"; h.IDNumber = alterEnd(h.IDNumber, 3) }, ErrSimilarHolderExists},
		{"removed letter shifts the name", func(h *Holder) { h.Name = "Ad Lovelace"; h.IDNumber = alterEnd(h.IDNumber, 1) }, ErrSimilarHolderExists},
		{"added character in the ID number", func(h *Holder) { h.IDNumber += "Z" }, ErrSimilarHolderExists},
		{"name differs only in case", func(h *Holder) { h.Name = "ADA LOVELACE"; h.IDNumber = alterEnd(h.IDNumber, 1) }, ErrSimilarHolderExists},
		{"identical ID number", func(h *Holder) {}, ErrHolderAlreadyRegistered},
		{"identical ID number, unrelated holder", func(h *Holder) { h.Name = "Grace Hopper"; h.DateOfBirth = otherDate }, ErrHolderAlreadyRegistered},
		{"name four edits away", func(h *Holder) { h.Name = "Ada Lovexyzw"; h.IDNumber = alterEnd(h.IDNumber, 1) }, nil},
		{"ID number four edits away", func(h *Holder) { h.IDNumber = alterEnd(h.IDNumber, 4) }, nil},
		{"different date of birth", func(h *Holder) { h.DateOfBirth = otherDate; h.IDNumber = alterEnd(h.IDNumber, 1) }, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := validHolder()
			if _, err := b.CreateAccount(t.Context(), existing, 0); err != nil {
				t.Fatalf("first account: %v", err)
			}

			candidate := existing
			tt.modify(&candidate)
			if _, err := b.CreateAccount(t.Context(), candidate, 0); !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestCreateAccountHandlesLongestAllowedHolder(t *testing.T) {
	b := openBank(t)
	holder := validHolder()
	holder.DateOfBirth = time.Date(1950, time.March, 3, 0, 0, 0, 0, time.UTC)
	holder.Name = strings.Repeat("é", maxHolderFieldLength)
	holder.Address = strings.Repeat("é", maxHolderFieldLength)

	// The second signup compares two longest-allowed names in PostgreSQL.
	for range 2 {
		holder.IDNumber = validHolder().IDNumber
		if _, err := b.CreateAccount(t.Context(), holder, 0); err != nil {
			t.Fatalf("CreateAccount: %v", err)
		}
	}

	holder.Name += "é"
	holder.IDNumber = validHolder().IDNumber
	if _, err := b.CreateAccount(t.Context(), holder, 0); !errors.Is(err, ErrInvalidHolder) {
		t.Errorf("err = %v, want %v", err, ErrInvalidHolder)
	}
}

// All of these ID numbers are one edit apart and none is identical, so only the
// similarity check can stop them, and only if signups take turns.
func TestConcurrentSimilarSignupsCreateOneAccount(t *testing.T) {
	const attempts = 26

	b := openBank(t)
	base := validHolder()

	var succeeded atomic.Int64
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() {
			holder := base
			holder.IDNumber = base.IDNumber + string(rune('A'+i))
			_, err := b.CreateAccount(t.Context(), holder, 0)
			switch {
			case err == nil:
				succeeded.Add(1)
			case !errors.Is(err, ErrSimilarHolderExists):
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()

	if got := succeeded.Load(); got != 1 {
		t.Errorf("succeeded = %d, want 1", got)
	}
}

func TestConcurrentSignupsCreateOneAccountPerIDNumber(t *testing.T) {
	const attempts = 100

	b := openBank(t)
	holder := validHolder()

	var succeeded atomic.Int64
	var wg sync.WaitGroup
	for range attempts {
		wg.Go(func() {
			_, err := b.CreateAccount(t.Context(), holder, 10)
			switch {
			case err == nil:
				succeeded.Add(1)
			case !errors.Is(err, ErrHolderAlreadyRegistered):
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()

	if got := succeeded.Load(); got != 1 {
		t.Errorf("succeeded = %d, want 1", got)
	}
}

func TestDeposit(t *testing.T) {
	b := openBank(t)
	funded := newAccount(t, b, 100)
	full := newAccount(t, b, math.MaxInt64)

	tests := []struct {
		name        string
		id          string
		amount      int64
		wantErr     error
		wantBalance int64
	}{
		{name: "adds to balance", id: funded, amount: 50, wantBalance: 150},
		{name: "zero amount", id: funded, amount: 0, wantErr: ErrInvalidAmount},
		{name: "negative amount", id: funded, amount: -1, wantErr: ErrInvalidAmount},
		{name: "unknown account", id: "missing", amount: 1, wantErr: ErrAccountNotFound},
		{name: "overflow", id: full, amount: 1, wantErr: ErrBalanceLimitExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account, err := b.Deposit(t.Context(), tt.id, tt.amount)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && account.Balance != tt.wantBalance {
				t.Errorf("Balance = %d, want %d", account.Balance, tt.wantBalance)
			}
		})
	}
}

func TestTransfer(t *testing.T) {
	b := openBank(t)

	tests := []struct {
		name        string
		fromBalance int64
		toBalance   int64
		amount      int64
		wantErr     error
		wantFrom    int64
		wantTo      int64
	}{
		{name: "partial", fromBalance: 100, toBalance: 5, amount: 30, wantFrom: 70, wantTo: 35},
		{name: "entire balance", fromBalance: 100, toBalance: 0, amount: 100, wantFrom: 0, wantTo: 100},
		{name: "overdraft", fromBalance: 100, toBalance: 0, amount: 101, wantErr: ErrInsufficientFunds, wantFrom: 100, wantTo: 0},
		{name: "zero amount", fromBalance: 100, toBalance: 0, amount: 0, wantErr: ErrInvalidAmount, wantFrom: 100, wantTo: 0},
		{name: "negative amount", fromBalance: 100, toBalance: 0, amount: -1, wantErr: ErrInvalidAmount, wantFrom: 100, wantTo: 0},
		{name: "overflow", fromBalance: 100, toBalance: math.MaxInt64, amount: 1, wantErr: ErrBalanceLimitExceeded, wantFrom: 100, wantTo: math.MaxInt64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from := newAccount(t, b, tt.fromBalance)
			to := newAccount(t, b, tt.toBalance)

			if _, _, err := b.Transfer(t.Context(), from, to, tt.amount); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got := balanceOf(t, b, from); got != tt.wantFrom {
				t.Errorf("from balance = %d, want %d", got, tt.wantFrom)
			}
			if got := balanceOf(t, b, to); got != tt.wantTo {
				t.Errorf("to balance = %d, want %d", got, tt.wantTo)
			}
		})
	}
}

func TestTransferRejectsInvalidAccounts(t *testing.T) {
	b := openBank(t)
	id := newAccount(t, b, 100)

	tests := []struct {
		name     string
		from, to string
		wantErr  error
	}{
		{name: "same account", from: id, to: id, wantErr: ErrSameAccount},
		{name: "unknown source", from: "missing", to: id, wantErr: ErrAccountNotFound},
		{name: "unknown destination", from: id, to: "missing", wantErr: ErrAccountNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := b.Transfer(t.Context(), tt.from, tt.to, 1); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// While one transaction holds an account pair, a transfer on other accounts
// must complete and a transfer on the same pair must wait.
func TestOnlyTransfersSharingAnAccountWait(t *testing.T) {
	b := openBank(t)
	heldFrom, heldTo := newAccount(t, b, 100), newAccount(t, b, 0)
	freeFrom, freeTo := newAccount(t, b, 100), newAccount(t, b, 0)

	tx, err := b.pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback(t.Context())
	if _, err := lockAccounts(t.Context(), tx, heldFrom, heldTo); err != nil {
		t.Fatalf("lockAccounts: %v", err)
	}

	unrelated, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, _, err := b.Transfer(unrelated, freeFrom, freeTo, 1); err != nil {
		t.Errorf("unrelated transfer: %v", err)
	}

	sharing, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := b.Transfer(sharing, heldFrom, heldTo, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("transfer on a locked pair: err = %v, want %v", err, context.DeadlineExceeded)
	}
}

// PostgreSQL resolves a deadlock by aborting one side after a delay. Either the
// abort or the deadline, which a run full of those delays would hit, fails the
// test.
func TestOpposingTransfersDoNotDeadlock(t *testing.T) {
	const transfers = 100

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	b := openBank(t)
	first, second := newAccount(t, b, transfers), newAccount(t, b, transfers)

	var wg sync.WaitGroup
	transfer := func(from, to string) {
		wg.Go(func() {
			if _, _, err := b.Transfer(ctx, from, to, 1); err != nil {
				t.Errorf("Transfer: %v", err)
			}
		})
	}
	for range transfers {
		transfer(first, second)
		transfer(second, first)
	}
	wg.Wait()

	if got := balanceOf(t, b, first); got != transfers {
		t.Errorf("first balance = %d, want %d", got, transfers)
	}
	if got := balanceOf(t, b, second); got != transfers {
		t.Errorf("second balance = %d, want %d", got, transfers)
	}
	assertLedgerReconciles(t, b, []string{first, second})
}

// A shared source must never be overdrawn, however many transfers race for it.
func TestConcurrentTransfersCannotOverdraw(t *testing.T) {
	const balance, attempts = 100, 1000

	b := openBank(t)
	from := newAccount(t, b, balance)
	to := newAccount(t, b, 0)

	var succeeded atomic.Int64
	var wg sync.WaitGroup
	for range attempts {
		wg.Go(func() {
			_, _, err := b.Transfer(t.Context(), from, to, 1)
			switch {
			case err == nil:
				succeeded.Add(1)
			case !errors.Is(err, ErrInsufficientFunds):
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()

	if got := succeeded.Load(); got != balance {
		t.Errorf("succeeded = %d, want %d", got, balance)
	}
	if got := balanceOf(t, b, from); got != 0 {
		t.Errorf("from balance = %d, want 0", got)
	}
	if got := balanceOf(t, b, to); got != balance {
		t.Errorf("to balance = %d, want %d", got, balance)
	}
}

func TestConcurrentTransfersConserveMoney(t *testing.T) {
	const (
		accountCount = 8
		balance      = 1000
		workers      = 32
		transfers    = 100
	)

	b := openBank(t)
	ids := make([]string, accountCount)
	for i := range ids {
		ids[i] = newAccount(t, b, balance)
	}

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range transfers {
				from, to := ids[rand.IntN(accountCount)], ids[rand.IntN(accountCount)]
				_, _, err := b.Transfer(t.Context(), from, to, rand.Int64N(balance)+1)
				if err != nil && !errors.Is(err, ErrInsufficientFunds) && !errors.Is(err, ErrSameAccount) {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
	wg.Wait()

	var total int64
	for _, id := range ids {
		got := balanceOf(t, b, id)
		if got < 0 {
			t.Errorf("account %s has negative balance %d", id, got)
		}
		total += got
	}
	if want := int64(accountCount * balance); total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
	assertLedgerReconciles(t, b, ids)
}

func TestConcurrentDepositsAreAllApplied(t *testing.T) {
	const deposits = 1000

	b := openBank(t)
	id := newAccount(t, b, 0)

	var wg sync.WaitGroup
	for range deposits {
		wg.Go(func() {
			if _, err := b.Deposit(t.Context(), id, 1); err != nil {
				t.Errorf("Deposit: %v", err)
			}
		})
	}
	wg.Wait()

	if got := balanceOf(t, b, id); got != deposits {
		t.Errorf("balance = %d, want %d", got, deposits)
	}
}
