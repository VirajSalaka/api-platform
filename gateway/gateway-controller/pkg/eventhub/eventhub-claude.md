# EventHub Implementation Plan

This document provides a step-by-step implementation guide for the EventHub package. The EventHub acts as a simple message broker based on database persistence with polling-based delivery.

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                           EventHub                                  │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────────────┐  │
│  │   Topics     │    │   States     │◄───│   Poller             │  │
│  │   Registry   │───▶│   Table      │    │   (polls for changes)│  │
│  │              │    │              │    │                      │  │
│  └──────────────┘    └──────────────┘    └──────────┬───────────┘  │
│         │                                           │               │
│         │                                           │ on change     │
│         ▼                                           ▼               │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────────────┐  │
│  │   Publish    │───▶│   Events     │───▶│   Subscriptions      │  │
│  │   (DB write) │    │   Table      │    │   (Channels)         │  │
│  └──────────────┘    └──────────────┘    └──────────────────────┘  │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

**Key Design Decisions:**
1. **Atomic operations**: States and Events tables are updated atomically
2. **Poll-based delivery**: Background poller detects state changes, fetches events from DB, delivers to subscribers
3. **No in-memory queue**: Events go directly to DB; poller fetches and delivers
4. **Natural batching**: Events fetched during each poll cycle form the batch (no separate batching mechanism)

---

## Step 1: Define Types and Interfaces

**File**: `pkg/eventhub/types.go`

Create the core types and interface definition.

```go
package eventhub

import (
	"context"
	"time"
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
	// Initialize sets up database connections and starts background poller
	Initialize(ctx context.Context) error

	// RegisterTopic registers a topic
	// Returns error if the events table for this topic does not exist
	// Creates entry in States table with empty version
	RegisterTopic(topicName TopicName) error

	// PublishEvent publishes an event to a topic
	// Updates the states table and events table atomically
	PublishEvent(ctx context.Context, topicName TopicName, eventData []byte) error

	// RegisterSubscription registers a channel to receive events for a topic
	// Events are delivered as batches (arrays) based on poll cycle
	RegisterSubscription(topicName TopicName, eventChan chan<- []Event) error

	// CleanUpEvents removes events between the specified time range
	CleanUpEvents(ctx context.Context, timeFrom, timeEnd time.Time) error

	// Close gracefully shuts down the EventHub
	Close() error
}

// Config holds EventHub configuration
type Config struct {
	// PollInterval is how often to poll for state changes
	PollInterval time.Duration
	// CleanupInterval is how often to run automatic cleanup
	CleanupInterval time.Duration
	// RetentionPeriod is how long to keep events (default 1 hour)
	RetentionPeriod time.Duration
}

// DefaultConfig returns sensible defaults
func DefaultConfig() Config {
	return Config{
		PollInterval:    time.Second * 5,
		CleanupInterval: time.Minute * 10,
		RetentionPeriod: time.Hour,
	}
}
```

**Checklist:**
- [ ] Create `types.go` with Event, TopicState, TopicName types
- [ ] Define EventHub interface with all methods
- [ ] Define Config struct with PollInterval, CleanupInterval, RetentionPeriod
- [ ] Add imports for context, time

---

## Step 2: Define Topic Registry

**File**: `pkg/eventhub/topic.go`

Create the internal topic registry that manages topics and their subscriptions.

```go
package eventhub

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrTopicNotFound      = errors.New("topic not found")
	ErrTopicAlreadyExists = errors.New("topic already registered")
	ErrTopicTableMissing  = errors.New("events table for topic does not exist")
)

// topic represents an internal topic with its subscriptions and poll state
type topic struct {
	name         TopicName
	subscribers  []chan<- []Event // Registered subscription channels
	subscriberMu sync.RWMutex

	// Polling state
	knownVersion string    // Last known version from states table
	lastPolled   time.Time // Timestamp of last successful poll
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
func (r *topicRegistry) register(name TopicName) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.topics[name]; exists {
		return ErrTopicAlreadyExists
	}

	r.topics[name] = &topic{
		name:        name,
		subscribers: make([]chan<- []Event, 0),
		lastPolled:  time.Now(), // Start from now, don't replay old events
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

// updatePollState updates the polling state for a topic
func (t *topic) updatePollState(version string, polledAt time.Time) {
	t.knownVersion = version
	t.lastPolled = polledAt
}

// getSubscribers returns a copy of the subscribers list
func (t *topic) getSubscribers() []chan<- []Event {
	t.subscriberMu.RLock()
	defer t.subscriberMu.RUnlock()

	subs := make([]chan<- []Event, len(t.subscribers))
	copy(subs, t.subscribers)
	return subs
}
```

