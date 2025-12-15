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
	"math/rand"
	"sync"
	"time"

	"go.uber.org/zap"
)

// syncPoller implements the SyncPoller interface
type syncPoller struct {
	stateManager   StateManager
	eventStore     EventStore
	logger         *zap.Logger

	pollInterval   time.Duration
	jitterMax      time.Duration
	organizationID string

	callbacks     map[EntityType]SyncCallback
	lastApplied   map[EntityType]time.Time
	knownVersions map[EntityType]string

	mu     sync.RWMutex
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// SyncPollerConfig holds configuration for the SyncPoller
type SyncPollerConfig struct {
	PollInterval   time.Duration // Default: 5 seconds
	JitterMax      time.Duration // Default: 1 second
	OrganizationID string        // Default: "default"
}

// NewSyncPoller creates a new SyncPoller instance
func NewSyncPoller(
	stateManager StateManager,
	eventStore EventStore,
	logger *zap.Logger,
	config SyncPollerConfig,
) SyncPoller {
	// Apply defaults
	if config.PollInterval == 0 {
		config.PollInterval = 5 * time.Second
	}
	if config.JitterMax == 0 {
		config.JitterMax = 1 * time.Second
	}
	if config.OrganizationID == "" {
		config.OrganizationID = "default"
	}

	return &syncPoller{
		stateManager:   stateManager,
		eventStore:     eventStore,
		logger:         logger,
		pollInterval:   config.PollInterval,
		jitterMax:      config.JitterMax,
		organizationID: config.OrganizationID,
		callbacks:      make(map[EntityType]SyncCallback),
		lastApplied:    make(map[EntityType]time.Time),
		knownVersions:  make(map[EntityType]string),
		stopCh:         make(chan struct{}),
	}
}

// Start begins the polling loop
func (sp *syncPoller) Start(ctx context.Context) error {
	sp.logger.Info("Starting sync poller",
		zap.Duration("poll_interval", sp.pollInterval),
		zap.Duration("jitter_max", sp.jitterMax),
		zap.String("organization_id", sp.organizationID))

	// Start poll loop
	sp.wg.Add(1)
	go sp.pollLoop(ctx)

	// Start cleanup loop (runs every hour)
	sp.wg.Add(1)
	go sp.cleanupLoop(ctx)

	return nil
}

// Stop gracefully stops the poller
func (sp *syncPoller) Stop() error {
	sp.logger.Info("Stopping sync poller")
	close(sp.stopCh)
	sp.wg.Wait()
	sp.logger.Info("Sync poller stopped")
	return nil
}

// RegisterCallback registers a callback for a specific entity type
func (sp *syncPoller) RegisterCallback(entityType EntityType, callback SyncCallback) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.callbacks[entityType] = callback
	sp.logger.Info("Registered sync callback", zap.String("entity_type", string(entityType)))
}

// GetLastAppliedTimestamp returns the last timestamp processed for an entity type
func (sp *syncPoller) GetLastAppliedTimestamp(entityType EntityType) time.Time {
	sp.mu.RLock()
	defer sp.mu.RUnlock()
	return sp.lastApplied[entityType]
}

// pollLoop is the main polling loop
func (sp *syncPoller) pollLoop(ctx context.Context) {
	defer sp.wg.Done()

	// Add initial jitter to spread out polling across instances
	initialDelay := time.Duration(rand.Int63n(int64(sp.jitterMax)))
	time.Sleep(initialDelay)

	ticker := time.NewTicker(sp.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sp.stopCh:
			return
		case <-ticker.C:
			sp.poll(ctx)

			// Add jitter between polls
			jitter := time.Duration(rand.Int63n(int64(sp.jitterMax)))
			time.Sleep(jitter)
		}
	}
}

// poll checks for state changes and syncs entities
func (sp *syncPoller) poll(ctx context.Context) {
	states, err := sp.stateManager.GetAllStates(ctx, sp.organizationID)
	if err != nil {
		sp.logger.Error("Failed to get states during poll", zap.Error(err))
		return
	}

	for _, state := range states {
		sp.checkAndSyncEntity(ctx, state)
	}
}

// checkAndSyncEntity checks if an entity has changed and syncs it
func (sp *syncPoller) checkAndSyncEntity(ctx context.Context, state EntityState) {
	sp.mu.Lock()
	knownVersion := sp.knownVersions[state.EntityType]
	sp.mu.Unlock()

	// Version mismatch detected - need to sync
	if knownVersion != state.VersionID {
		sp.logger.Info("Version mismatch detected, syncing",
			zap.String("entity_type", string(state.EntityType)),
			zap.String("known_version", knownVersion),
			zap.String("new_version", state.VersionID))

		sp.syncEntity(ctx, state.EntityType)

		// Update known version
		sp.mu.Lock()
		sp.knownVersions[state.EntityType] = state.VersionID
		sp.mu.Unlock()
	}
}

// syncEntity fetches and applies events for an entity type
func (sp *syncPoller) syncEntity(ctx context.Context, entityType EntityType) {
	sp.mu.RLock()
	callback := sp.callbacks[entityType]
	lastApplied := sp.lastApplied[entityType]
	sp.mu.RUnlock()

	if callback == nil {
		sp.logger.Warn("No callback registered for entity type",
			zap.String("entity_type", string(entityType)))
		return
	}

	// Fetch events since last applied timestamp
	events, err := sp.eventStore.GetEventsSince(ctx, entityType, sp.organizationID, lastApplied)
	if err != nil {
		sp.logger.Error("Failed to get events",
			zap.String("entity_type", string(entityType)),
			zap.Error(err))
		return
	}

	if len(events) == 0 {
		sp.logger.Debug("No new events to process",
			zap.String("entity_type", string(entityType)))
		return
	}

	sp.logger.Info("Processing sync events",
		zap.String("entity_type", string(entityType)),
		zap.Int("event_count", len(events)))

	// Apply events via callback
	if err := callback(entityType, events); err != nil {
		sp.logger.Error("Failed to apply events",
			zap.String("entity_type", string(entityType)),
			zap.Error(err))
		return
	}

	// Update last applied timestamp
	sp.mu.Lock()
	sp.lastApplied[entityType] = events[len(events)-1].ProcessedTimestamp
	sp.mu.Unlock()

	sp.logger.Info("Successfully applied sync events",
		zap.String("entity_type", string(entityType)),
		zap.Int("event_count", len(events)))
}

// cleanupLoop periodically cleans up old events
func (sp *syncPoller) cleanupLoop(ctx context.Context) {
	defer sp.wg.Done()

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sp.stopCh:
			return
		case <-ticker.C:
			// Clean up events older than 1 day
			cutoff := time.Now().Add(-24 * time.Hour)
			if err := sp.eventStore.CleanupOldEvents(ctx, cutoff); err != nil {
				sp.logger.Warn("Failed to cleanup old events", zap.Error(err))
			}
		}
	}
}
