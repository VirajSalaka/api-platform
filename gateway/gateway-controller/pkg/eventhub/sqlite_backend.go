/*
 * Copyright (c) 2025, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package eventhub

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SQLiteBackend implements EventhubImpl using SQLite polling
type SQLiteBackend struct {
	db     *sql.DB
	logger *slog.Logger
	config SQLiteBackendConfig

	registry *organizationRegistry

	// Prepared statements
	stmtMu              sync.RWMutex
	insertEventStmt     *sql.Stmt
	updateOrgVersionStmt *sql.Stmt
	getOrgStateStmt     *sql.Stmt
	getEventsStmt       *sql.Stmt
	insertOrgStmt       *sql.Stmt
	cleanupEventsStmt   *sql.Stmt

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewSQLiteBackend creates a new SQLite-backed event hub
func NewSQLiteBackend(db *sql.DB, logger *slog.Logger, config SQLiteBackendConfig) *SQLiteBackend {
	ctx, cancel := context.WithCancel(context.Background())
	return &SQLiteBackend{
		db:       db,
		logger:   logger,
		config:   config,
		registry: newOrganizationRegistry(),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Initialize prepares statements and starts background goroutines
func (b *SQLiteBackend) Initialize() error {
	if err := b.prepareStatements(); err != nil {
		return fmt.Errorf("failed to prepare statements: %w", err)
	}

	// Start poll loop
	b.wg.Add(1)
	go b.pollLoop()

	// Start cleanup loop
	b.wg.Add(1)
	go b.cleanupLoop()

	b.logger.Info("SQLite event hub backend initialized",
		slog.Duration("poll_interval", b.config.PollInterval),
		slog.Duration("cleanup_interval", b.config.CleanupInterval),
		slog.Duration("retention_period", b.config.RetentionPeriod))

	return nil
}

// prepareStatements prepares SQL statements for reuse
func (b *SQLiteBackend) prepareStatements() error {
	var err error

	b.insertEventStmt, err = b.db.Prepare(`
		INSERT INTO events (organization_id, processed_timestamp, originated_timestamp, event_type, action, entity_id, correlation_id, event_data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare insert event statement: %w", err)
	}

	b.updateOrgVersionStmt, err = b.db.Prepare(`
		UPDATE organization_states SET version_id = ?, updated_at = CURRENT_TIMESTAMP WHERE organization = ?
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare update org version statement: %w", err)
	}

	b.getOrgStateStmt, err = b.db.Prepare(`
		SELECT organization, version_id, updated_at FROM organization_states WHERE organization = ?
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare get org state statement: %w", err)
	}

	b.getEventsStmt, err = b.db.Prepare(`
		SELECT organization_id, processed_timestamp, originated_timestamp, event_type, action, entity_id, correlation_id, event_data
		FROM events
		WHERE organization_id = ? AND processed_timestamp > ?
		ORDER BY processed_timestamp ASC
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare get events statement: %w", err)
	}

	b.insertOrgStmt, err = b.db.Prepare(`
		INSERT OR IGNORE INTO organization_states (organization, version_id) VALUES (?, '')
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare insert org statement: %w", err)
	}

	b.cleanupEventsStmt, err = b.db.Prepare(`
		DELETE FROM events WHERE processed_timestamp < ?
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare cleanup events statement: %w", err)
	}

	return nil
}

// RegisterOrganization registers a new organization for event tracking
func (b *SQLiteBackend) RegisterOrganization(orgID string) error {
	// Register in database
	_, err := b.insertOrgStmt.Exec(orgID)
	if err != nil {
		return fmt.Errorf("failed to register organization in database: %w", err)
	}

	// Register in local registry (ignore already exists)
	if regErr := b.registry.register(orgID); regErr != nil && regErr != ErrOrganizationAlreadyExists {
		return fmt.Errorf("failed to register organization in registry: %w", regErr)
	}

	b.logger.Info("Organization registered for event tracking", slog.String("organization", orgID))
	return nil
}

// Publish publishes an event atomically (insert event + update org version)
func (b *SQLiteBackend) Publish(orgID string, event Event) error {
	newVersion := uuid.New().String()

	tx, err := b.db.BeginTx(b.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// Insert event (explicitly pass processed_timestamp to ensure consistent time format with Go driver)
	_, err = tx.Stmt(b.insertEventStmt).Exec(
		orgID,
		time.Now(),
		event.OriginatedTimestamp,
		string(event.EventType),
		event.Action,
		event.EntityID,
		event.CorrelationID,
		event.EventData,
	)
	if err != nil {
		return fmt.Errorf("failed to insert event: %w", err)
	}

	// Update organization version
	_, err = tx.Stmt(b.updateOrgVersionStmt).Exec(newVersion, orgID)
	if err != nil {
		return fmt.Errorf("failed to update organization version: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit event publish: %w", err)
	}

	b.logger.Debug("Event published",
		slog.String("organization", orgID),
		slog.String("event_type", string(event.EventType)),
		slog.String("action", event.Action),
		slog.String("entity_id", event.EntityID),
		slog.String("new_version", newVersion))

	return nil
}

// Subscribe subscribes to events for an organization
func (b *SQLiteBackend) Subscribe(orgID string) (<-chan Event, error) {
	ch := make(chan Event, 100)

	if err := b.registry.addSubscriber(orgID, ch); err != nil {
		close(ch)
		return nil, fmt.Errorf("failed to subscribe to organization %s: %w", orgID, err)
	}

	b.logger.Info("Subscribed to organization events", slog.String("organization", orgID))
	return ch, nil
}

// Unsubscribe removes the subscription for an organization
func (b *SQLiteBackend) Unsubscribe(orgID string) error {
	org, err := b.registry.get(orgID)
	if err != nil {
		return err
	}

	// Close and remove all subscribers
	b.registry.mu.Lock()
	defer b.registry.mu.Unlock()

	for _, ch := range org.subscribers {
		close(ch)
	}
	org.subscribers = nil

	return nil
}

// pollLoop periodically checks for new events
func (b *SQLiteBackend) pollLoop() {
	defer b.wg.Done()

	ticker := time.NewTicker(b.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			b.pollOrganizations()
		}
	}
}

// pollOrganizations checks each registered organization for version changes
func (b *SQLiteBackend) pollOrganizations() {
	orgs := b.registry.getAll()
	for _, org := range orgs {
		if err := b.pollOrganization(org); err != nil {
			b.logger.Warn("Failed to poll organization",
				slog.String("organization", org.id),
				slog.Any("error", err))
		}
	}
}

// pollOrganization checks a single organization for version changes
func (b *SQLiteBackend) pollOrganization(org *organization) error {
	var state OrganizationState
	err := b.getOrgStateStmt.QueryRow(org.id).Scan(&state.Organization, &state.VersionID, &state.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil // Organization not yet initialized
		}
		return fmt.Errorf("failed to query organization state: %w", err)
	}

	// Check if version has changed
	b.registry.mu.RLock()
	knownVersion := org.knownVersion
	subscribers := make([]chan Event, len(org.subscribers))
	copy(subscribers, org.subscribers)
	b.registry.mu.RUnlock()

	if state.VersionID == knownVersion || state.VersionID == "" {
		return nil // No changes
	}

	// Fetch new events since last poll
	var lastPolledTime time.Time
	if org.lastPolled > 0 {
		lastPolledTime = time.Unix(0, org.lastPolled)
	} else {
		// First poll - use epoch to catch all events
		lastPolledTime = time.Unix(0, 0)
	}

	rows, err := b.getEventsStmt.Query(org.id, lastPolledTime)
	if err != nil {
		return fmt.Errorf("failed to query events: %w", err)
	}
	defer rows.Close()

	var events []Event
	var latestTimestamp time.Time
	for rows.Next() {
		var evt Event
		var eventType string
		if err := rows.Scan(
			&evt.OrganizationID,
			&evt.ProcessedTimestamp,
			&evt.OriginatedTimestamp,
			&eventType,
			&evt.Action,
			&evt.EntityID,
			&evt.CorrelationID,
			&evt.EventData,
		); err != nil {
			return fmt.Errorf("failed to scan event row: %w", err)
		}
		evt.EventType = EventType(eventType)
		events = append(events, evt)
		if evt.ProcessedTimestamp.After(latestTimestamp) {
			latestTimestamp = evt.ProcessedTimestamp
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating event rows: %w", err)
	}

	// Deliver events to subscribers
	for _, evt := range events {
		for _, ch := range subscribers {
			select {
			case ch <- evt:
			default:
				b.logger.Warn("Subscriber channel full, dropping event",
					slog.String("organization", org.id),
					slog.String("entity_id", evt.EntityID))
			}
		}
	}

	// Update known version and last polled time
	b.registry.mu.Lock()
	org.knownVersion = state.VersionID
	if !latestTimestamp.IsZero() {
		org.lastPolled = latestTimestamp.UnixNano()
	}
	b.registry.mu.Unlock()

	if len(events) > 0 {
		b.logger.Debug("Delivered events to subscribers",
			slog.String("organization", org.id),
			slog.Int("event_count", len(events)),
			slog.Int("subscriber_count", len(subscribers)))
	}

	return nil
}

// cleanupLoop periodically removes old events
func (b *SQLiteBackend) cleanupLoop() {
	defer b.wg.Done()

	ticker := time.NewTicker(b.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			if err := b.Cleanup(b.config.RetentionPeriod); err != nil {
				b.logger.Warn("Failed to clean up events", slog.Any("error", err))
			}
		}
	}
}

// Cleanup removes events older than the retention period
func (b *SQLiteBackend) Cleanup(retentionPeriod time.Duration) error {
	cutoff := time.Now().Add(-retentionPeriod)
	result, err := b.cleanupEventsStmt.Exec(cutoff)
	if err != nil {
		return fmt.Errorf("failed to clean up events: %w", err)
	}

	affected, _ := result.RowsAffected()
	if affected > 0 {
		b.logger.Info("Cleaned up old events", slog.Int64("deleted_count", affected))
	}
	return nil
}

// CleanupRange removes events for an organization before a given time
func (b *SQLiteBackend) CleanupRange(orgID string, before time.Time) error {
	_, err := b.db.Exec(
		"DELETE FROM events WHERE organization_id = ? AND processed_timestamp < ?",
		orgID, before,
	)
	if err != nil {
		return fmt.Errorf("failed to clean up events for organization %s: %w", orgID, err)
	}
	return nil
}

// Close gracefully shuts down the backend
func (b *SQLiteBackend) Close() error {
	b.cancel()
	b.wg.Wait()

	// Close all subscriber channels
	for _, org := range b.registry.getAll() {
		b.registry.mu.Lock()
		for _, ch := range org.subscribers {
			close(ch)
		}
		org.subscribers = nil
		b.registry.mu.Unlock()
	}

	// Close prepared statements
	b.stmtMu.Lock()
	defer b.stmtMu.Unlock()

	stmts := []*sql.Stmt{
		b.insertEventStmt,
		b.updateOrgVersionStmt,
		b.getOrgStateStmt,
		b.getEventsStmt,
		b.insertOrgStmt,
		b.cleanupEventsStmt,
	}
	for _, stmt := range stmts {
		if stmt != nil {
			stmt.Close()
		}
	}

	b.logger.Info("SQLite event hub backend closed")
	return nil
}
