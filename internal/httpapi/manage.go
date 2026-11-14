package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/donaldsteele/rocket/internal/config"
	"github.com/donaldsteele/rocket/internal/history"
)

// Transfers, cancel, history, settings and trusted senders: all loopback
// only (see routes).

// defaultHistoryLimit is what GET /api/history returns without ?limit=.
const defaultHistoryLimit = 100

// handleTransfers lists the transfers in flight in both directions
// (transfer.Info), oldest first. Offers still waiting for a decision here are
// in GET /api/incoming instead.
func (s *Server) handleTransfers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.xfers.List())
}

// handleCancel stops a transfer: POST /api/cancel {transfer_id}. An outgoing
// transfer withdraws its offer or aborts its upload; an incoming one stops
// reading, removes its partial files and answers the sender 410 Gone. An
// incoming offer still waiting for a decision is declined.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TransferID string `json:"transfer_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.TransferID == "" {
		http.Error(w, "need transfer_id", http.StatusBadRequest)
		return
	}
	if s.xfers.Cancel(body.TransferID) {
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	s.mu.Lock()
	po := s.pending[body.TransferID]
	delete(s.pending, body.TransferID)
	s.mu.Unlock()
	if po == nil {
		http.Error(w, "no such transfer", http.StatusNotFound)
		return
	}
	po.reply <- decision{accept: false}
	writeJSON(w, map[string]any{"ok": true, "declined": true})
}

// handleHistory returns finished transfers, newest first: GET
// /api/history?limit= (default 100, at most history.Cap).
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	limit := defaultHistoryLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "limit must be a positive number", http.StatusBadRequest)
			return
		}
		limit = min(n, history.Cap)
	}
	list, err := s.hist.List(limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, list)
}

func (s *Server) handleHistoryClear(w http.ResponseWriter, _ *http.Request) {
	if err := s.hist.Clear(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// settingsView is GET /api/settings and the reply to PUT. DeviceName is the
// saved name, or the running one when none is saved; a changed name is
// announced to peers after a restart (RestartRequired). DownloadDir and
// AutoAcceptTrusted apply at once.
type settingsView struct {
	DeviceName        string `json:"device_name"`
	DownloadDir       string `json:"download_dir"`
	AutoAcceptTrusted bool   `json:"auto_accept_trusted"`
	RestartRequired   bool   `json:"restart_required"`
}

func (s *Server) settingsView() settingsView {
	saved := s.settings.Get()
	v := settingsView{DeviceName: saved.DeviceName, DownloadDir: s.downloadDir(), AutoAcceptTrusted: saved.AutoAcceptTrusted}
	if v.DeviceName == "" {
		v.DeviceName = s.id.Name
	}
	v.RestartRequired = v.DeviceName != s.id.Name
	return v
}

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.settingsView())
}

// handlePutSettings saves settings: PUT /api/settings with any of
// {device_name, download_dir, auto_accept_trusted}; fields left out keep
// their value. An empty device_name or download_dir means the default (the
// hostname, Downloads/rocket). The download folder is created if needed and
// used for the next transfer.
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceName        *string `json:"device_name"`
		DownloadDir       *string `json:"download_dir"`
		AutoAcceptTrusted *bool   `json:"auto_accept_trusted"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad settings: "+err.Error(), http.StatusBadRequest)
		return
	}
	next := s.settings.Get()
	if body.DeviceName != nil {
		next.DeviceName = *body.DeviceName
	}
	if body.DownloadDir != nil {
		next.DownloadDir = *body.DownloadDir
	}
	if body.AutoAcceptTrusted != nil {
		next.AutoAcceptTrusted = *body.AutoAcceptTrusted
	}
	next, err := next.Normalize()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dl := ""
	if body.DownloadDir != nil {
		dl = next.DownloadDir
		if dl == "" {
			if dl, err = filepath.Abs(config.DefaultDownloadDir()); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if err := os.MkdirAll(dl, 0o755); err != nil {
			http.Error(w, "cannot use that download folder: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if _, err := s.settings.Put(next); err != nil {
		http.Error(w, "could not save settings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if dl != "" {
		s.mu.Lock()
		s.dlDir = dl
		s.mu.Unlock()
	}
	writeJSON(w, s.settingsView())
}

// handleTrusted lists the trusted senders, most recently approved first.
func (s *Server) handleTrusted(w http.ResponseWriter, _ *http.Request) {
	list := s.trust.List()
	sort.Slice(list, func(i, j int) bool { return list[i].ApprovedAt.After(list[j].ApprovedAt) })
	writeJSON(w, list)
}

// handleRevoke forgets a trusted sender: POST /api/trusted/revoke
// {fingerprint}. Its next offer prompts again.
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.Fingerprint == "" {
		http.Error(w, "need fingerprint", http.StatusBadRequest)
		return
	}
	if _, ok := s.trust.Get(body.Fingerprint); !ok {
		http.Error(w, "not a trusted sender", http.StatusNotFound)
		return
	}
	if err := s.trust.Revoke(body.Fingerprint); err != nil {
		http.Error(w, "could not save: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
