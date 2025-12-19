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

// stateManager implements the StateManager interface for managing entity states
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
func (sm *stateManager) GetState(ctx context.Context, entityType EntityType, orgID string) (*EntityState, error) {
	query := `SELECT entity_type, organization_id, version_id, updated_at
	          FROM entity_states
	          WHERE entity_type = ? AND organization_id = ?`

	var state EntityState
	err := sm.db.QueryRowContext(ctx, query, string(entityType), orgID).Scan(
		&state.EntityType,
		&state.OrganizationID,
		&state.VersionID,
		&state.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		// State doesn't exist yet, initialize it
		return sm.initializeState(ctx, entityType, orgID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get state for %s: %w", entityType, err)
	}

	return &state, nil
}

// UpdateState atomically updates state version
// This must be called within a transaction
func (sm *stateManager) UpdateState(ctx context.Context, entityType EntityType, orgID string) (string, error) {
	newVersionID := uuid.New().String()
	now := time.Now()

	// Use INSERT OR REPLACE for SQLite (UPSERT)
	query := `INSERT INTO entity_states (entity_type, organization_id, version_id, updated_at)
	          VALUES (?, ?, ?, ?)
	          ON CONFLICT(entity_type, organization_id)
	          DO UPDATE SET version_id = excluded.version_id, updated_at = excluded.updated_at`

	_, err := sm.db.ExecContext(ctx, query, string(entityType), orgID, newVersionID, now)
	if err != nil {
		return "", fmt.Errorf("failed to update state for %s: %w", entityType, err)
	}

	sm.logger.Debug("Updated entity state",
		zap.String("entity_type", string(entityType)),
		zap.String("organization_id", orgID),
		zap.String("version_id", newVersionID))

	return newVersionID, nil
}

// GetAllStates retrieves all states for an organization
func (sm *stateManager) GetAllStates(ctx context.Context, orgID string) ([]EntityState, error) {
	query := `SELECT entity_type, organization_id, version_id, updated_at
	          FROM entity_states
	          WHERE organization_id = ?`

	rows, err := sm.db.QueryContext(ctx, query, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to get all states: %w", err)
	}
	defer rows.Close()

	var states []EntityState
	for rows.Next() {
		var state EntityState
		if err := rows.Scan(
			&state.EntityType,
			&state.OrganizationID,
			&state.VersionID,
			&state.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan state: %w", err)
		}
		states = append(states, state)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating states: %w", err)
	}

	// If no states exist, initialize all entity types
	if len(states) == 0 {
		entityTypes := []EntityType{EntityTypeAPI, EntityTypeCertificate, EntityTypeLLMTemplate}
		for _, entityType := range entityTypes {
			state, err := sm.initializeState(ctx, entityType, orgID)
			if err != nil {
				return nil, fmt.Errorf("failed to initialize state for %s: %w", entityType, err)
			}
			states = append(states, *state)
		}
	}

	return states, nil
}

// initializeState creates an initial state if one doesn't exist
func (sm *stateManager) initializeState(ctx context.Context, entityType EntityType, orgID string) (*EntityState, error) {
	newVersionID := uuid.New().String()
	now := time.Now()

	query := `INSERT OR IGNORE INTO entity_states (entity_type, organization_id, version_id, updated_at)
	          VALUES (?, ?, ?, ?)`

	_, err := sm.db.ExecContext(ctx, query, string(entityType), orgID, newVersionID, now)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize state for %s: %w", entityType, err)
	}

	sm.logger.Info("Initialized entity state",
		zap.String("entity_type", string(entityType)),
		zap.String("organization_id", orgID),
		zap.String("version_id", newVersionID))

	return &EntityState{
		EntityType:     entityType,
		OrganizationID: orgID,
		VersionID:      newVersionID,
		UpdatedAt:      now,
	}, nil
}
