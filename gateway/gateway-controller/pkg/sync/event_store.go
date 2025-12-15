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

package sync

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// sqliteEventStore implements EventStore interface for SQLite
type sqliteEventStore struct {
	db     *sql.DB
	logger *zap.Logger
}

// NewSQLiteEventStore creates a new EventStore for SQLite
func NewSQLiteEventStore(db *sql.DB, logger *zap.Logger) EventStore {
	return &sqliteEventStore{
		db:     db,
		logger: logger,
	}
}

// getTableName returns the events table name for an entity type
func (es *sqliteEventStore) getTableName(entityType EntityType) string {
	switch entityType {
	case EntityTypeAPI:
		return "api_events"
	case EntityTypeCertificate:
		return "certificate_events"
	case EntityTypeLLMTemplate:
		return "llm_template_events"
	default:
		return ""
	}
}

// RecordEvent records an event (must be called within transaction)
func (es *sqliteEventStore) RecordEvent(ctx context.Context, entityType EntityType, event *Event) error {
	tableName := es.getTableName(entityType)
	if tableName == "" {
		return fmt.Errorf("unknown entity type: %s", entityType)
	}

	query := fmt.Sprintf(`INSERT INTO %s
		(originated_timestamp, organization_id, action, entity_id, event_data)
		VALUES (?, ?, ?, ?, ?)`, tableName)

	result, err := es.db.ExecContext(ctx, query,
		event.OriginatedTimestamp,
		event.OrganizationID,
		string(event.Action),
		event.EntityID,
		event.EventData,
	)

	if err != nil {
		return fmt.Errorf("failed to record event for %s: %w", entityType, err)
	}

	// Get the generated event ID
	eventID, err := result.LastInsertId()
	if err == nil {
		event.ID = eventID
	}

	es.logger.Debug("Recorded sync event",
		zap.String("entity_type", string(entityType)),
		zap.String("action", string(event.Action)),
		zap.String("entity_id", event.EntityID),
		zap.Int64("event_id", eventID))

	return nil
}

// GetEventsSince retrieves events since a given timestamp
func (es *sqliteEventStore) GetEventsSince(ctx context.Context, entityType EntityType, orgID string, since time.Time) ([]Event, error) {
	tableName := es.getTableName(entityType)
	if tableName == "" {
		return nil, fmt.Errorf("unknown entity type: %s", entityType)
	}

	query := fmt.Sprintf(`SELECT id, processed_timestamp, originated_timestamp,
		organization_id, action, entity_id, event_data
		FROM %s
		WHERE organization_id = ? AND processed_timestamp > ?
		ORDER BY processed_timestamp ASC`, tableName)

	rows, err := es.db.QueryContext(ctx, query, orgID, since)
	if err != nil {
		return nil, fmt.Errorf("failed to get events for %s: %w", entityType, err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var event Event
		var action string
		if err := rows.Scan(
			&event.ID,
			&event.ProcessedTimestamp,
			&event.OriginatedTimestamp,
			&event.OrganizationID,
			&action,
			&event.EntityID,
			&event.EventData,
		); err != nil {
			return nil, fmt.Errorf("failed to scan event: %w", err)
		}
		event.Action = Action(action)
		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating events: %w", err)
	}

	es.logger.Debug("Retrieved sync events",
		zap.String("entity_type", string(entityType)),
		zap.String("organization_id", orgID),
		zap.Time("since", since),
		zap.Int("count", len(events)))

	return events, nil
}

// CleanupOldEvents removes events older than retention period
func (es *sqliteEventStore) CleanupOldEvents(ctx context.Context, olderThan time.Time) error {
	tables := []string{"api_events", "certificate_events", "llm_template_events"}

	totalDeleted := 0
	for _, table := range tables {
		query := fmt.Sprintf("DELETE FROM %s WHERE processed_timestamp < ?", table)
		result, err := es.db.ExecContext(ctx, query, olderThan)
		if err != nil {
			es.logger.Warn("Failed to cleanup events",
				zap.String("table", table),
				zap.Error(err))
			continue
		}

		deleted, _ := result.RowsAffected()
		totalDeleted += int(deleted)
	}

	if totalDeleted > 0 {
		es.logger.Info("Cleaned up old sync events",
			zap.Int("total_deleted", totalDeleted),
			zap.Time("older_than", olderThan))
	}

	return nil
}
