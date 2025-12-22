# EventHub Implementation Plan

This document provides a step-by-step implementation guide for the EventHub package. The EventHub acts as a simple message broker based on database persistence with in-memory queue delivery.

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                           EventHub                                  │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────────────┐  │
│  │   Topics     │    │   States     │    │   Subscriptions      │  │
│  │   Registry   │───▶│   Table      │    │   (Channels)         │  │
│  │              │    │              │    │                      │  │
│  └──────────────┘    └──────────────┘    └──────────────────────┘  │
│         │                                         ▲                 │
│         │                                         │                 │
│         ▼                                         │                 │
│  ┌──────────────┐    ┌──────────────┐            │                 │
│  │   Internal   │    │   Events     │────────────┘                 │
│  │   Queue      │───▶│   Table      │   (background dispatcher)    │
│  │              │    │              │                               │
│  └──────────────┘    └──────────────┘                               │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

**Key Design Decisions:**
1. **atomic operations**: States and Events tables are updated atomic (unlike sync package)
2. **Push-based delivery**: Uses channels to push events to subscribers (unlike sync's polling)
3. **Internal queuing**: Events are buffered in-memory before DB persistence
4. **Batch delivery**: Subscribers receive events as arrays for efficient processing

---

## Step 1: Define Types and Interfaces

**File**: `pkg/eventhub/types.go`

Create the core types and interface definition.

```go
package eventhub

import (
	"context"
	"database/sql"
	"time"

	"go.uber.org/zap"
)

// TopicName represents a unique topic identifier
type TopicName string

// Event represents a single event in the hub
type Event struct {
	ID                  int64
	TopicName           TopicName
	ProcessedTimestamp  time.Time // When event was recorded in DB
	OriginatedTimestamp time.Time // When event was created
	EventData           []byte    // JSON serialized payload
}

// TopicState represents the version state for a topic
type TopicState struct {
	TopicName TopicName
	VersionID string    // UUID that changes on every modification
	UpdatedAt time.Time
}

// EventHub is the main interface for the message broker
type EventHub interface {
	// Initialize sets up database connections and starts background workers
	Initialize(ctx context.Context) error

	// RegisterTopic registers a topic with specified queue size
	// Returns error if the events table for this topic does not exist
	RegisterTopic(topicName TopicName, queueSize int) error

	// PublishEvent publishes an event to a topic
	// Updates the states table and events table (non-atomically)
	PublishEvent(ctx context.Context, topicName TopicName, eventData []byte) error

	// RegisterSubscription registers a channel to receive events for a topic
	// Events are delivered as batches (arrays)
	RegisterSubscription(topicName TopicName, eventChan chan<- []Event) error

	// CleanUpEvents removes events between the specified time range
	CleanUpEvents(ctx context.Context, timeFrom, timeEnd time.Time) error

	// Close gracefully shuts down the EventHub
	Close() error
}

// Config holds EventHub configuration
type Config struct {
	// BatchSize is the number of events to batch before sending to subscribers
	BatchSize int
	// FlushInterval is how often to flush partial batches
	FlushInterval time.Duration
	// CleanupInterval is how often to run automatic cleanup
	CleanupInterval time.Duration
	// RetentionPeriod is how long to keep events (default 1 hour)
	RetentionPeriod time.Duration
}

// DefaultConfig returns sensible defaults
func DefaultConfig() Config {
	return Config{
		BatchSize:       100,
		FlushInterval:   time.Second * 5,
		CleanupInterval: time.Minute * 10,
		RetentionPeriod: time.Hour,
	}
}
```

**Checklist:**
- [ ] Create `types.go` with Event, TopicState, TopicName types
- [ ] Define EventHub interface with all methods
- [ ] Define Config struct with defaults
- [ ] Add imports for context, database/sql, time, zap

---

## Step 2: Define Topic Registry

**File**: `pkg/eventhub/topic.go`

Create the internal topic registry that manages topics and their queues.

```go
package eventhub

import (
	"errors"
	"sync"
)

var (
	ErrTopicNotFound      = errors.New("topic not found")
	ErrTopicAlreadyExists = errors.New("topic already registered")
	ErrTopicTableMissing  = errors.New("events table for topic does not exist")
	ErrInvalidQueueSize   = errors.New("queue size must be positive")
)

// topic represents an internal topic with its queue and subscriptions
type topic struct {
	name         TopicName
	queue        chan Event       // Internal buffer queue
	queueSize    int
	subscribers  []chan<- []Event // Registered subscription channels
	subscriberMu sync.RWMutex
}

// topicRegistry manages all registered topics
type topicRegistry struct {
	topics map[TopicName]*topic
	mu     sync.RWMutex
}

// newTopicRegistry creates a new topic registry
func newTopicRegistry() *topicRegistry {
	return &topicRegistry{
		topics: make(map[TopicName]*topic),
	}
}

// register adds a new topic to the registry
func (r *topicRegistry) register(name TopicName, queueSize int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if queueSize <= 0 {
		return ErrInvalidQueueSize
	}

	if _, exists := r.topics[name]; exists {
		return ErrTopicAlreadyExists
	}

	r.topics[name] = &topic{
		name:        name,
		queue:       make(chan Event, queueSize),
		queueSize:   queueSize,
		subscribers: make([]chan<- []Event, 0),
	}

	return nil
}

// get retrieves a topic by name
func (r *topicRegistry) get(name TopicName) (*topic, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	t, exists := r.topics[name]
	if !exists {
		return nil, ErrTopicNotFound
	}
	return t, nil
}

// addSubscriber adds a subscription channel to a topic
func (r *topicRegistry) addSubscriber(name TopicName, ch chan<- []Event) error {
	r.mu.RLock()
	t, exists := r.topics[name]
	r.mu.RUnlock()

	if !exists {
		return ErrTopicNotFound
	}

	t.subscriberMu.Lock()
	defer t.subscriberMu.Unlock()
	t.subscribers = append(t.subscribers, ch)
	return nil
}

// enqueue adds an event to a topic's internal queue
func (r *topicRegistry) enqueue(name TopicName, event Event) error {
	t, err := r.get(name)
	if err != nil {
		return err
	}

	select {
	case t.queue <- event:
		return nil
	default:
		// Queue is full - this is a design decision
		// Option 1: Drop event (current)
		// Option 2: Block
		// Option 3: Return error
		return errors.New("queue is full, event dropped")
	}
}

// getAll returns all registered topics
func (r *topicRegistry) getAll() []*topic {
	r.mu.RLock()
	defer r.mu.RUnlock()

	topics := make([]*topic, 0, len(r.topics))
	for _, t := range r.topics {
		topics = append(topics, t)
	}
	return topics
}
```

**Checklist:**
- [ ] Create `topic.go` with topic and topicRegistry types
- [ ] Implement register, get, addSubscriber, enqueue methods
- [ ] Add proper error definitions
- [ ] Use sync.RWMutex for thread safety

---

## Step 3: Implement Database Operations

**File**: `pkg/eventhub/store.go`

Create the database layer for states and events.

```go
package eventhub

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// store handles database operations for EventHub
type store struct {
	db     *sql.DB
	logger *zap.Logger
}

// newStore creates a new database store
func newStore(db *sql.DB, logger *zap.Logger) *store {
	return &store{
		db:     db,
		logger: logger,
	}
}

// tableExists checks if an events table exists for a topic
func (s *store) tableExists(ctx context.Context, topicName TopicName) (bool, error) {
	tableName := s.getEventsTableName(topicName)

	query := `SELECT name FROM sqlite_master WHERE type='table' AND name=?`
	var name string
	err := s.db.QueryRowContext(ctx, query, tableName).Scan(&name)

	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to check table existence: %w", err)
	}
	return true, nil
}

// getEventsTableName returns the events table name for a topic
func (s *store) getEventsTableName(topicName TopicName) string {
	// Sanitize topic name for SQL table name
	return fmt.Sprintf("%s_events", string(topicName))
}

// initializeTopicState creates an empty state entry for a topic
func (s *store) initializeTopicState(ctx context.Context, topicName TopicName) error {
	query := `
		INSERT INTO topic_states (topic_name, version_id, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(topic_name)
		DO NOTHING
	`

	_, err := s.db.ExecContext(ctx, query, string(topicName), "", time.Now())
	if err != nil {
		return fmt.Errorf("failed to initialize topic state: %w", err)
	}
	return nil
}

// updateState updates the version for a topic (non-atomic, separate from event recording)
func (s *store) updateState(ctx context.Context, topicName TopicName) (string, error) {
	newVersion := uuid.New().String()
	now := time.Now()

	query := `
		INSERT INTO topic_states (topic_name, version_id, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(topic_name)
		DO UPDATE SET version_id = excluded.version_id, updated_at = excluded.updated_at
	`

	_, err := s.db.ExecContext(ctx, query, string(topicName), newVersion, now)
	if err != nil {
		return "", fmt.Errorf("failed to update state: %w", err)
	}

	s.logger.Debug("Updated topic state",
		zap.String("topic", string(topicName)),
		zap.String("version", newVersion),
	)

	return newVersion, nil
}

// recordEvent inserts an event into the events table (non-atomic, separate from state update)
func (s *store) recordEvent(ctx context.Context, topicName TopicName, event *Event) (int64, error) {
	tableName := s.getEventsTableName(topicName)

	query := fmt.Sprintf(`
		INSERT INTO %s (processed_timestamp, originated_timestamp, event_data)
		VALUES (?, ?, ?)
	`, tableName)

	result, err := s.db.ExecContext(ctx, query,
		event.ProcessedTimestamp,
		event.OriginatedTimestamp,
		event.EventData,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to record event: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get event ID: %w", err)
	}

	s.logger.Debug("Recorded event",
		zap.String("topic", string(topicName)),
		zap.Int64("id", id),
	)

	return id, nil
}

// getEventsSince retrieves events after a given timestamp
func (s *store) getEventsSince(ctx context.Context, topicName TopicName, since time.Time) ([]Event, error) {
	tableName := s.getEventsTableName(topicName)

	query := fmt.Sprintf(`
		SELECT id, processed_timestamp, originated_timestamp, event_data
		FROM %s
		WHERE processed_timestamp > ?
		ORDER BY processed_timestamp ASC
	`, tableName)

	rows, err := s.db.QueryContext(ctx, query, since)
	if err != nil {
		return nil, fmt.Errorf("failed to query events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		e.TopicName = topicName
		if err := rows.Scan(&e.ID, &e.ProcessedTimestamp, &e.OriginatedTimestamp, &e.EventData); err != nil {
			return nil, fmt.Errorf("failed to scan event: %w", err)
		}
		events = append(events, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating events: %w", err)
	}

	return events, nil
}

// cleanupEvents removes events within the specified time range
func (s *store) cleanupEvents(ctx context.Context, topicName TopicName, timeFrom, timeEnd time.Time) (int64, error) {
	tableName := s.getEventsTableName(topicName)

	query := fmt.Sprintf(`
		DELETE FROM %s
		WHERE processed_timestamp >= ? AND processed_timestamp <= ?
	`, tableName)

	result, err := s.db.ExecContext(ctx, query, timeFrom, timeEnd)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup events: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get deleted count: %w", err)
	}

	s.logger.Info("Cleaned up events",
		zap.String("topic", string(topicName)),
		zap.Int64("deleted", deleted),
		zap.Time("from", timeFrom),
		zap.Time("to", timeEnd),
	)

	return deleted, nil
}

// cleanupAllTopics removes old events from all known topics
func (s *store) cleanupAllTopics(ctx context.Context, olderThan time.Time) error {
	// Get all topic names from states table
	rows, err := s.db.QueryContext(ctx, `SELECT topic_name FROM topic_states`)
	if err != nil {
		return fmt.Errorf("failed to query topics: %w", err)
	}
	defer rows.Close()

	var topics []TopicName
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("failed to scan topic name: %w", err)
		}
		topics = append(topics, TopicName(name))
	}

	// Cleanup each topic
	for _, topic := range topics {
		_, err := s.cleanupEvents(ctx, topic, time.Time{}, olderThan)
		if err != nil {
			s.logger.Warn("Failed to cleanup topic events",
				zap.String("topic", string(topic)),
				zap.Error(err),
			)
			// Continue with other topics
		}
	}

	return nil
}
```

**Checklist:**
- [ ] Create `store.go` with store struct
- [ ] Implement tableExists to validate topic tables
- [ ] Implement initializeTopicState for empty state creation
- [ ] Implement updateState (separate, non-atomic)
- [ ] Implement recordEvent (separate, non-atomic)
- [ ] Implement getEventsSince for querying events
- [ ] Implement cleanupEvents and cleanupAllTopics
- [ ] Use proper SQL parameterization to prevent injection

---

## Step 4: Implement Event Dispatcher

**File**: `pkg/eventhub/dispatcher.go`

Create the background dispatcher that delivers events to subscribers.

```go
package eventhub

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// dispatcher handles background event delivery to subscribers
type dispatcher struct {
	registry      *topicRegistry
	config        Config
	logger        *zap.Logger

	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
}

// newDispatcher creates a new event dispatcher
func newDispatcher(registry *topicRegistry, config Config, logger *zap.Logger) *dispatcher {
	return &dispatcher{
		registry: registry,
		config:   config,
		logger:   logger,
	}
}

// start begins the dispatcher background workers
func (d *dispatcher) start(ctx context.Context) {
	d.ctx, d.cancel = context.WithCancel(ctx)

	// Start a dispatcher goroutine for each topic
	for _, t := range d.registry.getAll() {
		d.wg.Add(1)
		go d.dispatchLoop(t)
	}

	d.logger.Info("Dispatcher started")
}

// startForTopic starts dispatcher for a newly registered topic
func (d *dispatcher) startForTopic(t *topic) {
	if d.ctx == nil {
		return // Dispatcher not started yet
	}

	d.wg.Add(1)
	go d.dispatchLoop(t)
}

// dispatchLoop processes events from a topic's queue and delivers to subscribers
func (d *dispatcher) dispatchLoop(t *topic) {
	defer d.wg.Done()

	batch := make([]Event, 0, d.config.BatchSize)
	flushTicker := time.NewTicker(d.config.FlushInterval)
	defer flushTicker.Stop()

	for {
		select {
		case <-d.ctx.Done():
			// Flush remaining events before exit
			if len(batch) > 0 {
				d.deliverBatch(t, batch)
			}
			return

		case event := <-t.queue:
			batch = append(batch, event)
			if len(batch) >= d.config.BatchSize {
				d.deliverBatch(t, batch)
				batch = make([]Event, 0, d.config.BatchSize)
			}

		case <-flushTicker.C:
			if len(batch) > 0 {
				d.deliverBatch(t, batch)
				batch = make([]Event, 0, d.config.BatchSize)
			}
		}
	}
}

// deliverBatch sends a batch of events to all subscribers
func (d *dispatcher) deliverBatch(t *topic, batch []Event) {
	t.subscriberMu.RLock()
	subscribers := make([]chan<- []Event, len(t.subscribers))
	copy(subscribers, t.subscribers)
	t.subscriberMu.RUnlock()

	if len(subscribers) == 0 {
		d.logger.Debug("No subscribers for topic",
			zap.String("topic", string(t.name)),
			zap.Int("events", len(batch)),
		)
		return
	}

	// Make a copy of the batch for each subscriber
	batchCopy := make([]Event, len(batch))
	copy(batchCopy, batch)

	for _, ch := range subscribers {
		select {
		case ch <- batchCopy:
			d.logger.Debug("Delivered batch to subscriber",
				zap.String("topic", string(t.name)),
				zap.Int("events", len(batchCopy)),
			)
		default:
			d.logger.Warn("Subscriber channel full, dropping batch",
				zap.String("topic", string(t.name)),
				zap.Int("events", len(batchCopy)),
			)
		}
	}
}

// stop gracefully stops the dispatcher
func (d *dispatcher) stop() {
	if d.cancel != nil {
		d.cancel()
	}
	d.wg.Wait()
	d.logger.Info("Dispatcher stopped")
}
```

**Checklist:**
- [ ] Create `dispatcher.go` with dispatcher struct
- [ ] Implement start/stop lifecycle methods
- [ ] Implement dispatchLoop with batching logic
- [ ] Implement deliverBatch to send to all subscribers
- [ ] Handle graceful shutdown with remaining event flush
- [ ] Use non-blocking sends to prevent subscriber blocking

---

## Step 5: Implement Main EventHub

**File**: `pkg/eventhub/eventhub.go`

Implement the main EventHub that ties everything together.

```go
package eventhub

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// eventHub is the main implementation of EventHub interface
type eventHub struct {
	db         *sql.DB
	store      *store
	registry   *topicRegistry
	dispatcher *dispatcher
	config     Config
	logger     *zap.Logger

	cleanupCtx    context.Context
	cleanupCancel context.CancelFunc
	wg            sync.WaitGroup
	initialized   bool
	mu            sync.RWMutex
}

// New creates a new EventHub instance
func New(db *sql.DB, logger *zap.Logger, config Config) EventHub {
	registry := newTopicRegistry()

	return &eventHub{
		db:       db,
		store:    newStore(db, logger),
		registry: registry,
		config:   config,
		logger:   logger,
	}
}

// Initialize sets up the EventHub and starts background workers
func (eh *eventHub) Initialize(ctx context.Context) error {
	eh.mu.Lock()
	defer eh.mu.Unlock()

	if eh.initialized {
		return nil
	}

	eh.logger.Info("Initializing EventHub")

	// Create dispatcher
	eh.dispatcher = newDispatcher(eh.registry, eh.config, eh.logger)
	eh.dispatcher.start(ctx)

	// Start cleanup goroutine
	eh.cleanupCtx, eh.cleanupCancel = context.WithCancel(ctx)
	eh.wg.Add(1)
	go eh.cleanupLoop()

	eh.initialized = true
	eh.logger.Info("EventHub initialized successfully")
	return nil
}

// RegisterTopic registers a new topic with the EventHub
func (eh *eventHub) RegisterTopic(topicName TopicName, queueSize int) error {
	eh.mu.RLock()
	initialized := eh.initialized
	eh.mu.RUnlock()

	ctx := context.Background()

	// Check if events table exists
	exists, err := eh.store.tableExists(ctx, topicName)
	if err != nil {
		return fmt.Errorf("failed to check table existence: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: table %s does not exist",
			ErrTopicTableMissing, eh.store.getEventsTableName(topicName))
	}

	// Register topic in registry
	if err := eh.registry.register(topicName, queueSize); err != nil {
		return err
	}

	// Initialize empty state in database
	if err := eh.store.initializeTopicState(ctx, topicName); err != nil {
		return fmt.Errorf("failed to initialize state: %w", err)
	}

	// Start dispatcher for this topic if already initialized
	if initialized {
		t, _ := eh.registry.get(topicName)
		eh.dispatcher.startForTopic(t)
	}

	eh.logger.Info("Topic registered",
		zap.String("topic", string(topicName)),
		zap.Int("queueSize", queueSize),
	)

	return nil
}

// PublishEvent publishes an event to a topic
// Note: States and Events are updated NON-ATOMICALLY (separate operations)
func (eh *eventHub) PublishEvent(ctx context.Context, topicName TopicName, eventData []byte) error {
	// Verify topic is registered
	_, err := eh.registry.get(topicName)
	if err != nil {
		return err
	}

	now := time.Now()
	event := &Event{
		TopicName:           topicName,
		ProcessedTimestamp:  now,
		OriginatedTimestamp: now,
		EventData:           eventData,
	}

	// Step 1: Record event in database (NON-ATOMIC)
	id, err := eh.store.recordEvent(ctx, topicName, event)
	if err != nil {
		return fmt.Errorf("failed to record event: %w", err)
	}
	event.ID = id

	// Step 2: Update state version (NON-ATOMIC, separate operation)
	_, err = eh.store.updateState(ctx, topicName)
	if err != nil {
		// Log warning but don't fail - event is already recorded
		eh.logger.Warn("Failed to update state after recording event",
			zap.String("topic", string(topicName)),
			zap.Int64("eventId", id),
			zap.Error(err),
		)
	}

	// Step 3: Enqueue for delivery to subscribers
	if err := eh.registry.enqueue(topicName, *event); err != nil {
		eh.logger.Warn("Failed to enqueue event for delivery",
			zap.String("topic", string(topicName)),
			zap.Int64("eventId", id),
			zap.Error(err),
		)
		// Don't fail - event is persisted, just not delivered via channel
	}

	eh.logger.Debug("Event published",
		zap.String("topic", string(topicName)),
		zap.Int64("id", id),
	)

	return nil
}

// RegisterSubscription registers a channel to receive events for a topic
func (eh *eventHub) RegisterSubscription(topicName TopicName, eventChan chan<- []Event) error {
	if err := eh.registry.addSubscriber(topicName, eventChan); err != nil {
		return err
	}

	eh.logger.Info("Subscription registered",
		zap.String("topic", string(topicName)),
	)

	return nil
}

// CleanUpEvents removes events within the specified time range
func (eh *eventHub) CleanUpEvents(ctx context.Context, timeFrom, timeEnd time.Time) error {
	for _, t := range eh.registry.getAll() {
		_, err := eh.store.cleanupEvents(ctx, t.name, timeFrom, timeEnd)
		if err != nil {
			eh.logger.Error("Failed to cleanup events for topic",
				zap.String("topic", string(t.name)),
				zap.Error(err),
			)
			// Continue with other topics
		}
	}
	return nil
}

// cleanupLoop runs periodic cleanup of old events
func (eh *eventHub) cleanupLoop() {
	defer eh.wg.Done()

	ticker := time.NewTicker(eh.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-eh.cleanupCtx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-eh.config.RetentionPeriod)
			if err := eh.store.cleanupAllTopics(eh.cleanupCtx, cutoff); err != nil {
				eh.logger.Error("Periodic cleanup failed", zap.Error(err))
			}
		}
	}
}

// Close gracefully shuts down the EventHub
func (eh *eventHub) Close() error {
	eh.mu.Lock()
	defer eh.mu.Unlock()

	if !eh.initialized {
		return nil
	}

	eh.logger.Info("Shutting down EventHub")

	// Stop cleanup loop
	if eh.cleanupCancel != nil {
		eh.cleanupCancel()
	}

	// Stop dispatcher
	if eh.dispatcher != nil {
		eh.dispatcher.stop()
	}

	// Wait for goroutines
	eh.wg.Wait()

	eh.initialized = false
	eh.logger.Info("EventHub shutdown complete")
	return nil
}
```

**Checklist:**
- [ ] Create `eventhub.go` with eventHub struct implementing EventHub interface
- [ ] Implement New() constructor
- [ ] Implement Initialize() to set up connections and start workers
- [ ] Implement RegisterTopic() with table existence check and state initialization
- [ ] Implement PublishEvent() with NON-ATOMIC state and event updates
- [ ] Implement RegisterSubscription() for channel registration
- [ ] Implement CleanUpEvents() for manual cleanup
- [ ] Implement cleanupLoop() for automatic periodic cleanup
- [ ] Implement Close() for graceful shutdown

---

## Step 6: Add SQL Schema

**File**: `pkg/storage/gateway-controller-db.sql` (add to existing file)

Add the required table schemas for EventHub.

```sql
-- EventHub: Topic States Table
-- Tracks version information per topic for change detection
CREATE TABLE IF NOT EXISTS topic_states (
    topic_name TEXT PRIMARY KEY,
    version_id TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_topic_states_updated ON topic_states(updated_at);

-- EventHub: Example events table template
-- Each topic needs its own events table following this pattern:
-- Table name format: {topic_name}_events
--
-- Example for 'api' topic:
-- CREATE TABLE IF NOT EXISTS api_events (
--     id INTEGER PRIMARY KEY AUTOINCREMENT,
--     processed_timestamp TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
--     originated_timestamp TIMESTAMP NOT NULL,
--     event_data TEXT NOT NULL
-- );
-- CREATE INDEX IF NOT EXISTS idx_api_events_processed ON api_events(processed_timestamp);
```

**Checklist:**
- [ ] Add topic_states table schema to SQL file
- [ ] Add documentation comment for events table template
- [ ] Add appropriate indexes

---

## Step 7: Write Unit Tests

**File**: `pkg/eventhub/eventhub_test.go`

Create comprehensive unit tests.

```go
package eventhub

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func setupTestDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)

	// Create topic_states table
	_, err = db.Exec(`
		CREATE TABLE topic_states (
			topic_name TEXT PRIMARY KEY,
			version_id TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`)
	require.NoError(t, err)

	// Create test events table
	_, err = db.Exec(`
		CREATE TABLE test_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			processed_timestamp TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			originated_timestamp TIMESTAMP NOT NULL,
			event_data TEXT NOT NULL
		)
	`)
	require.NoError(t, err)

	return db
}

func TestEventHub_RegisterTopic(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	logger := zap.NewNop()
	hub := New(db, logger, DefaultConfig())

	err := hub.Initialize(context.Background())
	require.NoError(t, err)
	defer hub.Close()

	// Test successful registration
	err = hub.RegisterTopic("test", 100)
	assert.NoError(t, err)

	// Test duplicate registration
	err = hub.RegisterTopic("test", 100)
	assert.ErrorIs(t, err, ErrTopicAlreadyExists)

	// Test missing table
	err = hub.RegisterTopic("nonexistent", 100)
	assert.ErrorIs(t, err, ErrTopicTableMissing)
}

func TestEventHub_PublishAndSubscribe(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	logger := zap.NewNop()
	config := DefaultConfig()
	config.FlushInterval = 100 * time.Millisecond
	hub := New(db, logger, config)

	err := hub.Initialize(context.Background())
	require.NoError(t, err)
	defer hub.Close()

	err = hub.RegisterTopic("test", 100)
	require.NoError(t, err)

	// Register subscription
	eventChan := make(chan []Event, 10)
	err = hub.RegisterSubscription("test", eventChan)
	require.NoError(t, err)

	// Publish event
	data, _ := json.Marshal(map[string]string{"key": "value"})
	err = hub.PublishEvent(context.Background(), "test", data)
	require.NoError(t, err)

	// Wait for event delivery
	select {
	case events := <-eventChan:
		assert.Len(t, events, 1)
		assert.Equal(t, TopicName("test"), events[0].TopicName)
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for event")
	}
}

func TestEventHub_CleanUpEvents(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	logger := zap.NewNop()
	hub := New(db, logger, DefaultConfig())

	err := hub.Initialize(context.Background())
	require.NoError(t, err)
	defer hub.Close()

	err = hub.RegisterTopic("test", 100)
	require.NoError(t, err)

	// Publish events
	for i := 0; i < 5; i++ {
		data, _ := json.Marshal(map[string]int{"index": i})
		err = hub.PublishEvent(context.Background(), "test", data)
		require.NoError(t, err)
	}

	// Cleanup all events
	err = hub.CleanUpEvents(context.Background(), time.Time{}, time.Now().Add(time.Hour))
	require.NoError(t, err)

	// Verify events are deleted
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM test_events").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}
```

**Checklist:**
- [ ] Create `eventhub_test.go` with test setup helpers
- [ ] Test RegisterTopic success and error cases
- [ ] Test PublishEvent and subscription delivery
- [ ] Test CleanUpEvents functionality
- [ ] Test graceful shutdown

---

## Implementation Order Summary

Execute these steps in order for a clean implementation:

| Step | File | Description | Dependencies |
|------|------|-------------|--------------|
| 1 | `types.go` | Core types and interface | None |
| 2 | `topic.go` | Topic registry | Step 1 |
| 3 | `store.go` | Database operations | Step 1 |
| 4 | `dispatcher.go` | Background event delivery | Steps 1, 2 |
| 5 | `eventhub.go` | Main implementation | Steps 1-4 |
| 6 | SQL schema | Database tables | None (can be done first) |
| 7 | `eventhub_test.go` | Unit tests | Steps 1-5 |

---

## Key Implementation Notes

### Non-Atomic Design
Unlike the sync package which uses transactions to ensure atomicity, EventHub explicitly separates state and event updates:

```go
// Step 1: Record event (can succeed)
id, err := eh.store.recordEvent(ctx, topicName, event)

// Step 2: Update state (separate operation, may fail independently)
_, err = eh.store.updateState(ctx, topicName)
```

This is intentional per requirements. If state update fails after event recording, the event is still persisted and delivered.

### Queue Behavior
The internal queue has overflow behavior:
- Default: Drop events when queue is full
- Alternative: Could be changed to block or return error

### Batch Delivery
Events are delivered to subscribers as batches (`[]Event`) based on:
1. **BatchSize**: When batch reaches configured size
2. **FlushInterval**: Periodic flush of partial batches

### Thread Safety
- `topicRegistry` uses `sync.RWMutex` for concurrent access
- `topic.subscribers` has its own lock for subscriber modifications
- `eventHub` has a mutex for initialization state

---

## References

Patterns referenced from sync package (do not import):
- `pkg/sync/types.go` - Event and state structures
- `pkg/sync/state_manager.go` - UPSERT pattern for states
- `pkg/sync/event_store.go` - Event recording and querying
- `pkg/sync/sync_poller.go` - Background cleanup loop pattern