**Checklist:**
- [ ] Create `topic.go` with topic and topicRegistry types
- [ ] Implement register, get, addSubscriber, getAll methods
- [ ] Add knownVersion and lastPolled fields for polling state
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

// getState retrieves the current state for a topic
func (s *store) getState(ctx context.Context, topicName TopicName) (*TopicState, error) {
	query := `
		SELECT topic_name, version_id, updated_at
		FROM topic_states
		WHERE topic_name = ?
	`

	var state TopicState
	var name string
	err := s.db.QueryRowContext(ctx, query, string(topicName)).Scan(
		&name, &state.VersionID, &state.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get state: %w", err)
	}

	state.TopicName = TopicName(name)
	return &state, nil
}

// publishEventAtomic records an event and updates state in a single transaction
func (s *store) publishEventAtomic(ctx context.Context, topicName TopicName, event *Event) (int64, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Step 1: Insert event
	tableName := s.getEventsTableName(topicName)
	insertQuery := fmt.Sprintf(`
		INSERT INTO %s (processed_timestamp, originated_timestamp, event_data)
		VALUES (?, ?, ?)
	`, tableName)

	result, err := tx.ExecContext(ctx, insertQuery,
		event.ProcessedTimestamp,
		event.OriginatedTimestamp,
		event.EventData,
	)
	if err != nil {
		return 0, "", fmt.Errorf("failed to record event: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, "", fmt.Errorf("failed to get event ID: %w", err)
	}

	// Step 2: Update state version
	newVersion := uuid.New().String()
	now := time.Now()

	updateQuery := `
		INSERT INTO topic_states (topic_name, version_id, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(topic_name)
		DO UPDATE SET version_id = excluded.version_id, updated_at = excluded.updated_at
	`

	_, err = tx.ExecContext(ctx, updateQuery, string(topicName), newVersion, now)
	if err != nil {
		return 0, "", fmt.Errorf("failed to update state: %w", err)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return 0, "", fmt.Errorf("failed to commit transaction: %w", err)
	}

	s.logger.Debug("Published event atomically",
		zap.String("topic", string(topicName)),
		zap.Int64("id", id),
		zap.String("version", newVersion),
	)

	return id, newVersion, nil
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

	for _, topic := range topics {
		_, err := s.cleanupEvents(ctx, topic, time.Time{}, olderThan)
		if err != nil {
			s.logger.Warn("Failed to cleanup topic events",
				zap.String("topic", string(topic)),
				zap.Error(err),
			)
		}
	}

	return nil
}
```

**Checklist:**
- [ ] Create `store.go` with store struct
- [ ] Implement tableExists to validate topic tables
- [ ] Implement initializeTopicState for empty state creation
- [ ] Implement getState to read current version
- [ ] Implement publishEventAtomic with transaction (event + state update)
- [ ] Implement getEventsSince for querying events
- [ ] Implement cleanupEvents and cleanupAllTopics
- [ ] Use proper SQL parameterization to prevent injection

---

## Step 4: Implement Poller

**File**: `pkg/eventhub/poller.go`

Create the background poller that detects state changes and delivers events. Reference: `pkg/sync/sync_poller.go`

```go
package eventhub

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// poller handles background polling for state changes and event delivery
type poller struct {
	store    *store
	registry *topicRegistry
	config   Config
	logger   *zap.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newPoller creates a new event poller
func newPoller(store *store, registry *topicRegistry, config Config, logger *zap.Logger) *poller {
	return &poller{
		store:    store,
		registry: registry,
		config:   config,
		logger:   logger,
	}
}

// start begins the poller background worker
func (p *poller) start(ctx context.Context) {
	p.ctx, p.cancel = context.WithCancel(ctx)

	p.wg.Add(1)
	go p.pollLoop()

	p.logger.Info("Poller started", zap.Duration("interval", p.config.PollInterval))
}

// pollLoop runs the main polling loop
func (p *poller) pollLoop() {
	defer p.wg.Done()

	ticker := time.NewTicker(p.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.pollAllTopics()
		}
	}
}

// pollAllTopics checks all registered topics for state changes
func (p *poller) pollAllTopics() {
	topics := p.registry.getAll()

	for _, t := range topics {
		if err := p.pollTopic(t); err != nil {
			p.logger.Error("Failed to poll topic",
				zap.String("topic", string(t.name)),
				zap.Error(err),
			)
		}
	}
}

// pollTopic checks a single topic for state changes and delivers events
func (p *poller) pollTopic(t *topic) error {
	ctx := p.ctx

	// Get current state from database
	state, err := p.store.getState(ctx, t.name)
	if err != nil {
		return err
	}
	if state == nil {
		// Topic state not initialized yet
		return nil
	}

	// Check if version has changed
	if state.VersionID == t.knownVersion {
		// No changes
		return nil
	}

	p.logger.Debug("State change detected",
		zap.String("topic", string(t.name)),
		zap.String("oldVersion", t.knownVersion),
		zap.String("newVersion", state.VersionID),
	)

	// Fetch events since last poll
	events, err := p.store.getEventsSince(ctx, t.name, t.lastPolled)
	if err != nil {
		return err
	}

	if len(events) > 0 {
		// Deliver events to subscribers
		p.deliverEvents(t, events)
	}

	// Update poll state
	t.updatePollState(state.VersionID, time.Now())

	return nil
}

// deliverEvents sends events to all subscribers of a topic
func (p *poller) deliverEvents(t *topic, events []Event) {
	subscribers := t.getSubscribers()

	if len(subscribers) == 0 {
		p.logger.Debug("No subscribers for topic",
			zap.String("topic", string(t.name)),
			zap.Int("events", len(events)),
		)
		return
	}

	// Deliver to all subscribers
	for _, ch := range subscribers {
		select {
		case ch <- events:
			p.logger.Debug("Delivered events to subscriber",
				zap.String("topic", string(t.name)),
				zap.Int("events", len(events)),
			)
		default:
			p.logger.Warn("Subscriber channel full, dropping events",
				zap.String("topic", string(t.name)),
				zap.Int("events", len(events)),
			)
		}
	}
}

// stop gracefully stops the poller
func (p *poller) stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	p.logger.Info("Poller stopped")
}
```

**Checklist:**
- [ ] Create `poller.go` with poller struct
- [ ] Implement start/stop lifecycle methods
- [ ] Implement pollLoop that runs on PollInterval
- [ ] Implement pollAllTopics to check each registered topic
- [ ] Implement pollTopic with version comparison logic
- [ ] Implement deliverEvents to send to all subscribers
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
	db       *sql.DB
	store    *store
	registry *topicRegistry
	poller   *poller
	config   Config
	logger   *zap.Logger

	cleanupCtx    context.Context
	cleanupCancel context.CancelFunc
	wg            sync.WaitGroup
	initialized   bool
	mu            sync.RWMutex
}

// New creates a new EventHub instance
func New(db *sql.DB, logger *zap.Logger, config Config) EventHub {
	registry := newTopicRegistry()
	store := newStore(db, logger)

	return &eventHub{
		db:       db,
		store:    store,
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

	// Create and start poller
	eh.poller = newPoller(eh.store, eh.registry, eh.config, eh.logger)
	eh.poller.start(ctx)

	// Start cleanup goroutine
	eh.cleanupCtx, eh.cleanupCancel = context.WithCancel(ctx)
	eh.wg.Add(1)
	go eh.cleanupLoop()

	eh.initialized = true
	eh.logger.Info("EventHub initialized successfully",
		zap.Duration("pollInterval", eh.config.PollInterval),
	)
	return nil
}

// RegisterTopic registers a new topic with the EventHub
func (eh *eventHub) RegisterTopic(topicName TopicName) error {
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
	if err := eh.registry.register(topicName); err != nil {
		return err
	}

	// Initialize empty state in database
	if err := eh.store.initializeTopicState(ctx, topicName); err != nil {
		return fmt.Errorf("failed to initialize state: %w", err)
	}

	eh.logger.Info("Topic registered",
		zap.String("topic", string(topicName)),
	)

	return nil
}

// PublishEvent publishes an event to a topic
// Note: States and Events are updated ATOMICALLY in a transaction
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

	// Publish atomically (event + state update in transaction)
	id, version, err := eh.store.publishEventAtomic(ctx, topicName, event)
	if err != nil {
		return fmt.Errorf("failed to publish event: %w", err)
	}

	eh.logger.Debug("Event published",
		zap.String("topic", string(topicName)),
		zap.Int64("id", id),
		zap.String("version", version),
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

	// Stop poller
	if eh.poller != nil {
		eh.poller.stop()
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
- [ ] Implement Initialize() to set up connections and start poller
- [ ] Implement RegisterTopic() with table existence check and state initialization
- [ ] Implement PublishEvent() with ATOMIC state and event updates
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
	err = hub.RegisterTopic("test")
	assert.NoError(t, err)

	// Test duplicate registration
	err = hub.RegisterTopic("test")
	assert.ErrorIs(t, err, ErrTopicAlreadyExists)

	// Test missing table
	err = hub.RegisterTopic("nonexistent")
	assert.ErrorIs(t, err, ErrTopicTableMissing)
}

func TestEventHub_PublishAndSubscribe(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	logger := zap.NewNop()
	config := DefaultConfig()
	config.PollInterval = 100 * time.Millisecond // Fast polling for test
	hub := New(db, logger, config)

	err := hub.Initialize(context.Background())
	require.NoError(t, err)
	defer hub.Close()

	err = hub.RegisterTopic("test")
	require.NoError(t, err)

	// Register subscription
	eventChan := make(chan []Event, 10)
	err = hub.RegisterSubscription("test", eventChan)
	require.NoError(t, err)

	// Publish event
	data, _ := json.Marshal(map[string]string{"key": "value"})
	err = hub.PublishEvent(context.Background(), "test", data)
	require.NoError(t, err)

	// Wait for event delivery via polling
	select {
	case events := <-eventChan:
		assert.GreaterOrEqual(t, len(events), 1)
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

	err = hub.RegisterTopic("test")
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

func TestEventHub_PollerDetectsChanges(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	logger := zap.NewNop()
	config := DefaultConfig()
	config.PollInterval = 50 * time.Millisecond
	hub := New(db, logger, config)

	err := hub.Initialize(context.Background())
	require.NoError(t, err)
	defer hub.Close()

	err = hub.RegisterTopic("test")
	require.NoError(t, err)

	eventChan := make(chan []Event, 10)
	err = hub.RegisterSubscription("test", eventChan)
	require.NoError(t, err)

	// Publish multiple events
	for i := 0; i < 3; i++ {
		data, _ := json.Marshal(map[string]int{"index": i})
		err = hub.PublishEvent(context.Background(), "test", data)
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
	}

	// Wait for events to be delivered
	var receivedEvents []Event
	timeout := time.After(500 * time.Millisecond)

	for {
		select {
		case events := <-eventChan:
			receivedEvents = append(receivedEvents, events...)
			if len(receivedEvents) >= 3 {
				assert.Len(t, receivedEvents, 3)
				return
			}
		case <-timeout:
			t.Fatalf("Timeout: received only %d events", len(receivedEvents))
		}
	}
}
```

