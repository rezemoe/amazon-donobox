package main

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"
)

type API struct {
	store  *Store
	worker *Worker
	token  string
}

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.Handle("POST /giftcards", a.auth(a.submit))
	// Lookup by code is a POST so codes never end up in URLs and access logs.
	mux.Handle("POST /giftcards/lookup", a.auth(a.lookup))
	mux.Handle("GET /giftcards/{id}", a.auth(a.get))
	mux.Handle("POST /giftcards/{id}/retry", a.auth(a.retry))
	mux.Handle("POST /worker/resume", a.auth(a.resume))
	return mux
}

func (a *API) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.token != "" {
			got := r.Header.Get("Authorization")
			if subtle.ConstantTimeCompare([]byte(got), []byte("Bearer "+a.token)) != 1 {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		next(w, r)
	})
}

type cardResponse struct {
	ID         string          `json:"id"`
	Code       string          `json:"code"`
	State      string          `json:"state"`
	Message    string          `json:"message,omitempty"`
	Attempts   int             `json:"attempts"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	RedeemedAt *time.Time      `json:"redeemed_at,omitempty"`
	Duplicate  bool            `json:"duplicate,omitempty"`
	Events     []eventResponse `json:"events"`
}

type eventResponse struct {
	State     string    `json:"state"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (a *API) writeCard(w http.ResponseWriter, r *http.Request, status int, c *GiftCard, duplicate bool) {
	events, err := a.store.Events(r.Context(), c.ID)
	if err != nil {
		a.internalError(w, err)
		return
	}
	resp := cardResponse{
		ID: c.ID, Code: maskCode(c.Code), State: c.State, Message: c.Message, Attempts: c.Attempts,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, RedeemedAt: c.RedeemedAt,
		Duplicate: duplicate, Events: []eventResponse{},
	}
	for _, e := range events {
		resp.Events = append(resp.Events, eventResponse{State: e.State, Message: e.Message, CreatedAt: e.CreatedAt})
	}
	writeJSON(w, status, resp)
}

type codeRequest struct {
	Code string `json:"code"`
}

func decodeCode(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req codeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON: {\"code\": \"...\"}")
		return "", false
	}
	code := normalizeCode(req.Code)
	if code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return "", false
	}
	return code, true
}

// submit validates a code, stores it and queues it for redemption.
//
//	202 queued, 422 rejected by validation (still stored), 409 code already submitted.
func (a *API) submit(w http.ResponseWriter, r *http.Request) {
	code, ok := decodeCode(w, r)
	if !ok {
		return
	}
	state, msg := StateQueued, ""
	if err := validateCode(code); err != nil {
		state, msg = StateRejected, err.Error()
	}

	card, created, err := a.store.Create(r.Context(), code, state, msg)
	if err != nil {
		a.internalError(w, err)
		return
	}
	switch {
	case !created:
		a.writeCard(w, r, http.StatusConflict, card, true)
	case card.State == StateRejected:
		a.writeCard(w, r, http.StatusUnprocessableEntity, card, false)
	default:
		a.worker.Notify()
		a.writeCard(w, r, http.StatusAccepted, card, false)
	}
}

func (a *API) lookup(w http.ResponseWriter, r *http.Request) {
	code, ok := decodeCode(w, r)
	if !ok {
		return
	}
	card, err := a.store.GetByCode(r.Context(), code)
	a.respondCard(w, r, card, err)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	card, err := a.store.GetByID(r.Context(), r.PathValue("id"))
	a.respondCard(w, r, card, err)
}

func (a *API) respondCard(w http.ResponseWriter, r *http.Request, card *GiftCard, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "gift card not found")
		return
	}
	if err != nil {
		a.internalError(w, err)
		return
	}
	a.writeCard(w, r, http.StatusOK, card, false)
}

// retry re-queues a card that failed or needs review, e.g. after checking the Amazon balance.
func (a *API) retry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ok, err := a.store.Transition(r.Context(), id, []string{StateFailed, StateNeedsReview}, StateQueued, "manually re-queued")
	if err != nil {
		a.internalError(w, err)
		return
	}
	card, err := a.store.GetByID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "gift card not found")
		return
	}
	if err != nil {
		a.internalError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusConflict, "only failed or needs_review cards can be retried, card is "+card.State)
		return
	}
	a.worker.Notify()
	a.writeCard(w, r, http.StatusAccepted, card, false)
}

func (a *API) resume(w http.ResponseWriter, r *http.Request) {
	a.worker.Resume()
	writeJSON(w, http.StatusOK, a.worker.Status())
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "worker": a.worker.Status()})
}

func (a *API) internalError(w http.ResponseWriter, err error) {
	log.Printf("internal error: %v", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
