// Package api exposes a Bank over HTTP as JSON.
package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"sumup/api/internal/bank"
)

const (
	maxBodyBytes   = 1 << 16
	requestTimeout = 5 * time.Second
)

var (
	errMalformedBody     = errors.New("malformed request body")
	errBodyTooLarge      = errors.New("request body is too large")
	errMissingAccountIDs = errors.New("from and to are required")
	errInvalidCursor     = errors.New("before must be a positive integer")
)

// Bank is the set of banking operations the API depends on.
type Bank interface {
	CreateAccount(ctx context.Context, holder bank.Holder, initialBalance int64) (bank.Account, error)
	Account(ctx context.Context, id string) (bank.Account, error)
	Ledger(ctx context.Context, accountID string, before int64) (entries []bank.LedgerEntry, nextBefore int64, err error)
	Deposit(ctx context.Context, id string, amount int64) (bank.Account, error)
	Transfer(ctx context.Context, fromID, toID string, amount int64) (from, to bank.Account, err error)
}

// NewHandler returns an http.Handler that serves the API backed by b.
func NewHandler(b Bank) http.Handler {
	h := handler{bank: b}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /accounts", h.createAccount)
	mux.HandleFunc("GET /accounts/{id}", h.getAccount)
	mux.HandleFunc("GET /accounts/{id}/ledger", h.getLedger)
	mux.HandleFunc("POST /accounts/{id}/deposits", h.deposit)
	mux.HandleFunc("POST /transfers", h.transfer)

	return withTimeout(withJSONRouteErrors(mux))
}

// withTimeout bounds each request, including any wait for a row lock. A
// transaction cut off by the deadline rolls back.
func withTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withJSONRouteErrors gives the router's own 404 and 405 responses the same
// JSON shape as every other error.
func withJSONRouteErrors(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			w = routeErrorWriter{w}
		}
		mux.ServeHTTP(w, r)
	})
}

// routeErrorWriter replaces the plain-text body of a router error. Headers the
// router sets, such as Allow, pass through.
type routeErrorWriter struct {
	http.ResponseWriter
}

func (w routeErrorWriter) WriteHeader(status int) {
	writeJSON(w.ResponseWriter, status, errorResponse{Error: http.StatusText(status)})
}

func (w routeErrorWriter) Write(body []byte) (int, error) {
	return len(body), nil
}

type handler struct {
	bank Bank
}

type createAccountRequest struct {
	Name           string `json:"name"`
	Address        string `json:"address"`
	DateOfBirth    string `json:"date_of_birth"`
	IDNumber       string `json:"id_number"`
	InitialBalance int64  `json:"initial_balance"`
}

type depositRequest struct {
	Amount int64 `json:"amount"`
}

type transferRequest struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Amount int64  `json:"amount"`
}

// accountResponse deliberately omits the holder's personal details: the API
// has no authentication, so anyone holding an account ID can read this.
type accountResponse struct {
	ID      string `json:"id"`
	Balance int64  `json:"balance"`
}

type transferResponse struct {
	From accountResponse `json:"from"`
	To   accountResponse `json:"to"`
}

type ledgerEntryResponse struct {
	ID                    int64     `json:"id"`
	TransactionID         string    `json:"transaction_id"`
	Type                  string    `json:"type"`
	Amount                int64     `json:"amount"`
	BalanceAfter          int64     `json:"balance_after"`
	CounterpartyAccountID string    `json:"counterparty_account_id,omitzero"`
	CreatedAt             time.Time `json:"created_at"`
}