**Checklist:**
- [ ] Create `eventhub_test.go` with test setup helpers
- [ ] Test RegisterTopic success and error cases
- [ ] Test PublishEvent and subscription delivery via polling
- [ ] Test CleanUpEvents functionality
- [ ] Test poller detects state changes correctly
- [ ] Test graceful shutdown

---

## Implementation Order Summary

Execute these steps in order for a clean implementation:

| Step | File | Description | Dependencies |
|------|------|-------------|--------------|
| 1 | `types.go` | Core types and interface | None |
| 2 | `topic.go` | Topic registry with poll state | Step 1 |
| 3 | `store.go` | Database operations | Step 1 |
| 4 | `poller.go` | Background state polling and event delivery | Steps 1, 2, 3 |
| 5 | `eventhub.go` | Main implementation | Steps 1-4 |
| 6 | SQL schema | Database tables | None (can be done first) |
| 7 | `eventhub_test.go` | Unit tests | Steps 1-5 |

---

## Key Implementation Notes

### Atomic Design
PublishEvent uses a transaction to ensure event recording and state update are atomic:

```go
tx, err := s.db.BeginTx(ctx, nil)
// Insert event
// Update state
tx.Commit()
```

### Poll-Based Delivery
The poller runs on a configurable interval and:
1. Checks each topic's state version against known version
2. If version changed, fetches events since `lastPolled` timestamp
3. Delivers all fetched events as a batch to subscribers
4. Updates `knownVersion` and `lastPolled`

### Natural Batching
No separate batching mechanism is needed. Events fetched during each poll cycle naturally form a batch based on:
- Poll interval timing
- Events accumulated since last poll

### Thread Safety
- `topicRegistry` uses `sync.RWMutex` for concurrent access
- `topic.subscribers` has its own lock for subscriber modifications
- `eventHub` has a mutex for initialization state
- Poll state (`knownVersion`, `lastPolled`) is updated only by the poller goroutine

---

## References

Patterns referenced from sync package (do not import):
- `pkg/sync/types.go` - Event and state structures
- `pkg/sync/state_manager.go` - UPSERT pattern for states
- `pkg/sync/event_store.go` - Event recording and querying
- `pkg/sync/sync_poller.go` - Background polling loop pattern with version detection
