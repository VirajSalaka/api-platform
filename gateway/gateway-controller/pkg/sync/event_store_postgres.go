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

// postgresEventStore implements EventStore for PostgreSQL
type postgresEventStore struct {
	db     *sql.DB
	logger *zap.Logger
}

// NewPostgresEventStore creates a new PostgreSQL-based EventStore
func NewPostgresEventStore(db *sql.DB, logger *zap.Logger) EventStore {
	return &postgresEventStore{
		db:     db,
		logger: logger,
	}
}

// RecordEvent records a new event for an entity type
func (es *postgresEventStore) RecordEvent(ctx context.Context, entityType EntityType, action Action, entityID string, eventData []byte, originatedTimestamp time.Time) error {
	tableName := getEventTableName(entityType)
	query := fmt.Sprintf(`
		INSERT INTO %s (processed_timestamp, originated_timestamp, action, entity_id, event_data)
		VALUES ($1, $2, $3, $4, $5)
	`, tableName)

	_, err := es.db.ExecContext(ctx, query,
		time.Now(),
		originatedTimestamp,
		action,
		entityID,
		eventData,
	)

	if err != nil {
		return fmt.Errorf("failed to record event: %w", err)
	}

	es.logger.Debug("Event recorded",
		zap.String("entity_type", string(entityType)),
		zap.String("action", string(action)),
		zap.String("entity_id", entityID))

	return nil
}

// RecordEventWithTx records a new event within a transaction
func (es *postgresEventStore) RecordEventWithTx(ctx context.Context, tx *sql.Tx, entityType EntityType, action Action, entityID string, eventData []byte, originatedTimestamp time.Time) error {
	tableName := getEventTableName(entityType)
	query := fmt.Sprintf(`
		INSERT INTO %s (processed_timestamp, originated_timestamp, action, entity_id, event_data)
		VALUES ($1, $2, $3, $4, $5)
	`, tableName)

	_, err := tx.ExecContext(ctx, query,
		time.Now(),
		originatedTimestamp,
		action,
		entityID,
		eventData,
	)

	if err != nil {
		return fmt.Errorf("failed to record event in transaction: %w", err)
	}

	es.logger.Debug("Event recorded (in transaction)",
		zap.String("entity_type", string(entityType)),
		zap.String("action", string(action)),
		zap.String("entity_id", entityID))

	return nil
}

// GetEventsSince retrieves all events since a given timestamp
func (es *postgresEventStore) GetEventsSince(ctx context.Context, entityType EntityType, since time.Time) ([]Event, error) {
	tableName := getEventTableName(entityType)
	query := fmt.Sprintf(`
		SELECT id, processed_timestamp, originated_timestamp, action, entity_id, event_data
		FROM %s
		WHERE processed_timestamp > $1
		ORDER BY processed_timestamp ASC
	`, tableName)

	rows, err := es.db.QueryContext(ctx, query, since)
	if err != nil {
		return nil, fmt.Errorf("failed to query events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var event Event
		var action string

		err := rows.Scan(
			&event.ID,
			&event.ProcessedTimestamp,
			&event.OriginatedTimestamp,
			&action,
			&event.EntityID,
			&event.EventData,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan event row: %w", err)
		}

		event.Action = Action(action)
		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating event rows: %w", err)
	}

	es.logger.Debug("Retrieved events",
		zap.String("entity_type", string(entityType)),
		zap.Time("since", since),
		zap.Int("count", len(events)))

	return events, nil
}

// CleanupOldEvents removes events older than the specified timestamp
func (es *postgresEventStore) CleanupOldEvents(ctx context.Context, olderThan time.Time) error {
	// Clean up events for all entity types
	// Currently only API events, but this is extensible
	entityTypes := []EntityType{EntityTypeAPI}

	for _, entityType := range entityTypes {
		tableName := getEventTableName(entityType)
		query := fmt.Sprintf(`DELETE FROM %s WHERE processed_timestamp < $1`, tableName)

		result, err := es.db.ExecContext(ctx, query, olderThan)
		if err != nil {
			return fmt.Errorf("failed to cleanup events for %s: %w", entityType, err)
		}

		rowsAffected, _ := result.RowsAffected()
		es.logger.Info("Cleaned up old events",
			zap.String("entity_type", string(entityType)),
			zap.Time("older_than", olderThan),
			zap.Int64("rows_deleted", rowsAffected))
	}

	return nil
}
