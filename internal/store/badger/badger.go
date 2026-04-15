package badger

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/types"
)

// Key prefix patterns:
//
//	evt:{table}:{block}:{logIndex}           → primary index (ascending by block)
//	rev:{table}:{invertedBlock}:{logIndex}   → reverse index (descending by block)
//	unq:{table}:{uniqueKey}                  → unique index (last-write-wins)
//	agg:{table}                              → aggregation data
//	meta:cursor:{contract}                   → per-contract last processed block number
//	schema:{table}                           → table schema definition
const (
	prefixEvt      = "evt:"
	prefixRev      = "rev:"
	prefixUnq      = "unq:"
	prefixAgg      = "agg:"
	prefixSchema   = "schema:"
	prefixCursor   = "meta:cursor:"
	prefixContract = "meta:contracts:"
)

// BadgerStore implements store.Store using BadgerDB v4.
type BadgerStore struct {
	db      *badgerdb.DB
	schemas map[string]types.TableSchema
	mu      sync.RWMutex
}

// New opens a BadgerDB at the given path and returns a BadgerStore.
func New(path string) (*BadgerStore, error) {
	opts := badgerdb.DefaultOptions(path).
		WithLoggingLevel(badgerdb.WARNING)

	db, err := badgerdb.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("opening badger at %s: %w", path, err)
	}

	s := &BadgerStore{
		db:      db,
		schemas: make(map[string]types.TableSchema),
	}

	if err := s.loadSchemas(); err != nil {
		db.Close()
		return nil, fmt.Errorf("loading schemas: %w", err)
	}

	return s, nil
}

// NewInMemory creates a BadgerStore backed by an in-memory database (for testing).
func NewInMemory() (*BadgerStore, error) {
	opts := badgerdb.DefaultOptions("").
		WithInMemory(true).
		WithLoggingLevel(badgerdb.WARNING)

	db, err := badgerdb.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("opening in-memory badger: %w", err)
	}

	return &BadgerStore{
		db:      db,
		schemas: make(map[string]types.TableSchema),
	}, nil
}

// ---- Key encoding helpers ----

// encodeBlock returns an 8-byte big-endian encoding of a block number.
func encodeBlock(block uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], block)
	return string(buf[:])
}

// invertBlock returns math.MaxUint64 - block for reverse index ordering.
func invertBlock(block uint64) uint64 {
	return math.MaxUint64 - block
}

// evtKey builds the primary event key.
func evtKey(table string, block, logIndex uint64) []byte {
	return []byte(fmt.Sprintf("%s%s:%s:%s", prefixEvt, table, encodeBlock(block), encodeBlock(logIndex)))
}

// revKey builds the reverse-order event key.
func revKey(table string, block, logIndex uint64) []byte {
	return []byte(fmt.Sprintf("%s%s:%s:%s", prefixRev, table, encodeBlock(invertBlock(block)), encodeBlock(logIndex)))
}

// unqKey builds the unique index key.
func unqKey(table, uniqueVal string) []byte {
	return []byte(fmt.Sprintf("%s%s:%s", prefixUnq, table, uniqueVal))
}

// aggKey builds the aggregation key.
func aggKey(table string) []byte {
	return []byte(fmt.Sprintf("%s%s", prefixAgg, table))
}

// schemaKey builds the schema storage key.
func schemaKey(table string) []byte {
	return []byte(fmt.Sprintf("%s%s", prefixSchema, table))
}

// evtPrefix returns the prefix for scanning all events in a table.
func evtPrefix(table string) []byte {
	return []byte(fmt.Sprintf("%s%s:", prefixEvt, table))
}

// revPrefix returns the prefix for scanning all reverse events in a table.
func revPrefix(table string) []byte {
	return []byte(fmt.Sprintf("%s%s:", prefixRev, table))
}

// ---- Store interface implementation ----

func (s *BadgerStore) ApplyOperations(ctx context.Context, ops []store.Operation) error {
	wb := s.db.NewWriteBatch()
	defer wb.Cancel()

	// Collect aggregation deltas per table so they can be applied in one read-modify-write.
	aggDeltas := make(map[string][]aggDelta)

	for _, op := range ops {
		if err := s.applyOp(wb, op, aggDeltas); err != nil {
			return fmt.Errorf("applying operation on %s: %w", op.Table, err)
		}
	}

	// Apply accumulated aggregation deltas.
	for table, deltas := range aggDeltas {
		if err := s.applyAggDeltas(wb, table, deltas); err != nil {
			return fmt.Errorf("applying aggregation for %s: %w", table, err)
		}
	}

	return wb.Flush()
}

// aggDelta records a single aggregation change.
type aggDelta struct {
	specs    []types.AggregateSpec
	data     map[string]any
	subtract bool
}

