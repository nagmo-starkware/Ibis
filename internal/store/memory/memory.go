package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/types"
)

// MemoryStore implements store.Store using Go maps. Thread-safe via sync.RWMutex.
// No persistence — data is lost on restart (by design for dev/test).
type MemoryStore struct {
	// events stores log/unique events keyed by table name, then by "block:logIndex".
	events map[string]map[string]types.IndexedEvent

	// uniqueEntries stores the latest entry per unique key for unique tables.
	uniqueEntries map[string]map[string]types.IndexedEvent

	// aggregations stores aggregation values per table.
	aggregations map[string]map[string]float64

	// schemas stores table schema definitions.
	schemas map[string]types.TableSchema

	cursors          map[string]uint64
	dynamicContracts map[string]config.ContractConfig
	mu               sync.RWMutex
}

// New creates a new in-memory store.
func New() *MemoryStore {
	return &MemoryStore{
		events:           make(map[string]map[string]types.IndexedEvent),
		uniqueEntries:    make(map[string]map[string]types.IndexedEvent),
		aggregations:     make(map[string]map[string]float64),
		schemas:          make(map[string]types.TableSchema),
		cursors:          make(map[string]uint64),
		dynamicContracts: make(map[string]config.ContractConfig),
	}
}

// eventKey builds a composite key from block number and log index.
func eventKey(block, logIndex uint64) string {
	return fmt.Sprintf("%d:%d", block, logIndex)
}

// ---- Store interface implementation ----

func (s *MemoryStore) ApplyOperations(_ context.Context, ops []store.Operation) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, op := range ops {
		s.applyOp(op)
	}
	return nil
}

func (s *MemoryStore) RevertOperations(ctx context.Context, ops []store.Operation) error {
	reversed := make([]store.Operation, len(ops))
	for i, op := range ops {
		reversed[len(ops)-1-i] = op.InverseOp()
	}
	return s.ApplyOperations(ctx, reversed)
}

func (s *MemoryStore) applyOp(op store.Operation) {
	schema, hasSchema := s.schemas[op.Table]
	key := eventKey(op.BlockNumber, op.LogIndex)
	// For shared tables, include contract_address in the key so concurrent
	// polls from different contracts don't overwrite each other.
	if hasSchema && schema.SharedTable {
		if addr, ok := op.Data["contract_address"]; ok {
			key = fmt.Sprint(addr) + ":" + key
		}
	}

	switch op.Type {
	case store.OpInsert:
		// Ensure table map exists.
		if s.events[op.Table] == nil {
			s.events[op.Table] = make(map[string]types.IndexedEvent)
		}

		evt := types.IndexedEvent{
			BlockNumber: op.BlockNumber,
			LogIndex:    op.LogIndex,
			Data:        copyMap(op.Data),
		}
		populateFromData(&evt)

		s.events[op.Table][key] = evt

		// Update unique index.
		if hasSchema && schema.TableType == types.TableTypeUnique && schema.UniqueKey != "" {
			if ukVal, ok := op.Data[schema.UniqueKey]; ok {
				if s.uniqueEntries[op.Table] == nil {
					s.uniqueEntries[op.Table] = make(map[string]types.IndexedEvent)
				}
				entryKey := uniqueEntryKey(&schema, op.Data, fmt.Sprint(ukVal))
				s.uniqueEntries[op.Table][entryKey] = evt
			}
		}

		// Update aggregations.
		if hasSchema && schema.TableType == types.TableTypeAggregation {
			s.applyAggDelta(op.Table, schema.Aggregates, op.Data, false)
		}

	case store.OpUpdate:
		if s.events[op.Table] == nil {
			s.events[op.Table] = make(map[string]types.IndexedEvent)
		}

		evt := types.IndexedEvent{
			BlockNumber: op.BlockNumber,
			LogIndex:    op.LogIndex,
			Data:        copyMap(op.Data),
		}
		populateFromData(&evt)

		s.events[op.Table][key] = evt

		// Update unique index.
		if hasSchema && schema.TableType == types.TableTypeUnique && schema.UniqueKey != "" {
			if ukVal, ok := op.Data[schema.UniqueKey]; ok {
				if s.uniqueEntries[op.Table] == nil {
					s.uniqueEntries[op.Table] = make(map[string]types.IndexedEvent)
				}
				entryKey := uniqueEntryKey(&schema, op.Data, fmt.Sprint(ukVal))
				s.uniqueEntries[op.Table][entryKey] = evt
			}
		}

	case store.OpDelete:
		if tbl := s.events[op.Table]; tbl != nil {
			delete(tbl, key)
		}

		// Remove from unique index.
		if hasSchema && schema.TableType == types.TableTypeUnique && schema.UniqueKey != "" {
			if ukVal, ok := op.Data[schema.UniqueKey]; ok {
				if tbl := s.uniqueEntries[op.Table]; tbl != nil {
					entryKey := uniqueEntryKey(&schema, op.Data, fmt.Sprint(ukVal))
					delete(tbl, entryKey)
				}
			}
		}

		// Subtract aggregations.
		if hasSchema && schema.TableType == types.TableTypeAggregation {
			s.applyAggDelta(op.Table, schema.Aggregates, op.Data, true)
		}
	}
}

