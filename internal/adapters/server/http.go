package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"replication-service/internal/core/domain"
	"replication-service/internal/core/ports"
	"replication-service/internal/core/services/orchestrator"
)

type HTTPServer struct {
	logger      ports.Logger
	service     *orchestrator.Service
	server      *http.Server
	broadcaster *LogBroadcaster
}

func NewHTTPServer(logger ports.Logger, service *orchestrator.Service, broadcaster *LogBroadcaster, port int) *HTTPServer {
	mux := http.NewServeMux()
	h := &HTTPServer{
		logger:      logger,
		service:     service,
		broadcaster: broadcaster,
	}

	mux.HandleFunc("GET /config", h.handleGetConfig)
	mux.HandleFunc("POST /config", h.handleUpdateConfig)
	
	mux.HandleFunc("GET /syncs", h.handleListSyncs)
	mux.HandleFunc("POST /syncs", h.handleCreateSync)
	mux.HandleFunc("GET /syncs/{name}", h.handleGetSync)
	mux.HandleFunc("PUT /syncs/{name}", h.handleUpdateSync)
	mux.HandleFunc("DELETE /syncs/{name}", h.handleDeleteSync)

	mux.HandleFunc("POST /start", h.handleStart)
	mux.HandleFunc("POST /stop", h.handleStop)
	mux.HandleFunc("POST /restart", h.handleRestart)

	mux.HandleFunc("/logs", h.broadcaster.HandleWebsocket)

	h.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	return h
}

func (h *HTTPServer) Start() {
	h.logger.Info("Starting HTTP server", "address", h.server.Addr)
	if err := h.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		h.logger.Error("HTTP server failed", "error", err)
	}
}

func (h *HTTPServer) Stop(ctx context.Context) error {
	return h.server.Shutdown(ctx)
}

func (h *HTTPServer) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	config, err := h.service.GetConfig(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(config)
}

func (h *HTTPServer) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var config domain.BucardoConfig
	if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if err := h.service.UpdateConfig(r.Context(), &config); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Config updated"))
}

func (h *HTTPServer) handleListSyncs(w http.ResponseWriter, r *http.Request) {
	syncs, err := h.service.ListSyncs(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(syncs)
}

func (h *HTTPServer) handleCreateSync(w http.ResponseWriter, r *http.Request) {
	var sync domain.Sync
	if err := json.NewDecoder(r.Body).Decode(&sync); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if err := h.service.AddSync(r.Context(), sync); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte("Sync created"))
}

func (h *HTTPServer) handleGetSync(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	sync, err := h.service.GetSync(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sync)
}

func (h *HTTPServer) handleUpdateSync(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var sync domain.Sync
	if err := json.NewDecoder(r.Body).Decode(&sync); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if err := h.service.UpdateSync(r.Context(), name, sync); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Sync updated"))
}

func (h *HTTPServer) handleDeleteSync(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.service.DeleteSync(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Sync deleted"))
}

func (h *HTTPServer) handleStart(w http.ResponseWriter, r *http.Request) {
	if err := h.service.StartBucardoProcess(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte("Bucardo started"))
}

func (h *HTTPServer) handleStop(w http.ResponseWriter, r *http.Request) {
	if err := h.service.StopBucardoProcess(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte("Bucardo stopped"))
}

func (h *HTTPServer) handleRestart(w http.ResponseWriter, r *http.Request) {
	// Restarting involves reloading config and reconciling
	if err := h.service.ReloadAndRestart(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte("Application reloaded and restarted"))
}
