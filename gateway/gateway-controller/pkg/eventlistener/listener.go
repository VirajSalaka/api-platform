/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
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

package eventlistener

import (
	"context"
	"log/slog"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/generated"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/policyxds"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/storage"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/xds"
)

// EventListener listens for events from an EventSource and processes them
// to keep the local replica synchronized with other replicas.
type EventListener struct {
	eventSource       EventSource
	store             *storage.ConfigStore
	db                storage.Storage
	snapshotManager   *xds.SnapshotManager
	policyManager     *policyxds.PolicyManager
	routerConfig      *config.RouterConfig
	logger            *slog.Logger
	systemConfig      *config.Config
	policyDefinitions map[string]api.PolicyDefinition

	eventCh chan Event
	ctx     context.Context
	cancel  context.CancelFunc
}

// NewEventListener creates a new EventListener
func NewEventListener(
	eventSource EventSource,
	store *storage.ConfigStore,
	db storage.Storage,
	snapshotManager *xds.SnapshotManager,
	policyManager *policyxds.PolicyManager,
	routerConfig *config.RouterConfig,
	logger *slog.Logger,
	systemConfig *config.Config,
	policyDefinitions map[string]api.PolicyDefinition,
) *EventListener {
	ctx, cancel := context.WithCancel(context.Background())
	return &EventListener{
		eventSource:       eventSource,
		store:             store,
		db:                db,
		snapshotManager:   snapshotManager,
		policyManager:     policyManager,
		routerConfig:      routerConfig,
		logger:            logger,
		systemConfig:      systemConfig,
		policyDefinitions: policyDefinitions,
		eventCh:           make(chan Event, 100),
		ctx:               ctx,
		cancel:            cancel,
	}
}

// Start begins listening for events
func (l *EventListener) Start(ctx context.Context) error {
	// Subscribe to "default" organization events
	if err := l.eventSource.Subscribe(ctx, "default", l.eventCh); err != nil {
		return err
	}

	// Start processing goroutine
	go l.processEvents()

	l.logger.Info("Event listener started")
	return nil
}

// Stop gracefully stops the event listener
func (l *EventListener) Stop() {
	l.cancel()
	if err := l.eventSource.Close(); err != nil {
		l.logger.Warn("Error closing event source", slog.Any("error", err))
	}
	l.logger.Info("Event listener stopped")
}

// processEvents handles incoming events from the event source
func (l *EventListener) processEvents() {
	for {
		select {
		case <-l.ctx.Done():
			return
		case event, ok := <-l.eventCh:
			if !ok {
				l.logger.Info("Event channel closed, stopping event processing")
				return
			}
			l.handleEvent(event)
		}
	}
}

// handleEvent dispatches events to the appropriate handler by event type
func (l *EventListener) handleEvent(event Event) {
	l.logger.Info("Processing replica sync event",
		slog.String("event_type", event.EventType),
		slog.String("action", event.Action),
		slog.String("entity_id", event.EntityID),
		slog.String("correlation_id", event.CorrelationID))

	switch event.EventType {
	case "API":
		l.processAPIEvent(event)
	case "CERTIFICATE":
		l.logger.Info("Certificate event received (processing not yet implemented)",
			slog.String("entity_id", event.EntityID))
	case "LLM_TEMPLATE":
		l.logger.Info("LLM template event received (processing not yet implemented)",
			slog.String("entity_id", event.EntityID))
	default:
		l.logger.Warn("Unknown event type received",
			slog.String("event_type", event.EventType),
			slog.String("entity_id", event.EntityID))
	}
}