func (s *BadgerStore) RevertOperations(ctx context.Context, ops []store.Operation) error {
	// Revert in reverse order.
	reversed := make([]store.Operation, len(ops))
	for i, op := range ops {
		reversed[len(ops)-1-i] = op.InverseOp()
	}
	return s.ApplyOperations(ctx, reversed)
}

func (s *BadgerStore) applyOp(wb *badgerdb.WriteBatch, op store.Operation, aggDeltas map[string][]aggDelta) error {
	data, err := json.Marshal(op.Data)
	if err != nil {
		return fmt.Errorf("marshaling data: %w", err)
	}

	s.mu.RLock()
	schema, hasSchema := s.schemas[op.Table]
	s.mu.RUnlock()

	switch op.Type {
	case store.OpInsert:
		if err := wb.Set(evtKey(op.Table, op.BlockNumber, op.LogIndex), data); err != nil {
			return err
		}
		if err := wb.Set(revKey(op.Table, op.BlockNumber, op.LogIndex), data); err != nil {
			return err
		}
		if hasSchema && schema.TableType == types.TableTypeUnique && schema.UniqueKey != "" {
			if ukVal, ok := op.Data[schema.UniqueKey]; ok {
				key := s.buildUnqKey(op, &schema, fmt.Sprint(ukVal))
				if err := wb.Set(key, data); err != nil {
					return err
				}
			}
		}
		if hasSchema && schema.TableType == types.TableTypeAggregation {
			aggDeltas[op.Table] = append(aggDeltas[op.Table], aggDelta{
				specs: schema.Aggregates, data: op.Data, subtract: false,
			})
		}

	case store.OpUpdate:
		if err := wb.Set(evtKey(op.Table, op.BlockNumber, op.LogIndex), data); err != nil {
			return err
		}
		if err := wb.Set(revKey(op.Table, op.BlockNumber, op.LogIndex), data); err != nil {
			return err
		}
		if hasSchema && schema.TableType == types.TableTypeUnique && schema.UniqueKey != "" {
			if ukVal, ok := op.Data[schema.UniqueKey]; ok {
				key := s.buildUnqKey(op, &schema, fmt.Sprint(ukVal))
				if err := wb.Set(key, data); err != nil {
					return err
				}
			}
		}

	case store.OpDelete:
		if err := wb.Delete(evtKey(op.Table, op.BlockNumber, op.LogIndex)); err != nil {
			return err
		}
		if err := wb.Delete(revKey(op.Table, op.BlockNumber, op.LogIndex)); err != nil {
			return err
		}
		if hasSchema && schema.TableType == types.TableTypeUnique && schema.UniqueKey != "" {
			if ukVal, ok := op.Data[schema.UniqueKey]; ok {
				key := s.buildUnqKey(op, &schema, fmt.Sprint(ukVal))
				if err := wb.Delete(key); err != nil {
					return err
				}
			}
		}
		if hasSchema && schema.TableType == types.TableTypeAggregation {
			aggDeltas[op.Table] = append(aggDeltas[op.Table], aggDelta{
				specs: schema.Aggregates, data: op.Data, subtract: true,
			})
		}
	}

	return nil
}

// buildUnqKey returns the unique index key, using a composite key for shared tables.
func (s *BadgerStore) buildUnqKey(op store.Operation, schema *types.TableSchema, uniqueVal string) []byte {
	if schema.SharedTable {
		contractAddr := fmt.Sprint(op.Data["contract_address"])
		return []byte(fmt.Sprintf("%s%s:%s:%s", prefixUnq, op.Table, contractAddr, uniqueVal))
	}
	return unqKey(op.Table, uniqueVal)
}

// applyAggDeltas reads the current aggregation, applies all deltas, and writes once.
func (s *BadgerStore) applyAggDeltas(wb *badgerdb.WriteBatch, table string, deltas []aggDelta) error {
	current := make(map[string]float64)
	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(aggKey(table))
		if err == badgerdb.ErrKeyNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &current)
		})
	})
	if err != nil {
		return fmt.Errorf("reading aggregation for %s: %w", table, err)
	}

	for _, d := range deltas {
		for _, spec := range d.specs {
			switch spec.Operation {
			case "sum":
				val := toFloat64(d.data[spec.Field])
				if d.subtract {
					current[spec.Column] -= val
				} else {
					current[spec.Column] += val
				}
			case "count":
				if d.subtract {
					current[spec.Column]--
				} else {
					current[spec.Column]++
				}
			case "avg":
				sumKey := spec.Column + "__sum"
				cntKey := spec.Column + "__count"
				val := toFloat64(d.data[spec.Field])
				if d.subtract {
					current[sumKey] -= val
					current[cntKey]--
				} else {
					current[sumKey] += val
					current[cntKey]++
				}
				if current[cntKey] > 0 {
					current[spec.Column] = current[sumKey] / current[cntKey]
				} else {
					current[spec.Column] = 0
				}
			}
		}
	}

	encoded, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("marshaling aggregation: %w", err)
	}
	return wb.Set(aggKey(table), encoded)
}

