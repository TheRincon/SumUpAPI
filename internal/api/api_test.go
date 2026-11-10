package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"sumup/api/internal/bank"
)

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Fatal("DATABASE_URL is not set; the README explains how to run the tests")
	}
	accounts, err := bank.Open(t.Context(), url)
	if err != nil {
		t.Fatalf("bank.Open: %v", err)
	}
	t.Cleanup(accounts.Close)
	return NewHandler(accounts)
}

func serve(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
	return recorder
}

// send issues a request against h, checks that the response is JSON, decodes
// it into out, and returns the response status.
func send(t *testing.T, h http.Handler, method, path, body string, out any) int {
	t.Helper()

	recorder := serve(h, method, path, body)
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), out); err != nil {
		t.Fatalf("decoding %q: %v", recorder.Body.String(), err)
	}
	return recorder.Code
}

// createBody is a valid account request with a unique ID number, followed by
// any extra JSON members.
func createBody(extra string) string {
	return fmt.Sprintf(`{"name": "Ada Lovelace", "address": "12 St James Square",
		"date_of_birth": "1985-12-10", "id_number": "ID%d"%s}`, rand.Uint64(), extra)
}

func createAccount(t *testing.T, h http.Handler, initialBalance int64) accountResponse {
	t.Helper()

	extra := ""
	if initialBalance != 0 {
		extra = fmt.Sprintf(`, "initial_balance": %d`, initialBalance)
	}

	var account accountResponse
	if status := send(t, h, http.MethodPost, "/accounts", createBody(extra), &account); status != http.StatusCreated {
		t.Fatalf("create account status = %d, want %d", status, http.StatusCreated)
	}
	return account
}

func TestMoneyFlow(t *testing.T) {
	h := newTestHandler(t)

	alice := createAccount(t, h, 100)
	bob := createAccount(t, h, 0)
	if alice.Balance != 100 || bob.Balance != 0 {
		t.Fatalf("initial balances = %d, %d, want 100, 0", alice.Balance, bob.Balance)
	}

	var deposited accountResponse
	status := send(t, h, http.MethodPost, "/accounts/"+alice.ID+"/deposits", `{"amount": 50}`, &deposited)
	if status != http.StatusOK || deposited.Balance != 150 {
		t.Fatalf("deposit = %d with balance %d, want %d with balance 150", status, deposited.Balance, http.StatusOK)
	}

	var transferred transferResponse
	body := `{"from": "` + alice.ID + `", "to": "` + bob.ID + `", "amount": 120}`
	status = send(t, h, http.MethodPost, "/transfers", body, &transferred)
	if status != http.StatusOK {
		t.Fatalf("transfer status = %d, want %d", status, http.StatusOK)
	}
	if transferred.From.Balance != 30 || transferred.To.Balance != 120 {
		t.Errorf("transfer balances = %d, %d, want 30, 120", transferred.From.Balance, transferred.To.Balance)
	}

	var fetched accountResponse
	status = send(t, h, http.MethodGet, "/accounts/"+bob.ID, "", &fetched)
	if status != http.StatusOK || fetched != transferred.To {
		t.Errorf("get account = %d %+v, want %d %+v", status, fetched, http.StatusOK, transferred.To)
	}

	var ledger ledgerResponse
	status = send(t, h, http.MethodGet, "/accounts/"+alice.ID+"/ledger", "", &ledger)
	if status != http.StatusOK {
		t.Fatalf("ledger status = %d, want %d", status, http.StatusOK)
	}
	if len(ledger.Entries) != 3 {
		t.Fatalf("ledger has %d entries, want 3", len(ledger.Entries))
	}
	newest := ledger.Entries[0]
	if newest.Type != "transfer" || newest.Amount != -120 || newest.BalanceAfter != 30 || newest.CounterpartyAccountID != bob.ID {
		t.Errorf("newest entry = %+v, want a -120 transfer to %s leaving 30", newest, bob.ID)
	}
}

func TestEmptyLedgerIsAnEmptyList(t *testing.T) {
	h := newTestHandler(t)
	account := createAccount(t, h, 0)

	response := serve(h, http.MethodGet, "/accounts/"+account.ID+"/ledger", "")

	if got, want := response.Body.String(), `{"entries":[]}`; got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestResponsesDoNotExposeHolderDetails(t *testing.T) {
	h := newTestHandler(t)
	idNumber := fmt.Sprintf("PRIVATE%d", rand.Uint64())
	body := `{"name": "Ada Lovelace", "address": "12 St James Square",
		"date_of_birth": "1985-12-10", "id_number": "` + idNumber + `"}`

	created := serve(h, http.MethodPost, "/accounts", body)
	location := created.Header().Get("Location")
	fetched := serve(h, http.MethodGet, location, "")

	for _, response := range []string{created.Body.String(), fetched.Body.String()} {
		for _, secret := range []string{"Ada", "James", "1985", idNumber} {
			if strings.Contains(response, secret) {
				t.Errorf("response %s exposes %q", response, secret)
			}
		}
	}
}

func TestCreateAccountRejectsSecondAccountForSameIDNumber(t *testing.T) {
	h := newTestHandler(t)
	body := createBody("")

	if first := serve(h, http.MethodPost, "/accounts", body); first.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want %d", first.Code, http.StatusCreated)
	}

	var response errorResponse
	if status := send(t, h, http.MethodPost, "/accounts", body, &response); status != http.StatusConflict {
		t.Errorf("second create status = %d, want %d", status, http.StatusConflict)
	}
	if want := "an account already exists for this ID number"; response.Error != want {
		t.Errorf("error = %q, want %q", response.Error, want)
	}
}

