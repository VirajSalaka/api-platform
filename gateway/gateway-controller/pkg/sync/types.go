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
	"time"
)

// EntityType represents the types of entities that can be synchronized across Gateway Controller instances
type EntityType string

const (
	// EntityTypeAPI represents API configurations
	EntityTypeAPI EntityType = "API"
	// EntityTypeCertificate represents TLS certificates
	EntityTypeCertificate EntityType = "Certificate"
	// EntityTypeLLMTemplate represents LLM provider templates
	EntityTypeLLMTemplate EntityType = "LLMProviderTemplate"
)

// Action represents the type of change made to an entity
type Action string

const (
	// ActionCreate indicates an entity was created
	ActionCreate Action = "CREATE"
	// ActionUpdate indicates an entity was updated
	ActionUpdate Action = "UPDATE"
	// ActionDelete indicates an entity was deleted
	ActionDelete Action = "DELETE"
)

// EntityState represents the version state for an entity type in a given organization
type EntityState struct {
	EntityType     EntityType
	OrganizationID string
	VersionID      string    // UUID that changes on every modification
	UpdatedAt      time.Time
}

// Event represents a change event for an entity
type Event struct {
	ID                  int64
	ProcessedTimestamp  time.Time // Timestamp when event was recorded in database
	OriginatedTimestamp time.Time // Timestamp when the change originated
	OrganizationID      string
	Action              Action
	EntityID            string
	EventData           []byte // JSON serialized entity data
}

// StateManager handles entity state versioning for detecting changes across instances
type StateManager interface {
	// GetState retrieves the current state for an entity type
	// Returns nil if state doesn't exist yet
	GetState(ctx context.Context, entityType EntityType, orgID string) (*EntityState, error)

	// UpdateState atomically updates state version (must be called within transaction)
	// Returns the new version ID (UUID)
	UpdateState(ctx context.Context, entityType EntityType, orgID string) (string, error)

	// GetAllStates retrieves all states for an organization (used by poller)
	GetAllStates(ctx context.Context, orgID string) ([]EntityState, error)
}

// EventStore handles event persistence and retrieval for synchronization
type EventStore interface {
	// RecordEvent records an event (must be called within transaction)
	RecordEvent(ctx context.Context, entityType EntityType, event *Event) error

	// GetEventsSince retrieves events since a given timestamp for synchronization
	GetEventsSince(ctx context.Context, entityType EntityType, orgID string, since time.Time) ([]Event, error)

	// CleanupOldEvents removes events older than retention period
	CleanupOldEvents(ctx context.Context, olderThan time.Time) error
}

// SyncCallback is called when events need to be applied to local state
type SyncCallback func(entityType EntityType, events []Event) error

// SyncPoller handles background polling for state changes across instances
type SyncPoller interface {
	// Start begins the polling loop
	Start(ctx context.Context) error

	// Stop gracefully stops the poller
	Stop() error

	// RegisterCallback registers a callback for a specific entity type
	// The callback will be invoked when changes are detected for that entity type
	RegisterCallback(entityType EntityType, callback SyncCallback)

	// GetLastAppliedTimestamp returns the last timestamp processed for an entity type
	GetLastAppliedTimestamp(entityType EntityType) time.Time
}