func (s *BadgerStore) GetEvents(ctx context.Context, table string, q store.Query) ([]types.IndexedEvent, error) {
	var prefix []byte
	reverse := false
	if q.OrderDir == store.OrderDesc {
		prefix = revPrefix(table)
		reverse = true
	} else {
		prefix = evtPrefix(table)
	}

	var events []types.IndexedEvent
	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		skipped := 0
		collected := 0
		limit := q.Limit
		if limit <= 0 {
			limit = 50
		}

		for it.Seek(prefix); it.Valid(); it.Next() {
			item := it.Item()
			var evt types.IndexedEvent
			if err := item.Value(func(val []byte) error {
				return json.Unmarshal(val, &evt.Data)
			}); err != nil {
				return err
			}

			// Extract block and logIndex from the key.
			s.parseEventKey(item.Key(), &evt, reverse)

			if !matchFilters(&evt, q.Filters) {
				continue
			}

			if skipped < q.Offset {
				skipped++
				continue
			}

			events = append(events, evt)
			collected++
			if collected >= limit {
				break
			}
		}
		return nil
	})

	return events, err
}

func (s *BadgerStore) GetUniqueEvents(ctx context.Context, table string, q store.Query) ([]types.IndexedEvent, error) {
	prefix := []byte(fmt.Sprintf("%s%s:", prefixUnq, table))

	var events []types.IndexedEvent
	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.Valid(); it.Next() {
			item := it.Item()
			var evt types.IndexedEvent
			if err := item.Value(func(val []byte) error {
				return json.Unmarshal(val, &evt.Data)
			}); err != nil {
				return err
			}
			// Populate fields from data if available.
			s.populateFromData(&evt)

			if !matchFilters(&evt, q.Filters) {
				continue
			}

			events = append(events, evt)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Sort.
	sortEvents(events, q.OrderBy, q.OrderDir)

	// Paginate.
	if q.Offset > 0 && q.Offset < len(events) {
		events = events[q.Offset:]
	} else if q.Offset >= len(events) {
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

func (s *BadgerStore) GetAggregation(ctx context.Context, table string, q store.Query) (store.AggResult, error) {
	result := store.AggResult{Values: make(map[string]any)}

	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(aggKey(table))
		if err == badgerdb.ErrKeyNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		var vals map[string]float64
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &vals)
		}); err != nil {
			return err
		}
		// Copy to result, excluding internal __sum/__count keys.
		for k, v := range vals {
			if !strings.HasSuffix(k, "__sum") && !strings.HasSuffix(k, "__count") {
				result.Values[k] = v
			}
		}
		return nil
	})

	return result, err
}

func (s *BadgerStore) GetCursor(_ context.Context, contract string) (uint64, error) {
	var cursor uint64
	key := []byte(prefixCursor + contract)
	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(key)
		if err == badgerdb.ErrKeyNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			if len(val) == 8 {
				cursor = binary.BigEndian.Uint64(val)
			}
			return nil
		})
	})
	return cursor, err
}

func (s *BadgerStore) SetCursor(_ context.Context, contract string, blockNumber uint64) error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], blockNumber)
	key := []byte(prefixCursor + contract)
	return s.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set(key, buf[:])
	})
}

func (s *BadgerStore) GetAllCursors(_ context.Context) (map[string]uint64, error) {
	result := make(map[string]uint64)
	prefix := []byte(prefixCursor)
	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.Valid(); it.Next() {
			item := it.Item()
			// Extract contract name from key: "meta:cursor:{contract}"
			contract := strings.TrimPrefix(string(item.Key()), string(prefix))
			if err := item.Value(func(val []byte) error {
				if len(val) == 8 {
					result[contract] = binary.BigEndian.Uint64(val)
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func (s *BadgerStore) CreateTable(ctx context.Context, schema *types.TableSchema) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.schemas[schema.Name] = *schema

	// Persist schema.
	data, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("marshaling schema: %w", err)
	}
	return s.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set(schemaKey(schema.Name), data)
	})
}

func (s *BadgerStore) MigrateTable(ctx context.Context, schema *types.TableSchema) error {
	// For BadgerDB, migration simply updates the schema definition.
	// Existing data remains accessible; new columns will have zero values.
	return s.CreateTable(ctx, schema)
}

func (s *BadgerStore) CountEvents(_ context.Context, table string, filters []store.Filter) (int64, error) {
	prefix := evtPrefix(table)
	var count int64

	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = prefix
		if len(filters) == 0 {
			opts.PrefetchValues = false
		}
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.Valid(); it.Next() {
			if len(filters) == 0 {
				count++
				continue
			}
			item := it.Item()
			var evt types.IndexedEvent
			if err := item.Value(func(val []byte) error {
				return json.Unmarshal(val, &evt.Data)
			}); err != nil {
				return err
			}
			s.populateFromData(&evt)
			if matchFilters(&evt, filters) {
				count++
			}
		}
		return nil
	})

	return count, err
}

