// Package handler is the HTTP transport layer: decode, validate shape, call the service, map errors.
package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"wallet-transfer-service/process"
)

type Handler struct {
	svc *process.TransferService
	log *slog.Logger
}

func New(svc *process.TransferService, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /transfers", h.createTransfer)
	mux.HandleFunc("GET /transfers/{id}", h.getTransfer)
	mux.HandleFunc("POST /wallets", h.createWallet)
	mux.HandleFunc("GET /wallets/{id}", h.getWallet)
	return h.logRequests(mux)
}

// ---- DTOs ----

type createTransferRequest struct {
	IdempotencyKey string `json:"idempotencyKey"`
	FromWalletID   string `json:"fromWalletId"`
	ToWalletID     string `json:"toWalletId"`
	Amount         int64  `json:"amount"`
}

type transferResponse struct {
	ID             string    `json:"id"`
	IdempotencyKey string    `json:"idempotencyKey,omitempty"`
	FromWalletID   string    `json:"fromWalletId"`
	ToWalletID     string    `json:"toWalletId"`
	Amount         int64     `json:"amount"`
	Status         string    `json:"status"`
	FailureReason  string    `json:"failureReason,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type createWalletRequest struct {
	ID             string `json:"id"`
	InitialBalance int64  `json:"initialBalance"`
}

type walletResponse struct {
	ID        string    `json:"id"`
	Balance   int64     `json:"balance"`
	CreatedAt time.Time `json:"createdAt"`
}

func toTransferResponse(t *process.Transfer) transferResponse {
	return transferResponse{
		ID: t.ID, IdempotencyKey: t.IdempotencyKey, FromWalletID: t.FromWalletID, ToWalletID: t.ToWalletID,
		Amount: t.Amount, Status: string(t.Status), FailureReason: t.FailureReason,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

// ---- handlers ----

func (h *Handler) createTransfer(w http.ResponseWriter, r *http.Request) {
	var req createTransferRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be valid JSON with known fields")
		return
	}
	res, err := h.svc.CreateTransfer(r.Context(), process.CreateTransferInput{
		IdempotencyKey: req.IdempotencyKey,
		FromWalletID:   req.FromWalletID,
		ToWalletID:     req.ToWalletID,
		Amount:         req.Amount,
	})
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}

	status := http.StatusCreated
	switch {
	case res.Transfer.Status == process.StatusFailed:
		status = http.StatusUnprocessableEntity // same code on first call and on every replay
	case res.Replayed:
		status = http.StatusOK
	}
	if res.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, status, toTransferResponse(res.Transfer))
}

func (h *Handler) getTransfer(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.GetTransfer(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toTransferResponse(t))
}

func (h *Handler) createWallet(w http.ResponseWriter, r *http.Request) {
	var req createWalletRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be valid JSON with known fields")
		return
	}
	wallet, err := h.svc.CreateWallet(r.Context(), req.ID, req.InitialBalance)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, walletResponse{ID: wallet.ID, Balance: wallet.Balance, CreatedAt: wallet.CreatedAt})
}

func (h *Handler) getWallet(w http.ResponseWriter, r *http.Request) {
	wallet, err := h.svc.GetWallet(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, walletResponse{ID: wallet.ID, Balance: wallet.Balance, CreatedAt: wallet.CreatedAt})
}

// ---- helpers ----

var errorMap = []struct {
	err    error
	status int
	code   string
}{
	{process.ErrInvalidAmount, http.StatusBadRequest, "invalid_amount"},
	{process.ErrMissingWalletID, http.StatusBadRequest, "missing_wallet_id"},
	{process.ErrSameWallet, http.StatusBadRequest, "same_wallet"},
	{process.ErrInvalidIdempotencyKey, http.StatusBadRequest, "invalid_idempotency_key"},
	{process.ErrInvalidWallet, http.StatusBadRequest, "invalid_wallet"},
	{process.ErrWalletNotFound, http.StatusNotFound, "wallet_not_found"},
	{process.ErrTransferNotFound, http.StatusNotFound, "transfer_not_found"},
	{process.ErrWalletExists, http.StatusConflict, "wallet_exists"},
	{process.ErrIdempotencyKeyReuse, http.StatusUnprocessableEntity, "idempotency_key_reuse"},
}

func (h *Handler) writeDomainError(w http.ResponseWriter, r *http.Request, err error) {
	for _, m := range errorMap {
		if errors.Is(err, m.err) {
			writeError(w, m.status, m.code, err.Error())
			return
		}
	}
	h.log.ErrorContext(r.Context(), "unhandled error", "error", err, "path", r.URL.Path)
	writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (h *Handler) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		h.log.InfoContext(r.Context(), "http request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	})
}
