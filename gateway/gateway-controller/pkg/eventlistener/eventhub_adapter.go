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

package eventlistener

import (
	"context"
	"log/slog"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/eventhub"
)

// EventHubAdapter adapts an eventhub.EventHub to the EventSource interface
type EventHubAdapter struct {
	eventHub eventhub.EventHub
	logger   *slog.Logger
	cancel   context.CancelFunc
}

// NewEventHubAdapter creates a new EventHubAdapter
func NewEventHubAdapter(hub eventhub.EventHub, logger *slog.Logger) EventSource {
	return &EventHubAdapter{
		eventHub: hub,
		logger:   logger,
	}
}

// Subscribe starts receiving events from the EventHub for the given organization
func (a *EventHubAdapter) Subscribe(ctx context.Context, orgID string, ch chan<- Event) error {
	hubCh, err := a.eventHub.Subscribe(orgID)
	if err != nil {
		return err
	}

	bridgeCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel

	go a.bridgeEvents(bridgeCtx, hubCh, ch, orgID)
	return nil
}

// bridgeEvents converts eventhub.Event to eventlistener.Event and forwards
func (a *EventHubAdapter) bridgeEvents(ctx context.Context, hubCh <-chan eventhub.Event, listenerCh chan<- Event, orgID string) {
	for {
		select {
		case <-ctx.Done():
			return
		case hubEvent, ok := <-hubCh:
			if !ok {
				a.logger.Info("EventHub channel closed", slog.String("organization", orgID))
				return
			}

			event := Event{
				OrganizationID: hubEvent.OrganizationID,
				EventType:      string(hubEvent.EventType),
				Action:         hubEvent.Action,
				EntityID:       hubEvent.EntityID,
				CorrelationID:  hubEvent.CorrelationID,
				EventData:      hubEvent.EventData,
				Timestamp:      hubEvent.ProcessedTimestamp,
			}

			select {
			case listenerCh <- event:
			case <-ctx.Done():
				return
			}
		}
	}
}

// Unsubscribe stops receiving events for the given organization
func (a *EventHubAdapter) Unsubscribe(orgID string) error {
	if a.cancel != nil {
		a.cancel()
	}
	return nil
}

// Close gracefully shuts down the adapter
func (a *EventHubAdapter) Close() error {
	if a.cancel != nil {
		a.cancel()
	}
	return nil
}