func (s *BadgerStore) DropTable(_ context.Context, tableName string) error {
	s.mu.Lock()
	delete(s.schemas, tableName)
	s.mu.Unlock()

	// Delete all keys for this table across all prefixes.
	prefixes := [][]byte{
		evtPrefix(tableName),
		revPrefix(tableName),
		[]byte(fmt.Sprintf("%s%s:", prefixUnq, tableName)),
		aggKey(tableName),
		schemaKey(tableName),
	}

	for _, prefix := range prefixes {
		if err := s.deleteByPrefix(prefix); err != nil {
			return fmt.Errorf("dropping table %s: %w", tableName, err)
		}
	}
	return nil
}

func (s *BadgerStore) deleteByPrefix(prefix []byte) error {
	return s.db.Update(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		var keys [][]byte
		for it.Seek(prefix); it.Valid(); it.Next() {
			key := make([]byte, len(it.Item().Key()))
			copy(key, it.Item().Key())
			keys = append(keys, key)
		}
		for _, key := range keys {
			if err := txn.Delete(key); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *BadgerStore) SaveDynamicContract(_ context.Context, cc *config.ContractConfig) error {
	data, err := json.Marshal(cc)
	if err != nil {
		return fmt.Errorf("marshaling contract config: %w", err)
	}
	key := []byte(prefixContract + cc.Name)
	return s.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set(key, data)
	})
}

func (s *BadgerStore) GetDynamicContracts(_ context.Context) ([]config.ContractConfig, error) {
	var contracts []config.ContractConfig
	prefix := []byte(prefixContract)
	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.Valid(); it.Next() {
			item := it.Item()
			var cc config.ContractConfig
			if err := item.Value(func(val []byte) error {
				return json.Unmarshal(val, &cc)
			}); err != nil {
				return err
			}
			cc.Dynamic = true
			contracts = append(contracts, cc)
		}
		return nil
	})
	return contracts, err
}

func (s *BadgerStore) DeleteDynamicContract(_ context.Context, name string) error {
	key := []byte(prefixContract + name)
	return s.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Delete(key)
	})
}

func (s *BadgerStore) DeleteCursor(_ context.Context, contract string) error {
	key := []byte(prefixCursor + contract)
	return s.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Delete(key)
	})
}

func (s *BadgerStore) Close() error {
	return s.db.Close()
}

// ---- Internal helpers ----

func (s *BadgerStore) loadSchemas() error {
	return s.db.View(func(txn *badgerdb.Txn) error {
		prefix := []byte(prefixSchema)
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.Valid(); it.Next() {
			item := it.Item()
			var schema types.TableSchema
			if err := item.Value(func(val []byte) error {
				return json.Unmarshal(val, &schema)
			}); err != nil {
				return err
			}
			s.schemas[schema.Name] = schema
		}
		return nil
	})
}

// parseEventKey extracts block number and log index from a primary or reverse key.
func (s *BadgerStore) parseEventKey(key []byte, evt *types.IndexedEvent, reverse bool) {
	// Key format: prefix{table}:{8-byte block}:{8-byte logIndex}
	// Parse from the end using fixed offsets to avoid mis-splitting on ':'
	// bytes (0x3A) that may appear inside the 8-byte binary encodings.
	var prefixLen int
	if reverse {
		prefixLen = len(prefixRev)
	} else {
		prefixLen = len(prefixEvt)
	}

	rest := key[prefixLen:]

	// Fixed suffix: ':'(1) + block(8) + ':'(1) + logIndex(8) = 18 bytes
	if len(rest) < 18 {
		return
	}

	logBytes := rest[len(rest)-8:]
	blockBytes := rest[len(rest)-17 : len(rest)-9]

	block := binary.BigEndian.Uint64(blockBytes)
	if reverse {
		block = invertBlock(block)
	}
	evt.BlockNumber = block
	evt.LogIndex = binary.BigEndian.Uint64(logBytes)

	s.populateFromData(evt)
}

func (s *BadgerStore) populateFromData(evt *types.IndexedEvent) {
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
	case string:
		u, err := strconv.ParseUint(n, 10, 64)
		if err != nil {
			return 0
		}
		return u
	default:
		return 0
	}
}
