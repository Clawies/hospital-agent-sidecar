package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"github.com/clawies/hospital-agent/internal/collector"
	"github.com/clawies/hospital-agent/internal/config"
	"github.com/clawies/hospital-agent/internal/repair"
	"github.com/clawies/hospital-agent/internal/watcher"
)

type Handlers struct {
	Config    *config.Config
	Version   string
	Logger    *slog.Logger
	Watcher   *watcher.Watcher
	Repairer  *repair.Repairer
	Collector *collector.Collector
}

func (h *Handlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.handleHealthz)
	mux.HandleFunc("GET /health", h.handleHealth)
	mux.HandleFunc("POST /repair", h.handleRepair)
}

// GET /healthz -- unauthenticated liveness probe
func (h *Handlers) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": h.Version,
	})
}

// GET /health -- full agent + system health snapshot
func (h *Handlers) handleHealth(w http.ResponseWriter, r *http.Request) {
	state := h.Watcher.State()
	sysInfo := h.Collector.SystemInfo()

	writeJSON(w, http.StatusOK, map[string]any{
		"version":       h.Version,
		"framework":     h.Config.Framework,
		"agentName":     h.Config.AgentName,
		"uptime":        time.Since(startTime).Seconds(),
		"agentState":    state.Status,
		"agentUnit":     h.Config.SystemdUnit,
		"lastCrash":     state.LastCrashAt,
		"crashCount":    state.CrashCount,
		"system":        sysInfo,
		"goVersion":     runtime.Version(),
		"repairHistory": h.Repairer.RecentHistory(10),
	})
}

// POST /repair -- execute a whitelisted repair action (from hospital)
func (h *Handlers) handleRepair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	if req.Action == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "action is required"})
		return
	}

	h.Logger.Info("repair requested by hospital", "action", req.Action)

	result := h.Repairer.Execute(req.Action)

	status := http.StatusOK
	if !result.Success {
		status = http.StatusUnprocessableEntity
	}

	writeJSON(w, status, map[string]any{
		"action":  result.Action,
		"success": result.Success,
		"output":  result.Output,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

var startTime = time.Now()
