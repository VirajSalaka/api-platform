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
	"time"
)

// Event represents a generic, source-agnostic change event
type Event struct {
	OrganizationID string
	EventType      string
	Action         string
	EntityID       string
	CorrelationID  string
	EventData      string
	Timestamp      time.Time
}

// EventSource is the interface for subscribing to change events.
// This abstraction allows swapping the underlying event transport
// (e.g., EventHub/SQLite polling, Kafka, NATS) without changing the listener.
type EventSource interface {
	// Subscribe starts receiving events for the given organization.
	// Events are delivered to the provided channel.
	Subscribe(ctx context.Context, orgID string, ch chan<- Event) error
	// Unsubscribe stops receiving events for the given organization.
	Unsubscribe(orgID string) error
	// Close gracefully shuts down the event source.
	Close() error
}