func (s *MemoryStore) applyAggDelta(table string, specs []types.AggregateSpec, data map[string]any, subtract bool) {
	if s.aggregations[table] == nil {
		s.aggregations[table] = make(map[string]float64)
	}
	agg := s.aggregations[table]

	for _, spec := range specs {
		switch spec.Operation {
		case "sum":
			val := toFloat64(data[spec.Field])
			if subtract {
				agg[spec.Column] -= val
			} else {
				agg[spec.Column] += val
			}
		case "count":
			if subtract {
				agg[spec.Column]--
			} else {
				agg[spec.Column]++
			}
		case "avg":
			sumKey := spec.Column + "__sum"
			cntKey := spec.Column + "__count"
			val := toFloat64(data[spec.Field])
			if subtract {
				agg[sumKey] -= val
				agg[cntKey]--
			} else {
				agg[sumKey] += val
				agg[cntKey]++
			}
			if agg[cntKey] > 0 {
				agg[spec.Column] = agg[sumKey] / agg[cntKey]
			} else {
				agg[spec.Column] = 0
			}
		}
	}
}

func (s *MemoryStore) GetEvents(_ context.Context, table string, q store.Query) ([]types.IndexedEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tbl := s.events[table]
	if tbl == nil {
		return nil, nil
	}

	// Collect all events.
	events := make([]types.IndexedEvent, 0, len(tbl))
	for _, evt := range tbl {
		if matchFilters(&evt, q.Filters) {
			events = append(events, evt)
		}
	}

	// Sort.
	sortEvents(events, q.OrderBy, q.OrderDir)

	// Paginate.
	if q.Offset > 0 && q.Offset < len(events) {
		events = events[q.Offset:]
	} else if q.Offset >= len(events) && q.Offset > 0 {
		return nil, nil
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if len(events) > limit {
		events = events[:limit]
	}

	return events, nil
}

func (s *MemoryStore) GetUniqueEvents(_ context.Context, table string, q store.Query) ([]types.IndexedEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tbl := s.uniqueEntries[table]
	if tbl == nil {
		return nil, nil
	}

	events := make([]types.IndexedEvent, 0, len(tbl))
	for _, evt := range tbl {
		if matchFilters(&evt, q.Filters) {
			events = append(events, evt)
		}
	}

	// Sort.
	sortEvents(events, q.OrderBy, q.OrderDir)

	// Paginate.
	if q.Offset > 0 && q.Offset < len(events) {
		events = events[q.Offset:]
	} else if q.Offset >= len(events) && q.Offset > 0 {
		return nil, nil
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if len(events) > limit {
		events = events[:limit]
	}

	return events, nil
}

func (s *MemoryStore) GetAggregation(_ context.Context, table string, _ store.Query) (store.AggResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := store.AggResult{Values: make(map[string]any)}

	agg := s.aggregations[table]
	if agg == nil {
		return result, nil
	}

	// Copy values, excluding internal __sum/__count keys.
	for k, v := range agg {
		if !strings.HasSuffix(k, "__sum") && !strings.HasSuffix(k, "__count") {
			result.Values[k] = v
		}
	}

	return result, nil
}

func (s *MemoryStore) GetCursor(_ context.Context, contract string) (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cursors[contract], nil
}

func (s *MemoryStore) SetCursor(_ context.Context, contract string, blockNumber uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursors[contract] = blockNumber
	return nil
}

func (s *MemoryStore) GetAllCursors(_ context.Context) (map[string]uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]uint64, len(s.cursors))
	for k, v := range s.cursors {
		result[k] = v
	}
	return result, nil
}

func (s *MemoryStore) CreateTable(_ context.Context, schema *types.TableSchema) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schemas[schema.Name] = *schema
	return nil
}

func (s *MemoryStore) MigrateTable(ctx context.Context, schema *types.TableSchema) error {
	return s.CreateTable(ctx, schema)
}

func (s *MemoryStore) CountEvents(_ context.Context, table string, filters []store.Filter) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tbl := s.events[table]
	if tbl == nil {
		return 0, nil
	}

	var count int64
	for _, evt := range tbl {
		if matchFilters(&evt, filters) {
			count++
		}
	}
	return count, nil
}

