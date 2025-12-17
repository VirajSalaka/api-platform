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
	"fmt"
	"math/rand"
	"sync"
	"time"

	"go.uber.org/zap"
)

// SyncPoller polls for changes in entity states and triggers synchronization
type SyncPoller struct {
	stateManager  StateManager
	eventStore    EventStore
	callbacks     map[EntityType]SyncCallback
	lastApplied   map[EntityType]time.Time
	knownVersions map[EntityType]string
	pollInterval  time.Duration
	jitterMax     time.Duration
	cleanupTicker *time.Ticker
	eventRetention time.Duration
	logger        *zap.Logger

	// Control
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.RWMutex
}

// NewSyncPoller creates a new SyncPoller instance
func NewSyncPoller(
	stateManager StateManager,
	eventStore EventStore,
	pollInterval time.Duration,
	jitterMax time.Duration,
	eventRetention time.Duration,
	logger *zap.Logger,
) *SyncPoller {
	return &SyncPoller{
		stateManager:   stateManager,
		eventStore:     eventStore,
		callbacks:      make(map[EntityType]SyncCallback),
		lastApplied:    make(map[EntityType]time.Time),
		knownVersions:  make(map[EntityType]string),
		pollInterval:   pollInterval,
		jitterMax:      jitterMax,
		eventRetention: eventRetention,
		logger:         logger,
	}
}

// RegisterCallback registers a callback for an entity type
func (sp *SyncPoller) RegisterCallback(entityType EntityType, callback SyncCallback) {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	sp.callbacks[entityType] = callback
	sp.logger.Info("Callback registered",
		zap.String("entity_type", string(entityType)))
}

// Start starts the synchronization poller
func (sp *SyncPoller) Start(ctx context.Context) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	if sp.cancel != nil {
		return fmt.Errorf("sync poller already started")
	}

	sp.ctx, sp.cancel = context.WithCancel(ctx)

	// Initialize known versions from database
	for entityType := range sp.callbacks {
		state, err := sp.stateManager.GetState(sp.ctx, entityType)
		if err != nil {
			sp.logger.Error("Failed to get initial state",
				zap.String("entity_type", string(entityType)),
				zap.Error(err))
			continue
		}

		if state != nil {
			sp.knownVersions[entityType] = state.VersionID
			sp.lastApplied[entityType] = state.UpdatedAt
			sp.logger.Info("Initialized entity state",
				zap.String("entity_type", string(entityType)),
				zap.String("version_id", state.VersionID),
				zap.Time("updated_at", state.UpdatedAt))
		} else {
			// No state exists yet - initialize with current time
			sp.lastApplied[entityType] = time.Now()
			sp.logger.Info("No existing state, starting fresh",
				zap.String("entity_type", string(entityType)))
		}
	}

	// Start polling goroutine
	sp.wg.Add(1)
	go sp.pollLoop()

	// Start cleanup goroutine
	sp.cleanupTicker = time.NewTicker(1 * time.Hour)
	sp.wg.Add(1)
	go sp.cleanupLoop()

	sp.logger.Info("Sync poller started",
		zap.Duration("poll_interval", sp.pollInterval),
		zap.Duration("jitter_max", sp.jitterMax),
		zap.Duration("event_retention", sp.eventRetention))

	return nil
}

// Stop stops the synchronization poller
func (sp *SyncPoller) Stop() error {
	sp.mu.Lock()
	if sp.cancel == nil {
		sp.mu.Unlock()
		return fmt.Errorf("sync poller not started")
	}

	sp.cancel()
	if sp.cleanupTicker != nil {
		sp.cleanupTicker.Stop()
	}
	sp.mu.Unlock()

	// Wait for goroutines to finish
	sp.wg.Wait()

	sp.logger.Info("Sync poller stopped")
	return nil
}

// pollLoop runs the polling loop with jitter
func (sp *SyncPoller) pollLoop() {
	defer sp.wg.Done()

	ticker := time.NewTicker(sp.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sp.ctx.Done():
			return
		case <-ticker.C:
			// Add jitter to prevent thundering herd
			if sp.jitterMax > 0 {
				jitter := time.Duration(rand.Int63n(int64(sp.jitterMax)))
				time.Sleep(jitter)
			}

			sp.poll()
		}
	}
}

// poll checks for state changes and triggers callbacks
func (sp *SyncPoller) poll() {
	sp.mu.RLock()
	callbacks := make(map[EntityType]SyncCallback)
	for k, v := range sp.callbacks {
		callbacks[k] = v
	}
	sp.mu.RUnlock()

	for entityType, callback := range callbacks {
		if err := sp.pollEntityType(entityType, callback); err != nil {
			sp.logger.Error("Failed to poll entity type",
				zap.String("entity_type", string(entityType)),
				zap.Error(err))
		}
	}
}

// pollEntityType checks for changes to a specific entity type
func (sp *SyncPoller) pollEntityType(entityType EntityType, callback SyncCallback) error {
	// Get current state from database
	state, err := sp.stateManager.GetState(sp.ctx, entityType)
	if err != nil {
		return fmt.Errorf("failed to get state: %w", err)
	}

	if state == nil {
		// No state exists yet - nothing to sync
		return nil
	}

	sp.mu.RLock()
	knownVersion := sp.knownVersions[entityType]
	lastApplied := sp.lastApplied[entityType]
	sp.mu.RUnlock()

	// Check if version has changed
	if state.VersionID == knownVersion {
		// No changes
		return nil
	}

	sp.logger.Info("Detected state change",
		zap.String("entity_type", string(entityType)),
		zap.String("old_version", knownVersion),
		zap.String("new_version", state.VersionID))

	// Fetch events since last applied timestamp
	events, err := sp.eventStore.GetEventsSince(sp.ctx, entityType, lastApplied)
	if err != nil {
		return fmt.Errorf("failed to get events: %w", err)
	}

	if len(events) == 0 {
		sp.logger.Warn("Version changed but no events found",
			zap.String("entity_type", string(entityType)),
			zap.Time("since", lastApplied))
		// Update known version anyway to avoid re-checking
		sp.mu.Lock()
		sp.knownVersions[entityType] = state.VersionID
		sp.mu.Unlock()
		return nil
	}

	sp.logger.Info("Processing events",
		zap.String("entity_type", string(entityType)),
		zap.Int("event_count", len(events)))

	// Apply events via callback
	if err := callback(events); err != nil {
		return fmt.Errorf("failed to apply events: %w", err)
	}

	// Update last applied timestamp to the latest event's timestamp
	latestTimestamp := events[len(events)-1].ProcessedTimestamp

	sp.mu.Lock()
	sp.knownVersions[entityType] = state.VersionID
	sp.lastApplied[entityType] = latestTimestamp
	sp.mu.Unlock()

	sp.logger.Info("Successfully applied events",
		zap.String("entity_type", string(entityType)),
		zap.Int("event_count", len(events)),
		zap.String("new_version", state.VersionID))

	return nil
}

// cleanupLoop runs periodic cleanup of old events
func (sp *SyncPoller) cleanupLoop() {
	defer sp.wg.Done()

	for {
		select {
		case <-sp.ctx.Done():
			return
		case <-sp.cleanupTicker.C:
			sp.cleanup()
		}
	}
}

// cleanup removes old events
func (sp *SyncPoller) cleanup() {
	olderThan := time.Now().Add(-sp.eventRetention)

	sp.logger.Info("Starting event cleanup",
		zap.Time("older_than", olderThan))

	if err := sp.eventStore.CleanupOldEvents(sp.ctx, olderThan); err != nil {
		sp.logger.Error("Failed to cleanup old events", zap.Error(err))
	}
}