func TestCreateAccountRejectsSimilarHolder(t *testing.T) {
	h := newTestHandler(t)
	id := fmt.Sprintf("ID%d", rand.Uint64())
	body := func(name, idNumber string) string {
		return `{"name": "` + name + `", "address": "12 St James Square",
			"date_of_birth": "1985-12-10", "id_number": "` + idNumber + `"}`
	}

	if first := serve(h, http.MethodPost, "/accounts", body("John Smith", id)); first.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want %d", first.Code, http.StatusCreated)
	}

	var response errorResponse
	if status := send(t, h, http.MethodPost, "/accounts", body("Jon Smith", id+"Z"), &response); status != http.StatusConflict {
		t.Errorf("similar create status = %d, want %d", status, http.StatusConflict)
	}
	if want := "an account already exists for a similar holder"; response.Error != want {
		t.Errorf("error = %q, want %q", response.Error, want)
	}
}

func TestCreateAccountLocationPointsAtAccount(t *testing.T) {
	h := newTestHandler(t)

	created := serve(h, http.MethodPost, "/accounts", createBody(""))
	location := created.Header().Get("Location")

	if fetched := serve(h, http.MethodGet, location, ""); fetched.Code != http.StatusOK {
		t.Errorf("GET %q status = %d, want %d", location, fetched.Code, http.StatusOK)
	}
}