func (s *MemoryStore) DropTable(_ context.Context, tableName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.events, tableName)
	delete(s.uniqueEntries, tableName)
	delete(s.aggregations, tableName)
	delete(s.schemas, tableName)
	return nil
}

func (s *MemoryStore) SaveDynamicContract(_ context.Context, cc *config.ContractConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dynamicContracts[cc.Name] = *cc
	return nil
}

func (s *MemoryStore) GetDynamicContracts(_ context.Context) ([]config.ContractConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]config.ContractConfig, 0, len(s.dynamicContracts))
	for k := range s.dynamicContracts {
		result = append(result, s.dynamicContracts[k])
	}
	return result, nil
}

func (s *MemoryStore) DeleteDynamicContract(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.dynamicContracts, name)
	return nil
}

func (s *MemoryStore) DeleteCursor(_ context.Context, contract string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cursors, contract)
	return nil
}

func (s *MemoryStore) Close() error {
	return nil
}

// uniqueEntryKey builds the map key for unique entries. For shared tables, includes
// contract_address to provide per-contract uniqueness.
func uniqueEntryKey(schema *types.TableSchema, data map[string]any, ukVal string) string {
	if schema.SharedTable {
		contractAddr := fmt.Sprint(data["contract_address"])
		return contractAddr + ":" + ukVal
	}
	return ukVal
}

// ---- Internal helpers ----

func copyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func populateFromData(evt *types.IndexedEvent) {
	if evt.Data == nil {
		return
	}
	if v, ok := evt.Data["block_number"]; ok {
		evt.BlockNumber = toUint64(v)
	}
	if v, ok := evt.Data["log_index"]; ok {
		evt.LogIndex = toUint64(v)
	}
	if v, ok := evt.Data["transaction_hash"]; ok {
		evt.TransactionHash = fmt.Sprint(v)
	}
	if v, ok := evt.Data["contract_address"]; ok {
		evt.ContractAddress = fmt.Sprint(v)
	}
	if v, ok := evt.Data["event_name"]; ok {
		evt.EventName = fmt.Sprint(v)
	}
	if v, ok := evt.Data["timestamp"]; ok {
		evt.Timestamp = toUint64(v)
	}
}

func matchFilters(evt *types.IndexedEvent, filters []store.Filter) bool {
	for _, f := range filters {
		val, ok := evt.Data[f.Field]
		if !ok {
			return false
		}
		if !matchFilter(val, f.Operator, f.Value) {
			return false
		}
	}
	return true
}

func matchFilter(actual any, op string, expected any) bool {
	switch op {
	case "eq":
		return fmt.Sprint(actual) == fmt.Sprint(expected)
	case "neq":
		return fmt.Sprint(actual) != fmt.Sprint(expected)
	case "gt":
		return toFloat64(actual) > toFloat64(expected)
	case "gte":
		return toFloat64(actual) >= toFloat64(expected)
	case "lt":
		return toFloat64(actual) < toFloat64(expected)
	case "lte":
		return toFloat64(actual) <= toFloat64(expected)
	default:
		return fmt.Sprint(actual) == fmt.Sprint(expected)
	}
}

func sortEvents(events []types.IndexedEvent, orderBy string, dir store.OrderDirection) {
	if orderBy == "" {
		orderBy = "block_number"
	}
	sort.Slice(events, func(i, j int) bool {
		vi := getFieldValue(&events[i], orderBy)
		vj := getFieldValue(&events[j], orderBy)

		// Secondary sort by log_index for stable ordering within same block.
		if toFloat64(vi) == toFloat64(vj) {
			if dir == store.OrderDesc {
				return events[i].LogIndex > events[j].LogIndex
			}
			return events[i].LogIndex < events[j].LogIndex
		}

		if dir == store.OrderDesc {
			return toFloat64(vi) > toFloat64(vj)
		}
		return toFloat64(vi) < toFloat64(vj)
	})
}

func getFieldValue(evt *types.IndexedEvent, field string) any {
	switch field {
	case "block_number":
		return evt.BlockNumber
	case "log_index":
		return evt.LogIndex
	case "timestamp":
		return evt.Timestamp
	default:
		if evt.Data != nil {
			return evt.Data[field]
		}
		return nil
	}
}

func toFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0
		}
		return f
	default:
		return 0
	}
}

func toUint64(v any) uint64 {
	switch n := v.(type) {
	case float64:
		return uint64(n)
	case float32:
		return uint64(n)
	case int:
		return uint64(n)
	case int64:
		return uint64(n)
	case uint64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return uint64(i)
	default:
		return 0
	}
}
