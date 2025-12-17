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

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// StateManager manages entity states for synchronization
type StateManager interface {
	// GetState retrieves the current state for an entity type
	GetState(ctx context.Context, entityType EntityType) (*EntityState, error)

	// UpdateState updates the version ID for an entity type
	UpdateState(ctx context.Context, entityType EntityType) (newVersionID string, err error)

	// UpdateStateWithTx updates the version ID for an entity type within a transaction
	UpdateStateWithTx(ctx context.Context, tx *sql.Tx, entityType EntityType) (newVersionID string, err error)
}

// stateManager implements StateManager using a SQL database
type stateManager struct {
	db     *sql.DB
	logger *zap.Logger
}

// NewStateManager creates a new StateManager instance
func NewStateManager(db *sql.DB, logger *zap.Logger) StateManager {
	return &stateManager{
		db:     db,
		logger: logger,
	}
}

// GetState retrieves the current state for an entity type
func (sm *stateManager) GetState(ctx context.Context, entityType EntityType) (*EntityState, error) {
	query := `SELECT entity_type, version_id, updated_at FROM entity_states WHERE entity_type = ?`

	var state EntityState
	err := sm.db.QueryRowContext(ctx, query, entityType).Scan(
		&state.EntityType,
		&state.VersionID,
		&state.UpdatedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			// No state exists yet - this is normal for new entity types
			return nil, nil
		}
		return nil, fmt.Errorf("failed to query entity state: %w", err)
	}

	return &state, nil
}

// UpdateState updates the version ID for an entity type
func (sm *stateManager) UpdateState(ctx context.Context, entityType EntityType) (string, error) {
	newVersionID := uuid.New().String()

	query := `
		INSERT INTO entity_states (entity_type, version_id, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(entity_type) DO UPDATE SET
			version_id = excluded.version_id,
			updated_at = excluded.updated_at
	`

	now := time.Now()
	_, err := sm.db.ExecContext(ctx, query, entityType, newVersionID, now)
	if err != nil {
		return "", fmt.Errorf("failed to update entity state: %w", err)
	}

	sm.logger.Debug("Entity state updated",
		zap.String("entity_type", string(entityType)),
		zap.String("version_id", newVersionID))

	return newVersionID, nil
}

// UpdateStateWithTx updates the version ID for an entity type within a transaction
func (sm *stateManager) UpdateStateWithTx(ctx context.Context, tx *sql.Tx, entityType EntityType) (string, error) {
	newVersionID := uuid.New().String()

	query := `
		INSERT INTO entity_states (entity_type, version_id, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(entity_type) DO UPDATE SET
			version_id = excluded.version_id,
			updated_at = excluded.updated_at
	`

	now := time.Now()
	_, err := tx.ExecContext(ctx, query, entityType, newVersionID, now)
	if err != nil {
		return "", fmt.Errorf("failed to update entity state: %w", err)
	}

	sm.logger.Debug("Entity state updated (in transaction)",
		zap.String("entity_type", string(entityType)),
		zap.String("version_id", newVersionID))

	return newVersionID, nil
}
