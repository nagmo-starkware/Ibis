package api

import (
	"fmt"
	"net/http"

	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/types"
)

// listResponse is the standard JSON envelope for list endpoints.
type listResponse struct {
	Data   []map[string]any `json:"data"`
	Count  int              `json:"count"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	contract := r.PathValue("contract")
	event := r.PathValue("event")

	schema := s.lookupSchema(contract, event)
	if schema == nil {
		writeError(w, http.StatusNotFound, "table not found")
		return
	}

	q, err := parseQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	events, err := s.store.GetEvents(r.Context(), schema.Name, q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		s.logger.Error("GetEvents failed", "table", schema.Name, "error", err)
		return
	}

	data := eventsToMaps(events)
	writeJSON(w, http.StatusOK, listResponse{
		Data:   data,
		Count:  len(data),
		Limit:  q.Limit,
		Offset: q.Offset,
	})
}

func (s *Server) handleGetLatest(w http.ResponseWriter, r *http.Request) {
	contract := r.PathValue("contract")
	event := r.PathValue("event")

	schema := s.lookupSchema(contract, event)
	if schema == nil {
		writeError(w, http.StatusNotFound, "table not found")
		return
	}

	q := store.Query{
		Limit:    1,
		OrderBy:  "block_number",
		OrderDir: store.OrderDesc,
	}

	events, err := s.store.GetEvents(r.Context(), schema.Name, q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		s.logger.Error("GetEvents failed", "table", schema.Name, "error", err)
		return
	}

	if len(events) == 0 {
		writeError(w, http.StatusNotFound, "no events found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": events[0].Data})
}

func (s *Server) handleGetCount(w http.ResponseWriter, r *http.Request) {
	contract := r.PathValue("contract")
	event := r.PathValue("event")

	schema := s.lookupSchema(contract, event)
	if schema == nil {
		writeError(w, http.StatusNotFound, "table not found")
		return
	}

	filters, err := parseFiltersFromURL(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	count, err := s.store.CountEvents(r.Context(), schema.Name, filters)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "count failed")
		s.logger.Error("CountEvents failed", "table", schema.Name, "error", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"count": count})
}

func (s *Server) handleGetUnique(w http.ResponseWriter, r *http.Request) {
	contract := r.PathValue("contract")
	event := r.PathValue("event")

	schema := s.lookupSchema(contract, event)
	if schema == nil {
		writeError(w, http.StatusNotFound, "table not found")
		return
	}

	if schema.TableType != types.TableTypeUnique {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("endpoint /unique is only available for unique table types; '%s' is a %s table", event, schema.TableType))
		return
	}

	q, err := parseQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	events, err := s.store.GetUniqueEvents(r.Context(), schema.Name, q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		s.logger.Error("GetUniqueEvents failed", "table", schema.Name, "error", err)
		return
	}

	data := eventsToMaps(events)
	writeJSON(w, http.StatusOK, listResponse{
		Data:   data,
		Count:  len(data),
		Limit:  q.Limit,
		Offset: q.Offset,
	})
}

func (s *Server) handleGetAggregate(w http.ResponseWriter, r *http.Request) {
	contract := r.PathValue("contract")
	event := r.PathValue("event")

	schema := s.lookupSchema(contract, event)
	if schema == nil {
		writeError(w, http.StatusNotFound, "table not found")
		return
	}

	if schema.TableType != types.TableTypeAggregation {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("endpoint /aggregate is only available for aggregation table types; '%s' is a %s table", event, schema.TableType))
		return
	}

	q, err := parseQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := s.store.GetAggregation(r.Context(), schema.Name, q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "aggregation failed")
		s.logger.Error("GetAggregation failed", "table", schema.Name, "error", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": result.Values})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// transportStatus is the stream-liveness view, served both nested under
// "transport" by /v1/status and flat by /v1/catchup_status. Defined once
// because the two must agree on what "caught up" means: a promote gate reads
// the second to decide whether to flip a slot, and an operator reads the first
// to understand why it did.
type transportStatus struct {
	StreamsLive  int64 `json:"streams_live"`
	StreamsTotal int64 `json:"streams_total"`
	// BackfillsPending explains a false catchup_complete when every stream is
	// live: a contract discovered after startup whose history is still loading
	// (or retrying). Informational — gate on catchup_complete.
	BackfillsPending int64 `json:"backfills_pending"`
	CatchupComplete  bool  `json:"catchup_complete"`
}

// transportStatus reads stream liveness from the engine. With no engine wired
// it reports zero streams, which is NOT caught up — an instance that is not
// indexing must never satisfy a gate.
func (s *Server) transportStatus() transportStatus {
	if s.engine == nil {
		return transportStatus{}
	}
	live, total, complete := s.engine.TransportStatus()
	return transportStatus{
		StreamsLive:      live,
		StreamsTotal:     total,
		BackfillsPending: s.engine.BackfillsPending(),
		CatchupComplete:  complete,
	}
}

// handleCatchupStatus answers "is this instance streaming, or still
// backfilling?" and nothing else. It exists separately from /v1/status because
// a promote gate has to poll it: /v1/status serialises every contract, which on
// a large deployment is ~10MB per request, while this reads only in-memory
// counters and touches no store. Exempt from the ready gate, so a slot that is
// still starting up answers instead of 503-ing -- a gate cannot distinguish
// "not caught up" from "unreachable" if the endpoint stays silent.
//
// Always 200: "still catching up" is a valid, healthy answer. Callers gate on
// catchup_complete rather than on the status code.
func (s *Server) handleCatchupStatus(w http.ResponseWriter, _ *http.Request) {
	// Embedded, so the liveness fields sit flat alongside "ready" rather than
	// nested — this is the probe payload, not a report.
	writeJSON(w, http.StatusOK, struct {
		Ready bool `json:"ready"`
		transportStatus
	}{
		Ready:           s.ready.Load(),
		transportStatus: s.transportStatus(),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cursors, err := s.store.GetAllCursors(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get cursors")
		return
	}

	// Compute global cursor as min of all persisted contract cursors.
	// Contracts that haven't processed any events yet are excluded
	// from the min so they don't drag global progress to zero.
	var globalCursor uint64
	first := true
	for _, cur := range cursors {
		if first || cur < globalCursor {
			globalCursor = cur
			first = false
		}
	}

	s.mu.RLock()
	contractsCopy := make([]config.ContractConfig, len(s.contracts))
	copy(contractsCopy, s.contracts)
	s.mu.RUnlock()

	contracts := make([]map[string]any, 0, len(contractsCopy))
	for i := range contractsCopy {
		c := &contractsCopy[i]
		entry := map[string]any{
			"name":          c.Name,
			"address":       c.Address,
			"events":        len(c.Events),
			"current_block": cursors[c.Name],
		}
		contracts = append(contracts, entry)
	}

	resp := map[string]any{
		// NOTE: this is the MIN cursor across contracts that have indexed at
		// least one event, so it is pinned by the most dormant contract and
		// barely moves. It is a data-floor, NOT a catch-up signal — read
		// "transport" below to tell whether this instance is actually current.
		"current_block": globalCursor,
		// Alias of current_block. promote-ibis.sh's parity check reads this
		// name; the field never existed, so every promote silently fell through
		// to its "could not reach /v1/status" warning and proceeded unverified.
		"indexed_block_number": globalCursor,
		"ready":                s.ready.Load(),
		"contracts":            contracts,
	}

	// Stream liveness: the signal a blue/green promote gate needs. live < total
	// means some stream is still gap-filling, so the contracts it covers are
	// stale even though the instance is ready and serving.
	resp["transport"] = s.transportStatus()

	// Add factory summary: child count, synced count, backfilling count.
	if s.engine != nil {
		factories := make(map[string]any)
		for i := range contractsCopy {
			c := &contractsCopy[i]
			if len(c.Factories) > 0 {
				children := s.engine.FactoryChildren(c.Name)
				synced := 0
				backfilling := 0
				for _, child := range children {
					if child.CurrentBlock >= globalCursor {
						synced++
					} else {
						backfilling++
					}
				}
				factories[c.Name] = map[string]any{
					"child_count": len(children),
					"synced":      synced,
					"backfilling": backfilling,
				}
			}
		}
		if len(factories) > 0 {
			resp["factories"] = factories
		}

		// Add view function status.
		views := s.engine.ViewStatuses()
		if len(views) > 0 {
			resp["views"] = views
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func eventsToMaps(events []types.IndexedEvent) []map[string]any {
	if events == nil {
		return []map[string]any{}
	}
	data := make([]map[string]any, len(events))
	for i, evt := range events {
		data[i] = evt.Data
	}
	return data
}
