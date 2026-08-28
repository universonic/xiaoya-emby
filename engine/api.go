package engine

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// controlBodyLimit bounds JSON request bodies of the control API.
const controlBodyLimit = 64 << 10

// csrfHeader must be present on every state-changing request. It is not a
// standard simple header, so cross-site form posts cannot satisfy it.
const csrfHeader = "X-Requested-With"
const csrfValue = "xiaoya-emby"

// API error codes surfaced in the JSON error envelope.
const (
	errCodeBadRequest       = "bad_request"
	errCodeForbidden        = "forbidden"
	errCodeMethodNotAllowed = "method_not_allowed"
	errCodeUnsupportedMedia = "unsupported_media_type"
	errCodeRequestTooLarge  = "request_too_large"
	errCodeControlOff       = "control_unavailable"
	errCodeInternal         = "internal"
	errCodeInvalidMode      = "invalid_mode"
	errCodeConfirmRequired  = "confirm_required"
	errCodeBusy             = "busy"
	errCodeModeConflict     = "mode_conflict"
	errCodeRecoveryPaused   = "recovery_paused"
	errCodeNoActiveJob      = "no_active_job"
	errCodeRevisionConflict = "revision_conflict"
	errCodeInvalidSettings  = "invalid_settings"
)

// controlPlane bundles the controller, the settings store and the auth
// policy for the HTTP handlers. A nil controller marks the control
// capability unavailable (non-daemon mode).
type controlPlane struct {
	ctrl  *Controller
	store *SettingsStore
	token string
}

func newControlPlane(ctrl *Controller, store *SettingsStore, token string) *controlPlane {
	return &controlPlane{ctrl: ctrl, store: store, token: token}
}

// readOnly reports whether the deployment forbids control operations: no
// control token configured.
func (cp *controlPlane) readOnly() bool {
	return cp.token == ""
}

// authorized authenticates a control request. Mutations always require the
// configured bearer token; the peer address is never trusted, because a
// same-host reverse proxy would otherwise turn a loopback-bound status
// page into an unauthenticated control endpoint for its remote users.
func (cp *controlPlane) authorized(r *http.Request) bool {
	if cp == nil || cp.ctrl == nil || cp.token == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	provided := []byte(strings.TrimSpace(strings.TrimPrefix(auth, prefix)))
	return subtle.ConstantTimeCompare(provided, []byte(cp.token)) == 1
}

// writeAuthorized enforces the CSRF requirement for state-changing
// requests (custom header that cross-site form posts cannot set).
func writeAuthorized(r *http.Request) bool {
	return r.Header.Get(csrfHeader) == csrfValue
}

type jsonErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type jsonErrorEnvelope struct {
	Error jsonErrorBody `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, jsonErrorEnvelope{Error: jsonErrorBody{Code: code, Message: msg}})
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeJSONError(w, http.StatusMethodNotAllowed, errCodeMethodNotAllowed, "method "+r.Method+" is not allowed")
	return false
}

// decodeJSONBody enforces the content type, size limit and strict field
// set of a control request body.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.TrimSpace(strings.ToLower(ct)), "application/json") {
		writeJSONError(w, http.StatusUnsupportedMediaType, errCodeUnsupportedMedia, "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, controlBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, errCodeRequestTooLarge, "request body exceeds the limit")
			return false
		}
		writeJSONError(w, http.StatusBadRequest, errCodeBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// statusHTTPHandler builds the HTTP routes of the status/control page. The
// controlPlane may be nil (status-only, control reported unavailable).
func statusHTTPHandler(cp *controlPlane) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(globalStatus.snapshot())
	})

	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		after := int64(0)
		if s := r.URL.Query().Get("after"); s != "" {
			fmt.Sscanf(s, "%d", &after)
		}
		var records []logRecord
		if statusRingLog != nil {
			records = statusRingLog.logsAfter(after)
		}
		if records == nil {
			records = []logRecord{}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"records": records})
	})

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if cp == nil || cp.store == nil {
			writeJSONError(w, http.StatusServiceUnavailable, errCodeControlOff, "control plane is not available")
			return
		}
		switch r.Method {
		case http.MethodGet:
			if cp.ctrl == nil {
				writeJSONError(w, http.StatusServiceUnavailable, errCodeControlOff, "control plane is not available")
				return
			}
			if !cp.authorized(r) {
				writeJSONError(w, http.StatusForbidden, errCodeForbidden, forbiddenMessage(cp))
				return
			}
			cp.handleConfigGet(w)
		case http.MethodPut:
			if !cp.requireWrite(w, r) {
				return
			}
			cp.handleConfigPut(w, r)
		case http.MethodDelete:
			if !cp.requireWrite(w, r) {
				return
			}
			cp.handleConfigDelete(w, r)
		default:
			w.Header().Set("Allow", "GET, PUT, DELETE")
			writeJSONError(w, http.StatusMethodNotAllowed, errCodeMethodNotAllowed, "method "+r.Method+" is not allowed")
		}
	})

	mux.HandleFunc("/api/sync", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !cp.requireWrite(w, r) {
			return
		}
		var req syncRequest
		if !decodeJSONBody(w, r, &req) {
			return
		}
		switch req.Mode {
		case SyncTypeIncremental, SyncTypeFullRelaxed, SyncTypeFullStrict:
		default:
			writeJSONError(w, http.StatusBadRequest, errCodeInvalidMode, "mode must be one of incremental, full-relaxed, full-strict")
			return
		}
		if req.Mode == SyncTypeFullStrict && !req.Confirm {
			writeJSONError(w, http.StatusBadRequest, errCodeConfirmRequired, "strict full rebuild requires confirm: true")
			return
		}
		jobID, effective, err := cp.ctrl.Trigger(req.Mode, req.Confirm)
		if err != nil {
			cp.writeTriggerError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"job_id":         jobID,
			"mode":           req.Mode,
			"effective_mode": effective,
		})
	})

	mux.HandleFunc("/api/sync/abort", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !cp.requireWrite(w, r) {
			return
		}
		accepted, cancelRequested, err := cp.ctrl.Abort()
		if err != nil {
			cp.writeTriggerError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"accepted":         accepted,
			"cancel_requested": cancelRequested,
		})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		body, err := statusWebFS.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, "status page unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	})
	return mux
}

func forbiddenMessage(cp *controlPlane) string {
	if cp.readOnly() {
		return "control requires a --control-token (CONTROL_TOKEN); without one the interface is read-only"
	}
	return "control requires a valid Bearer token"
}

// requireWrite performs the full write-request gate: availability, token,
// CSRF header.
func (cp *controlPlane) requireWrite(w http.ResponseWriter, r *http.Request) bool {
	if cp == nil || cp.ctrl == nil {
		writeJSONError(w, http.StatusServiceUnavailable, errCodeControlOff, "control plane is not available")
		return false
	}
	if !cp.authorized(r) {
		writeJSONError(w, http.StatusForbidden, errCodeForbidden, forbiddenMessage(cp))
		return false
	}
	if !writeAuthorized(r) {
		writeJSONError(w, http.StatusForbidden, errCodeForbidden, "write requests must set "+csrfHeader+": "+csrfValue)
		return false
	}
	return true
}

type configResponse struct {
	Revision     int64        `json:"revision"`
	Persisted    bool         `json:"persisted"`
	PersistError string       `json:"persist_error,omitempty"`
	Settings     SyncSettings `json:"settings"`
	Baseline     SyncSettings `json:"baseline"`
}

func (cp *controlPlane) configResponse() configResponse {
	settings, rev := cp.store.Snapshot()
	return configResponse{
		Revision:     rev,
		Persisted:    cp.store.Persisted(),
		PersistError: cp.store.PersistError(),
		Settings:     settings,
		Baseline:     cp.store.Baseline(),
	}
}

func (cp *controlPlane) handleConfigGet(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, cp.configResponse())
}

type configPutRequest struct {
	Revision int64        `json:"revision"`
	Settings SyncSettings `json:"settings"`
}

func (cp *controlPlane) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	var req configPutRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	settings, rev, err := cp.store.Update(req.Revision, req.Settings)
	if err != nil {
		switch {
		case errors.Is(err, errRevisionConflict):
			writeJSONError(w, http.StatusConflict, errCodeRevisionConflict,
				fmt.Sprintf("revision %d is stale; the current revision is %d", req.Revision, cp.store.Revision()))
		default:
			writeJSONError(w, http.StatusBadRequest, errCodeInvalidSettings, err.Error())
		}
		return
	}
	globalStatus.setConfig(rev, cp.store.PersistError() != "")
	slog.Info("Sync settings updated via control API", "revision", rev)
	writeJSON(w, http.StatusOK, configResponse{
		Revision:     rev,
		Persisted:    cp.store.Persisted(),
		PersistError: cp.store.PersistError(),
		Settings:     settings,
		Baseline:     cp.store.Baseline(),
	})
}

func (cp *controlPlane) handleConfigDelete(w http.ResponseWriter, r *http.Request) {
	settings, rev, err := cp.store.Reset()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, errCodeInternal, err.Error())
		return
	}
	globalStatus.setConfig(rev, cp.store.PersistError() != "")
	slog.Info("Sync settings override removed; startup baseline restored", "revision", rev)
	writeJSON(w, http.StatusOK, configResponse{
		Revision:     rev,
		Persisted:    cp.store.Persisted(),
		PersistError: cp.store.PersistError(),
		Settings:     settings,
		Baseline:     cp.store.Baseline(),
	})
}

type syncRequest struct {
	Mode    string `json:"mode"`
	Confirm bool   `json:"confirm"`
}

func (cp *controlPlane) writeTriggerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBusy):
		writeJSONError(w, http.StatusConflict, errCodeBusy, err.Error())
	case errors.Is(err, errModeConflict):
		writeJSONError(w, http.StatusConflict, errCodeModeConflict, errModeConflict.Error())
	case errors.Is(err, errRecoveryPaused):
		writeJSONError(w, http.StatusConflict, errCodeRecoveryPaused, errRecoveryPaused.Error())
	case errors.Is(err, errConfirm):
		writeJSONError(w, http.StatusBadRequest, errCodeConfirmRequired, errConfirm.Error())
	case errors.Is(err, errNoActiveJob):
		writeJSONError(w, http.StatusConflict, errCodeNoActiveJob, errNoActiveJob.Error())
	case errors.Is(err, errControlUnavailable):
		writeJSONError(w, http.StatusServiceUnavailable, errCodeControlOff, err.Error())
	default:
		var kindErr error
		if strings.Contains(err.Error(), "unknown sync type") {
			kindErr = err
			writeJSONError(w, http.StatusBadRequest, errCodeInvalidMode, kindErr.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, errCodeInternal, err.Error())
	}
}
