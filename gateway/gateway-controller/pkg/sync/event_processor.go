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
	"encoding/json"
	"fmt"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"go.uber.org/zap"
)

// ConfigStore defines the interface for in-memory configuration storage
type ConfigStore interface {
	Add(cfg *models.StoredConfig) error
	Update(cfg *models.StoredConfig) error
	Delete(id string) error
	Get(id string) (*models.StoredConfig, error)
}

// SnapshotManager defines the interface for xDS snapshot management
type SnapshotManager interface {
	UpdateSnapshot(ctx context.Context, correlationID string) error
}

// EventProcessor processes synchronization events and applies them to in-memory state
type EventProcessor struct {
	configStore     ConfigStore
	snapshotManager SnapshotManager
	logger          *zap.Logger
}

// NewEventProcessor creates a new EventProcessor instance
func NewEventProcessor(configStore ConfigStore, snapshotManager SnapshotManager, logger *zap.Logger) *EventProcessor {
	return &EventProcessor{
		configStore:     configStore,
		snapshotManager: snapshotManager,
		logger:          logger,
	}
}

// ProcessAPIEvents processes API events and applies them to the ConfigStore
func (ep *EventProcessor) ProcessAPIEvents(events []Event) error {
	if len(events) == 0 {
		return nil
	}

	ep.logger.Info("Processing API events",
		zap.Int("event_count", len(events)))

	// Track if we need to update the xDS snapshot
	needsSnapshotUpdate := false

	for _, event := range events {
		if err := ep.processAPIEvent(event); err != nil {
			ep.logger.Error("Failed to process API event",
				zap.Int64("event_id", event.ID),
				zap.String("action", string(event.Action)),
				zap.String("entity_id", event.EntityID),
				zap.Error(err))
			// Continue processing other events even if one fails
			continue
		}
		needsSnapshotUpdate = true
	}

	// Trigger xDS snapshot update if any events were successfully processed
	if needsSnapshotUpdate {
		ep.logger.Info("Triggering xDS snapshot update after processing events")
		if err := ep.snapshotManager.UpdateSnapshot(context.Background(), "sync-event"); err != nil {
			ep.logger.Error("Failed to update xDS snapshot", zap.Error(err))
			return fmt.Errorf("failed to update xDS snapshot: %w", err)
		}
	}

	return nil
}

// processAPIEvent processes a single API event
func (ep *EventProcessor) processAPIEvent(event Event) error {
	switch event.Action {
	case ActionCreate:
		return ep.handleCreate(event)
	case ActionUpdate:
		return ep.handleUpdate(event)
	case ActionDelete:
		return ep.handleDelete(event)
	default:
		return fmt.Errorf("unknown action: %s", event.Action)
	}
}

// handleCreate handles CREATE events
func (ep *EventProcessor) handleCreate(event Event) error {
	var cfg models.StoredConfig
	if err := json.Unmarshal(event.EventData, &cfg); err != nil {
		return fmt.Errorf("failed to unmarshal config data: %w", err)
	}

	// Check if already exists in ConfigStore
	existing, err := ep.configStore.Get(cfg.ID)
	if err == nil && existing != nil {
		// Already exists - this can happen if the event was already processed
		// Just log and skip
		ep.logger.Debug("Config already exists in store, skipping CREATE",
			zap.String("config_id", cfg.ID))
		return nil
	}

	if err := ep.configStore.Add(&cfg); err != nil {
		return fmt.Errorf("failed to add config to store: %w", err)
	}

	ep.logger.Info("Applied CREATE event",
		zap.String("config_id", cfg.ID),
		zap.String("name", cfg.GetName()),
		zap.String("version", cfg.GetVersion()))

	return nil
}

// handleUpdate handles UPDATE events
func (ep *EventProcessor) handleUpdate(event Event) error {
	var cfg models.StoredConfig
	if err := json.Unmarshal(event.EventData, &cfg); err != nil {
		return fmt.Errorf("failed to unmarshal config data: %w", err)
	}

	// Check if exists in ConfigStore
	existing, err := ep.configStore.Get(cfg.ID)
	if err != nil || existing == nil {
		// Doesn't exist - treat as CREATE
		ep.logger.Warn("Config not found in store for UPDATE, treating as CREATE",
			zap.String("config_id", cfg.ID))
		if err := ep.configStore.Add(&cfg); err != nil {
			return fmt.Errorf("failed to add config to store: %w", err)
		}
		return nil
	}

	if err := ep.configStore.Update(&cfg); err != nil {
		return fmt.Errorf("failed to update config in store: %w", err)
	}

	ep.logger.Info("Applied UPDATE event",
		zap.String("config_id", cfg.ID),
		zap.String("name", cfg.GetName()),
		zap.String("version", cfg.GetVersion()))

	return nil
}

// handleDelete handles DELETE events
func (ep *EventProcessor) handleDelete(event Event) error {
	// For DELETE events, the event data just contains the ID
	var deleteData struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(event.EventData, &deleteData); err != nil {
		return fmt.Errorf("failed to unmarshal delete data: %w", err)
	}

	// Check if exists in ConfigStore
	existing, err := ep.configStore.Get(deleteData.ID)
	if err != nil || existing == nil {
		// Already deleted - this can happen if the event was already processed
		ep.logger.Debug("Config not found in store, skipping DELETE",
			zap.String("config_id", deleteData.ID))
		return nil
	}

	if err := ep.configStore.Delete(deleteData.ID); err != nil {
		return fmt.Errorf("failed to delete config from store: %w", err)
	}

	ep.logger.Info("Applied DELETE event",
		zap.String("config_id", deleteData.ID))

	return nil
}
