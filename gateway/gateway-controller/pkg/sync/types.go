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

import "time"

// EntityType represents the type of entity being synchronized
type EntityType string

const (
	// EntityTypeAPI represents API entities
	EntityTypeAPI EntityType = "API"
)

// Action represents the type of change that occurred
type Action string

const (
	// ActionCreate represents a creation event
	ActionCreate Action = "CREATE"
	// ActionUpdate represents an update event
	ActionUpdate Action = "UPDATE"
	// ActionDelete represents a deletion event
	ActionDelete Action = "DELETE"
)

// EntityState tracks the current version of an entity type
type EntityState struct {
	EntityType EntityType
	VersionID  string
	UpdatedAt  time.Time
}

// Event represents a synchronization event
type Event struct {
	ID                  int64
	ProcessedTimestamp  time.Time
	OriginatedTimestamp time.Time
	Action              Action
	EntityID            string
	EventData           []byte // JSON encoded event data
}

// SyncCallback is a function that processes events for a specific entity type
type SyncCallback func(events []Event) error
