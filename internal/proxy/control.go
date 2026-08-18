// Handlers in this file are thin adapters over view.Source (spec §9): each
// one parses its HTTP inputs, calls exactly one Source method, and writes the
// result. None of them aggregates, filters, or computes anything a
// view.Source method does not already return — that logic lives once, in
// internal/view, so the TUI and the dashboard reading through the same
// interface can never compute a different answer than this API reports
// (spec invariant 4).
package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/nicko170/aiproxy/internal/view"
)

// controlHandler wraps a func needing view.Source with the same proxy-key
// gate every other endpoint enforces, loopback exempt (see Authorized).
func controlHandler(o HandlerOptions, fn func(view.Source, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !Authorized(r.RemoteAddr, r.Header.Get("x-api-key"), o.APIKey) {
			writeError(w, http.StatusUnauthorized, "authentication_error", "Invalid proxy API key")
			return
		}
		fn(o.View, w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func statusHandler(o HandlerOptions) http.HandlerFunc {
	return controlHandler(o, func(src view.Source, w http.ResponseWriter, r *http.Request) {
		st, err := src.ServerStatus(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		writeJSON(w, st)
	})
}

func accountsHandler(o HandlerOptions) http.HandlerFunc {
	return controlHandler(o, func(src view.Source, w http.ResponseWriter, r *http.Request) {
		accts, err := src.Accounts(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		if accts == nil {
			accts = []view.Account{}
		}
		writeJSON(w, accts)
	})
}

// parseWindow reads the "from" and "to" query parameters shared by every
// window-bounded read route. Both are required: a query silently defaulted to
// "all of history" is an easy way for a chart to look plausible while
// actually answering the wrong question.
func parseWindow(r *http.Request) (view.Window, bool) {
	from, ok1 := parseInt64Param(r, "from")
	to, ok2 := parseInt64Param(r, "to")
	if !ok1 || !ok2 {
		return view.Window{}, false
	}
	return view.Window{From: from, To: to}, true
}

func parseInt64Param(r *http.Request, name string) (int64, bool) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

func usageHandler(o HandlerOptions) http.HandlerFunc {
	return controlHandler(o, func(src view.Source, w http.ResponseWriter, r *http.Request) {
		win, ok := parseWindow(r)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "from and to are required unix-ms query parameters")
			return
		}
		gran := view.Granularity(r.URL.Query().Get("granularity"))
		if gran == "" {
			gran = view.GranularityHour
		}
		if gran != view.GranularityMinute && gran != view.GranularityHour {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "granularity must be \"minute\" or \"hour\"")
			return
		}
		groupBy := view.GroupBy(r.URL.Query().Get("groupBy"))
		switch groupBy {
		case view.GroupByAccount, view.GroupByModel, view.GroupByOutcome:
		default:
			writeError(w, http.StatusBadRequest, "invalid_request_error",
				"groupBy must be \"account\", \"model\", or \"outcome\"")
			return
		}

		series, err := src.UsageSeries(r.Context(), view.SeriesQuery{Window: win, Granularity: gran, GroupBy: groupBy})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		writeJSON(w, series)
	})
}

func totalsHandler(o HandlerOptions) http.HandlerFunc {
	return controlHandler(o, func(src view.Source, w http.ResponseWriter, r *http.Request) {
		win, ok := parseWindow(r)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "from and to are required unix-ms query parameters")
			return
		}
		t, err := src.Totals(r.Context(), win)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		writeJSON(w, t)
	})
}

func latencyHandler(o HandlerOptions) http.HandlerFunc {
	return controlHandler(o, func(src view.Source, w http.ResponseWriter, r *http.Request) {
		win, ok := parseWindow(r)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "from and to are required unix-ms query parameters")
			return
		}
		l, err := src.LatencyPercentiles(r.Context(), win)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		writeJSON(w, l)
	})
}

