package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/engine"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/types"
)

// Server is the auto-generated REST API server. It generates endpoints from
// ABI-derived table schemas with Supabase-style query syntax.
type Server struct {
	store     store.Store
	schemas   map[string]*types.TableSchema // "contract/event" -> schema
	cfg       *config.APIConfig
	contracts []config.ContractConfig
	logger    *slog.Logger
	server    *http.Server
	bus       *EventBus      // SSE event bus for real-time streaming
	engine    *engine.Engine // Engine reference for dynamic contract management
	mu        sync.RWMutex   // Protects schemas and contracts

	// ready gates the data endpoints. Defaults true, so a server that never
	// calls SetReady behaves as before. Callers that bind the listener before
	// the engine has built its schemas set it false until setup finishes, so
	// consumers get a loud 503 rather than a silently empty result.
	ready atomic.Bool
}

// SetReady toggles whether the data endpoints serve. /v1/health and /v1/status
// answer regardless, so a not-ready instance stays diagnosable.
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

// ServerConfig holds the dependencies needed to create an API server.
type ServerConfig struct {
	Store     store.Store
	Schemas   []*types.TableSchema
	APIConfig *config.APIConfig
	Contracts []config.ContractConfig
	Logger    *slog.Logger
	EventBus  *EventBus      // Optional: enables SSE streaming when set
	Engine    *engine.Engine // Optional: enables dynamic contract management
}

// New creates an API Server from the given configuration.
func New(cfg *ServerConfig) *Server {
	schemaMap := make(map[string]*types.TableSchema)
	for _, s := range cfg.Schemas {
		key := strings.ToLower(s.Contract) + "/" + strings.ToLower(s.Event)
		schemaMap[key] = s
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	srv := &Server{
		store:     cfg.Store,
		schemas:   schemaMap,
		cfg:       cfg.APIConfig,
		contracts: cfg.Contracts,
		logger:    logger.With("component", "api"),
		bus:       cfg.EventBus,
		engine:    cfg.Engine,
	}
	srv.ready.Store(true)
	return srv
}

// Handler returns the configured http.Handler (for testing with httptest).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	return s.corsMiddleware(s.loggingMiddleware(s.readyGate(mux)))
}

// Start begins serving HTTP requests. It blocks until the context is canceled.
func (s *Server) Start(ctx context.Context) error {
	handler := s.Handler()

	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	s.server = &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	s.logger.Info("API server starting", "addr", addr)

	go func() {
		<-ctx.Done()
		// Close SSE connections before shutting down the HTTP server.
		if s.bus != nil {
			s.bus.Close()
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("HTTP server shutdown error", "error", err)
		}
	}()

	if err := s.server.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// readyGate 503s the data endpoints until the server is marked ready. Health
// and status are always allowed: promote-ibis.sh reads /v1/status to decide
// whether a standby slot has caught up, and it can only tell "not ready yet"
// from "unreachable" if the endpoint answers.
func (s *Server) readyGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() && r.URL.Path != "/v1/health" && r.URL.Path != "/v1/status" {
			writeError(w, http.StatusServiceUnavailable, "indexer is still starting up")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	// System endpoints.
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/status", s.handleStatus)

	// Admin endpoints for dynamic contract management.
	mux.HandleFunc("POST /v1/admin/contracts", s.adminAuth(s.handleAdminRegisterContract))
	mux.HandleFunc("DELETE /v1/admin/contracts/{name}", s.adminAuth(s.handleAdminDeregisterContract))
	mux.HandleFunc("GET /v1/admin/contracts", s.adminAuth(s.handleAdminListContracts))
	mux.HandleFunc("PUT /v1/admin/contracts/{name}", s.adminAuth(s.handleAdminUpdateContract))

	// Discover endpoint (literal "discover" takes priority over {contract} wildcard).
	mux.HandleFunc("GET /v1/discover/{classHash}/contracts", s.handleDiscoverContracts)

	// Factory children endpoints (literal "children" takes priority over {event} wildcard).
	mux.HandleFunc("GET /v1/{factory}/children/count", s.handleFactoryChildCount)
	mux.HandleFunc("GET /v1/{factory}/children", s.handleFactoryChildren)

	// Event table endpoints (more specific paths registered first).
	mux.HandleFunc("GET /v1/{contract}/{event}/stream", s.handleStream)
	mux.HandleFunc("GET /v1/{contract}/{event}/latest", s.handleGetLatest)
	mux.HandleFunc("GET /v1/{contract}/{event}/count", s.handleGetCount)
	mux.HandleFunc("GET /v1/{contract}/{event}/unique", s.handleGetUnique)
	mux.HandleFunc("GET /v1/{contract}/{event}/aggregate", s.handleGetAggregate)
	mux.HandleFunc("GET /v1/{contract}/{event}", s.handleListEvents)
}

// lookupSchema finds a table schema by contract name and event name (case-insensitive).
func (s *Server) lookupSchema(contract, event string) *types.TableSchema {
	key := strings.ToLower(contract) + "/" + strings.ToLower(event)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.schemas[key]
}

// AddSchemas registers additional table schemas (for dynamically registered contracts).
func (s *Server) AddSchemas(cc *config.ContractConfig, schemas []*types.TableSchema) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sch := range schemas {
		key := strings.ToLower(sch.Contract) + "/" + strings.ToLower(sch.Event)
		s.schemas[key] = sch
	}
	s.contracts = append(s.contracts, *cc)
}

// RemoveSchemas removes all schemas for a contract (for deregistered contracts).
func (s *Server) RemoveSchemas(contractName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lower := strings.ToLower(contractName)
	for key := range s.schemas {
		if strings.HasPrefix(key, lower+"/") {
			delete(s.schemas, key)
		}
	}
	// Remove from contracts list.
	for i := range s.contracts {
		if s.contracts[i].Name == contractName {
			s.contracts = append(s.contracts[:i], s.contracts[i+1:]...)
			break
		}
	}
}

// ---- Middleware ----

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		origins := s.cfg.CORSOrigins
		if len(origins) == 0 {
			origins = []string{"*"}
		}

		allowed := false
		for _, o := range origins {
			if o == "*" || o == origin {
				allowed = true
				break
			}
		}

		if allowed {
			if origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			} else {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			}
		}

		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Admin-Key")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration", time.Since(start),
		)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher by delegating to the underlying ResponseWriter.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
