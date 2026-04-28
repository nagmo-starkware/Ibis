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
		"current_block": globalCursor,
		"contracts":     contracts,
	}

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