func quotaHistoryHandler(o HandlerOptions) http.HandlerFunc {
	return controlHandler(o, func(src view.Source, w http.ResponseWriter, r *http.Request) {
		accountID := r.URL.Query().Get("account")
		if accountID == "" {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "account is a required query parameter")
			return
		}
		win, ok := parseWindow(r)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "from and to are required unix-ms query parameters")
			return
		}
		pts, err := src.AccountQuotaHistory(r.Context(), accountID, win)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		if pts == nil {
			pts = []view.QuotaPoint{}
		}
		writeJSON(w, pts)
	})
}

// eventsHandler streams live request activity over SSE. Each event is
// flushed as it arrives, matching the relay's own "never buffer a whole
// response" rule (§4.6) — a live activity feed that batches would defeat its
// own purpose.
func eventsHandler(o HandlerOptions) http.HandlerFunc {
	return controlHandler(o, func(src view.Source, w http.ResponseWriter, r *http.Request) {
		ch, err := src.Subscribe(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		// Flush immediately: net/http otherwise buffers the header block until
		// the first Write, so a subscriber that connects before any event
		// exists would hang waiting for response headers that never arrive.
		if flusher != nil {
			flusher.Flush()
		}
		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				raw, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				if _, err := w.Write([]byte("data: " + string(raw) + "\n\n")); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	})
}

type enabledBody struct {
	Enabled bool `json:"enabled"`
}

func setAccountEnabledHandler(o HandlerOptions) http.HandlerFunc {
	return controlHandler(o, func(src view.Source, w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var body enabledBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed request body")
			return
		}
		if err := src.SetAccountEnabled(r.Context(), id, body.Enabled); err != nil {
			writeMutationError(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
}

type priorityBody struct {
	Priority int `json:"priority"`
}

func setAccountPriorityHandler(o HandlerOptions) http.HandlerFunc {
	return controlHandler(o, func(src view.Source, w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var body priorityBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed request body")
			return
		}
		if err := src.SetPriority(r.Context(), id, body.Priority); err != nil {
			writeMutationError(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
}

func removeAccountHandler(o HandlerOptions) http.HandlerFunc {
	return controlHandler(o, func(src view.Source, w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := src.RemoveAccount(r.Context(), id); err != nil {
			writeMutationError(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
}

func settingsHandler(o HandlerOptions) http.HandlerFunc {
	return controlHandler(o, func(src view.Source, w http.ResponseWriter, r *http.Request) {
		s, err := src.Settings(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		writeJSON(w, s)
	})
}

func updateSettingsHandler(o HandlerOptions) http.HandlerFunc {
	return controlHandler(o, func(src view.Source, w http.ResponseWriter, r *http.Request) {
		var s view.Settings
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed request body")
			return
		}
		applied, err := src.UpdateSettings(r.Context(), s)
		if err != nil {
			// Validate is the only reason UpdateSettings fails on well-formed
			// JSON (a config-store write failure is an internal error), so this
			// route only ever sees a validation error here — reported as 400.
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		// applied.Live / applied.NeedsRestart tell the caller which of the
		// fields it just sent actually took effect versus which were persisted
		// but require a restart (see view.Applied's doc comment) — returned as
		// data so a settings screen cannot mistake a pending field for an
		// applied one.
		writeJSON(w, applied)
	})
}

// writeMutationError reports an unknown-account-id error as 404: the caller
// named something that does not exist, not a proxy fault. Every other error a
// mutation on Source (SetAccountEnabled, SetPriority, RemoveAccount) can
// return — a config-store write failure, for instance — is reported as 500
// instead: it is not the caller's fault and pretending it is a 404 would tell
// a client the account still doesn't exist when the real problem was the
// store.
func writeMutationError(w http.ResponseWriter, err error) {
	if errors.Is(err, view.ErrUnknownAccount) {
		writeError(w, http.StatusNotFound, "not_found_error", err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
}