type ledgerResponse struct {
	Entries    []ledgerEntryResponse `json:"entries"`
	NextBefore int64                 `json:"next_before,omitzero"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func newAccountResponse(account bank.Account) accountResponse {
	return accountResponse{ID: account.ID, Balance: account.Balance}
}

func newLedgerResponse(entries []bank.LedgerEntry, nextBefore int64) ledgerResponse {
	response := ledgerResponse{
		Entries:    make([]ledgerEntryResponse, len(entries)),
		NextBefore: nextBefore,
	}
	for i, entry := range entries {
		response.Entries[i] = ledgerEntryResponse{
			ID:                    entry.ID,
			TransactionID:         entry.TransactionID,
			Type:                  string(entry.Type),
			Amount:                entry.Amount,
			BalanceAfter:          entry.BalanceAfter,
			CounterpartyAccountID: entry.CounterpartyID,
			CreatedAt:             entry.CreatedAt,
		}
	}
	return response
}

func (h handler) createAccount(w http.ResponseWriter, r *http.Request) {
	var req createAccountRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, err)
		return
	}

	dateOfBirth, err := parseDateOfBirth(req.DateOfBirth)
	if err != nil {
		writeError(w, err)
		return
	}

	holder := bank.Holder{
		Name:        req.Name,
		Address:     req.Address,
		DateOfBirth: dateOfBirth,
		IDNumber:    req.IDNumber,
	}
	account, err := h.bank.CreateAccount(r.Context(), holder, req.InitialBalance)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Location", "/accounts/"+account.ID)
	writeJSON(w, http.StatusCreated, newAccountResponse(account))
}

func (h handler) getAccount(w http.ResponseWriter, r *http.Request) {
	account, err := h.bank.Account(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newAccountResponse(account))
}

func (h handler) getLedger(w http.ResponseWriter, r *http.Request) {
	before, err := parseCursor(r.URL.Query().Get("before"))
	if err != nil {
		writeError(w, err)
		return
	}

	entries, nextBefore, err := h.bank.Ledger(r.Context(), r.PathValue("id"), before)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newLedgerResponse(entries, nextBefore))
}

func (h handler) deposit(w http.ResponseWriter, r *http.Request) {
	var req depositRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, err)
		return
	}

	account, err := h.bank.Deposit(r.Context(), r.PathValue("id"), req.Amount)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newAccountResponse(account))
}

func (h handler) transfer(w http.ResponseWriter, r *http.Request) {
	var req transferRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.From == "" || req.To == "" {
		writeError(w, errMissingAccountIDs)
		return
	}

	from, to, err := h.bank.Transfer(r.Context(), req.From, req.To, req.Amount)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, transferResponse{
		From: newAccountResponse(from),
		To:   newAccountResponse(to),
	})
}

// parseDateOfBirth reads a YYYY-MM-DD date. An absent date stays zero for the
// bank to report as required.
func parseDateOfBirth(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	date, err := time.Parse(time.DateOnly, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: invalid field /date_of_birth", errMalformedBody)
	}
	return date, nil
}

// parseCursor reads the optional before query parameter; absent means zero.
func parseCursor(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	cursor, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || cursor <= 0 {
		return 0, errInvalidCursor
	}
	return cursor, nil
}

// decodeJSON reads exactly one JSON value into dst. Decoder errors are reduced
// to the offending field because their full text names internal Go types.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return errBodyTooLarge
	}
	if err != nil {
		return errMalformedBody
	}
	if len(body) == 0 {
		return fmt.Errorf("%w: body is empty", errMalformedBody)
	}

	err = json.Unmarshal(body, dst, json.RejectUnknownMembers(true))
	var semantic *json.SemanticError
	if errors.As(err, &semantic) && semantic.JSONPointer != "" {
		return fmt.Errorf("%w: invalid field %s", errMalformedBody, semantic.JSONPointer)
	}
	if err != nil {
		return errMalformedBody
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.MarshalWrite(w, body); err != nil {
		slog.Error("writing response", "error", err)
	}
}

func writeError(w http.ResponseWriter, err error) {
	status := statusFor(err)
	message := err.Error()
	if status >= http.StatusInternalServerError {
		slog.Error("handling request", "error", err)
		message = http.StatusText(status) // Internal details stay in the logs.
	}
	writeJSON(w, status, errorResponse{Error: message})
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, errMalformedBody),
		errors.Is(err, errMissingAccountIDs),
		errors.Is(err, errInvalidCursor),
		errors.Is(err, bank.ErrInvalidHolder),
		errors.Is(err, bank.ErrNegativeInitialBalance),
		errors.Is(err, bank.ErrInvalidAmount),
		errors.Is(err, bank.ErrSameAccount):
		return http.StatusBadRequest
	case errors.Is(err, bank.ErrAccountNotFound):
		return http.StatusNotFound
	case errors.Is(err, bank.ErrInsufficientFunds),
		errors.Is(err, bank.ErrBalanceLimitExceeded),
		errors.Is(err, bank.ErrHolderAlreadyRegistered),
		errors.Is(err, bank.ErrSimilarHolderExists):
		return http.StatusConflict
	case errors.Is(err, errBodyTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
