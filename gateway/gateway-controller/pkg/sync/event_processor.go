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
	"time"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/storage"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/xds"
	"go.uber.org/zap"
)

// EventProcessor handles applying sync events to local state
type EventProcessor struct {
	configStore     *storage.ConfigStore
	snapshotManager *xds.SnapshotManager
	logger          *zap.Logger
}

// NewEventProcessor creates a new EventProcessor
func NewEventProcessor(
	configStore *storage.ConfigStore,
	snapshotManager *xds.SnapshotManager,
	logger *zap.Logger,
) *EventProcessor {
	return &EventProcessor{
		configStore:     configStore,
		snapshotManager: snapshotManager,
		logger:          logger,
	}
}

// ProcessAPIEvents handles API entity sync events
func (ep *EventProcessor) ProcessAPIEvents(entityType EntityType, events []Event) error {
	if entityType != EntityTypeAPI {
		return fmt.Errorf("unexpected entity type: %s", entityType)
	}

	for _, event := range events {
		if err := ep.processAPIEvent(event); err != nil {
			ep.logger.Error("Failed to process API event",
				zap.String("entity_id", event.EntityID),
				zap.String("action", string(event.Action)),
				zap.Error(err))
			// Continue processing other events
			continue
		}
	}

	// Trigger xDS update after processing all events
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ep.snapshotManager.UpdateSnapshot(ctx, "sync-event"); err != nil {
		ep.logger.Error("Failed to update xDS snapshot after sync", zap.Error(err))
		return err
	}

	return nil
}

// processAPIEvent processes a single API event
func (ep *EventProcessor) processAPIEvent(event Event) error {
	switch event.Action {
	case ActionCreate, ActionUpdate:
		var cfg models.StoredConfig
		if err := json.Unmarshal(event.EventData, &cfg); err != nil {
			return fmt.Errorf("failed to unmarshal config: %w", err)
		}

		// Check if config already exists
		existing, err := ep.configStore.Get(cfg.ID)
		if err == nil && existing != nil {
			// Update existing
			return ep.configStore.Update(&cfg)
		}
		// Add new
		return ep.configStore.Add(&cfg)

	case ActionDelete:
		return ep.configStore.Delete(event.EntityID)

	default:
		return fmt.Errorf("unknown action: %s", event.Action)
	}
}

// ProcessCertificateEvents handles Certificate entity sync events
func (ep *EventProcessor) ProcessCertificateEvents(entityType EntityType, events []Event) error {
	if entityType != EntityTypeCertificate {
		return fmt.Errorf("unexpected entity type: %s", entityType)
	}

	ep.logger.Info("Processing certificate sync events",
		zap.Int("event_count", len(events)))

	// Certificate sync would update certificate store and trigger SDS update
	// For now, we just log it as certificates are less frequently updated
	// and may need additional integration with SDS

	// TODO: Implement certificate event processing when needed
	ep.logger.Warn("Certificate sync event processing not yet implemented")

	return nil
}

// ProcessLLMTemplateEvents handles LLM Template entity sync events
func (ep *EventProcessor) ProcessLLMTemplateEvents(entityType EntityType, events []Event) error {
	if entityType != EntityTypeLLMTemplate {
		return fmt.Errorf("unexpected entity type: %s", entityType)
	}

	ep.logger.Info("Processing LLM template sync events",
		zap.Int("event_count", len(events)))

	// LLM template sync would update template store
	// For now, we just log it as templates are less frequently updated

	// TODO: Implement LLM template event processing when needed
	ep.logger.Warn("LLM template sync event processing not yet implemented")

	return nil
}