func TestErrorResponses(t *testing.T) {
	h := newTestHandler(t)
	funded := createAccount(t, h, 100).ID
	empty := createAccount(t, h, 0).ID
	deposits := "/accounts/" + funded + "/deposits"
	transfer := func(from, to string, amount string) string {
		return `{"from": "` + from + `", "to": "` + to + `", "amount": ` + amount + `}`
	}
	holder := func(name, address, dateOfBirth, idNumber string) string {
		return `{"name": "` + name + `", "address": "` + address +
			`", "date_of_birth": "` + dateOfBirth + `", "id_number": "` + idNumber + `"}`
	}
	badDate := "malformed request body: invalid field /date_of_birth"

	const (
		post = http.MethodPost
		get  = http.MethodGet
	)
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantError  string
	}{
		{"create empty body", post, "/accounts", "", 400, "malformed request body: body is empty"},
		{"create empty object", post, "/accounts", `{}`, 400, "invalid account holder: name is required"},
		{"blank name", post, "/accounts", holder("   ", "a", "1985-12-10", "X1"), 400, "invalid account holder: name is required"},
		{"name with NUL", post, "/accounts", holder(`a\u0000b`, "a", "1985-12-10", "X1"), 400, "invalid account holder: name contains invalid characters"},
		{"blank address", post, "/accounts", holder("A", " ", "1985-12-10", "X1"), 400, "invalid account holder: address is required"},
		{"blank ID number", post, "/accounts", holder("A", "a", "1985-12-10", " - "), 400, "invalid account holder: ID number is required"},
		{"missing date of birth", post, "/accounts", `{"name": "A", "address": "a", "id_number": "X1"}`, 400, "invalid account holder: date of birth is required"},
		{"date in wrong format", post, "/accounts", holder("A", "a", "10/12/1985", "X1"), 400, badDate},
		{"impossible date", post, "/accounts", holder("A", "a", "2024-02-30", "X1"), 400, badDate},
		{"future date of birth", post, "/accounts", holder("A", "a", "2999-01-01", "X1"), 400, "invalid account holder: date of birth must be between 1900 and today"},
		{"ancient date of birth", post, "/accounts", holder("A", "a", "1899-12-31", "X1"), 400, "invalid account holder: date of birth must be between 1900 and today"},
		{"negative initial balance", post, "/accounts", createBody(`, "initial_balance": -1`), 400, "initial balance must not be negative"},
		{"fractional amount", post, "/accounts", createBody(`, "initial_balance": 10.5`), 400, "malformed request body: invalid field /initial_balance"},
		{"string amount", post, deposits, `{"amount": "1"}`, 400, "malformed request body: invalid field /amount"},
		{"amount beyond int64", post, deposits, `{"amount": 9223372036854775808}`, 400, "malformed request body: invalid field /amount"},
		{"unknown field", post, "/accounts", createBody(`, "balance": 1`), 400, "malformed request body: invalid field /balance"},
		{"wrong key case", post, "/accounts", `{"NAME": "A"}`, 400, "malformed request body: invalid field /NAME"},
		{"truncated JSON", post, "/accounts", `{"name":`, 400, "malformed request body"},
		{"duplicate key", post, "/accounts", `{"name": "A", "name": "B"}`, 400, "malformed request body"},
		{"trailing data", post, "/accounts", createBody("") + `{}`, 400, "malformed request body"},
		{"wrong JSON type", post, deposits, `[1]`, 400, "malformed request body"},
		{"oversized body", post, deposits, strings.Repeat(" ", maxBodyBytes+1), 413, "request body is too large"},
		{"get unknown account", get, "/accounts/missing", "", 404, "account not found"},
		{"account ID with NUL", get, "/accounts/%00", "", 404, "account not found"},
		{"account ID with invalid UTF-8", get, "/accounts/%ff", "", 404, "account not found"},
		{"ledger of unknown account", get, "/accounts/missing/ledger", "", 404, "account not found"},
		{"ledger cursor not a number", get, "/accounts/" + funded + "/ledger?before=abc", "", 400, "before must be a positive integer"},
		{"ledger cursor negative", get, "/accounts/" + funded + "/ledger?before=-1", "", 400, "before must be a positive integer"},
		{"deposit empty body", post, deposits, "", 400, "malformed request body: body is empty"},
		{"deposit zero", post, deposits, `{"amount": 0}`, 400, "amount must be positive"},
		{"deposit negative", post, deposits, `{"amount": -5}`, 400, "amount must be positive"},
		{"deposit unknown account", post, "/accounts/missing/deposits", `{"amount": 1}`, 404, "account not found"},
		{"deposit to invalid ID", post, "/accounts/%00/deposits", `{"amount": 1}`, 404, "account not found"},
		{"transfer empty object", post, "/transfers", `{}`, 400, "from and to are required"},
		{"transfer missing destination", post, "/transfers", `{"from": "` + funded + `", "amount": 1}`, 400, "from and to are required"},
		{"transfer missing amount", post, "/transfers", `{"from": "` + funded + `", "to": "` + empty + `"}`, 400, "amount must be positive"},
		{"transfer to self", post, "/transfers", transfer(funded, funded, "1"), 400, "source and destination accounts must differ"},
		{"transfer unknown account", post, "/transfers", transfer(funded, "missing", "1"), 404, "account not found"},
		{"transfer from invalid ID", post, "/transfers", transfer(`\u0000`, funded, "1"), 404, "account not found"},
		{"transfer overdraft", post, "/transfers", transfer(empty, funded, "1"), 409, "insufficient funds"},
		{"unknown route", get, "/nope", "", 404, "Not Found"},
		{"wrong method", get, "/transfers", "", 405, "Method Not Allowed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var response errorResponse
			if status := send(t, h, tt.method, tt.path, tt.body, &response); status != tt.wantStatus {
				t.Errorf("status = %d, want %d", status, tt.wantStatus)
			}
			if response.Error != tt.wantError {
				t.Errorf("error = %q, want %q", response.Error, tt.wantError)
			}
		})
	}
}

func TestMethodNotAllowedListsAllowedMethods(t *testing.T) {
	response := serve(NewHandler(stubBank{}), http.MethodDelete, "/transfers", "")

	if got := response.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want %q", got, http.MethodPost)
	}
}

// stubBank implements only Account; calling any other method panics.
type stubBank struct {
	Bank
	account func(context.Context) (bank.Account, error)
}

func (s stubBank) Account(ctx context.Context, _ string) (bank.Account, error) {
	return s.account(ctx)
}

func TestInfrastructureErrorsDoNotLeakDetails(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"unexpected", errors.New("connection to 10.0.0.1 refused"), http.StatusInternalServerError},
		{"timed out", context.DeadlineExceeded, http.StatusServiceUnavailable},
		{"canceled", context.Canceled, http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHandler(stubBank{account: func(context.Context) (bank.Account, error) {
				return bank.Account{}, tt.err
			}})

			var response errorResponse
			status := send(t, h, http.MethodGet, "/accounts/any", "", &response)

			if status != tt.wantStatus {
				t.Errorf("status = %d, want %d", status, tt.wantStatus)
			}
			if response.Error != http.StatusText(tt.wantStatus) {
				t.Errorf("error = %q, want the generic status text", response.Error)
			}
		})
	}
}

func TestBankCallsCarryRequestDeadline(t *testing.T) {
	var hasDeadline bool
	h := NewHandler(stubBank{account: func(ctx context.Context) (bank.Account, error) {
		_, hasDeadline = ctx.Deadline()
		return bank.Account{}, nil
	}})

	serve(h, http.MethodGet, "/accounts/any", "")

	if !hasDeadline {
		t.Error("the bank call's context has no deadline")
	}
}
