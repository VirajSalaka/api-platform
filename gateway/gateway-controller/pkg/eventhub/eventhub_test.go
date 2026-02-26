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

package eventhub

import (
	"database/sql"
	"log/slog"
	"os"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON")
	require.NoError(t, err)

	// Create required tables
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS organization_states (
			organization TEXT PRIMARY KEY,
			version_id TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS events (
			organization_id TEXT NOT NULL,
			processed_timestamp TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			originated_timestamp TIMESTAMP NOT NULL,
			event_type TEXT NOT NULL,
			action TEXT NOT NULL CHECK(action IN ('CREATE', 'UPDATE', 'DELETE')),
			entity_id TEXT NOT NULL,
			correlation_id TEXT NOT NULL DEFAULT '',
			event_data TEXT NOT NULL,
			PRIMARY KEY (organization_id, processed_timestamp)
		);
	`)
	require.NoError(t, err)

	t.Cleanup(func() { db.Close() })
	return db
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestRegisterOrganization(t *testing.T) {
	db := setupTestDB(t)
	logger := testLogger()

	hub := New(db, logger, DefaultConfig())
	require.NoError(t, hub.Initialize())
	defer hub.Close()

	err := hub.RegisterOrganization("test-org")
	assert.NoError(t, err)

	// Verify in database
	var org string
	err = db.QueryRow("SELECT organization FROM organization_states WHERE organization = ?", "test-org").Scan(&org)
	assert.NoError(t, err)
	assert.Equal(t, "test-org", org)
}

func TestPublishAndSubscribe(t *testing.T) {
	db := setupTestDB(t)
	logger := testLogger()

	config := Config{
		PollInterval:    100 * time.Millisecond,
		CleanupInterval: 5 * time.Minute,
		RetentionPeriod: 1 * time.Hour,
	}
	hub := New(db, logger, config)
	require.NoError(t, hub.Initialize())
	defer hub.Close()

	// Register org
	require.NoError(t, hub.RegisterOrganization("test-org"))

	// Subscribe
	ch, err := hub.Subscribe("test-org")
	require.NoError(t, err)

	// Publish event
	event := Event{
		OrganizationID:      "test-org",
		OriginatedTimestamp: time.Now(),
		EventType:          EventTypeAPI,
		Action:             "CREATE",
		EntityID:           "api-123",
		CorrelationID:      "corr-456",
		EventData:          `{"name":"test-api"}`,
	}
	require.NoError(t, hub.PublishEvent("test-org", event))

	// Wait for event to be delivered
	select {
	case received := <-ch:
		assert.Equal(t, EventTypeAPI, received.EventType)
		assert.Equal(t, "CREATE", received.Action)
		assert.Equal(t, "api-123", received.EntityID)
		assert.Equal(t, `{"name":"test-api"}`, received.EventData)
	case <-time.After(5 * time.Second):
		t.Fatal("Timed out waiting for event")
	}
}

func TestCleanUpEvents(t *testing.T) {
	db := setupTestDB(t)
	logger := testLogger()

	hub := New(db, logger, DefaultConfig())
	require.NoError(t, hub.Initialize())
	defer hub.Close()

	require.NoError(t, hub.RegisterOrganization("test-org"))

	// Insert old event directly
	oldTime := time.Now().Add(-2 * time.Hour)
	_, err := db.Exec(`
		INSERT INTO events (organization_id, processed_timestamp, originated_timestamp, event_type, action, entity_id, event_data)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "test-org", oldTime, oldTime, "API", "CREATE", "old-api", "{}")
	require.NoError(t, err)

	// Cleanup
	require.NoError(t, hub.CleanUpEvents())

	// Verify old event was deleted
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM events WHERE entity_id = 'old-api'").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestAtomicPublish(t *testing.T) {
	db := setupTestDB(t)
	logger := testLogger()

	hub := New(db, logger, DefaultConfig())
	require.NoError(t, hub.Initialize())
	defer hub.Close()

	require.NoError(t, hub.RegisterOrganization("test-org"))

	// Publish event
	event := Event{
		OriginatedTimestamp: time.Now(),
		EventType:          EventTypeAPI,
		Action:             "CREATE",
		EntityID:           "api-1",
		EventData:          `{"test":"data"}`,
	}
	require.NoError(t, hub.PublishEvent("test-org", event))

	// Verify both event and version were updated atomically
	var eventCount int
	err := db.QueryRow("SELECT COUNT(*) FROM events WHERE organization_id = 'test-org'").Scan(&eventCount)
	require.NoError(t, err)
	assert.Equal(t, 1, eventCount)

	var versionID string
	err = db.QueryRow("SELECT version_id FROM organization_states WHERE organization = 'test-org'").Scan(&versionID)
	require.NoError(t, err)
	assert.NotEmpty(t, versionID)
}

func TestMultipleSubscribers(t *testing.T) {
	db := setupTestDB(t)
	logger := testLogger()

	config := Config{
		PollInterval:    100 * time.Millisecond,
		CleanupInterval: 5 * time.Minute,
		RetentionPeriod: 1 * time.Hour,
	}
	hub := New(db, logger, config)
	require.NoError(t, hub.Initialize())
	defer hub.Close()

	require.NoError(t, hub.RegisterOrganization("test-org"))

	// Subscribe twice
	ch1, err := hub.Subscribe("test-org")
	require.NoError(t, err)
	ch2, err := hub.Subscribe("test-org")
	require.NoError(t, err)

	// Publish event
	event := Event{
		OriginatedTimestamp: time.Now(),
		EventType:          EventTypeAPI,
		Action:             "UPDATE",
		EntityID:           "api-multi",
		EventData:          `{}`,
	}
	require.NoError(t, hub.PublishEvent("test-org", event))

	// Both subscribers should receive the event
	for _, ch := range []<-chan Event{ch1, ch2} {
		select {
		case received := <-ch:
			assert.Equal(t, "api-multi", received.EntityID)
		case <-time.After(5 * time.Second):
			t.Fatal("Timed out waiting for event on subscriber")
		}
	}
}

func TestGracefulShutdown(t *testing.T) {
	db := setupTestDB(t)
	logger := testLogger()

	hub := New(db, logger, DefaultConfig())
	require.NoError(t, hub.Initialize())

	require.NoError(t, hub.RegisterOrganization("test-org"))
	_, err := hub.Subscribe("test-org")
	require.NoError(t, err)

	// Close should not panic or hang
	err = hub.Close()
	assert.NoError(t, err)
}

func TestMultipleEventTypes(t *testing.T) {
	db := setupTestDB(t)
	logger := testLogger()

	config := Config{
		PollInterval:    100 * time.Millisecond,
		CleanupInterval: 5 * time.Minute,
		RetentionPeriod: 1 * time.Hour,
	}
	hub := New(db, logger, config)
	require.NoError(t, hub.Initialize())
	defer hub.Close()

	require.NoError(t, hub.RegisterOrganization("test-org"))

	ch, err := hub.Subscribe("test-org")
	require.NoError(t, err)

	// Publish different event types
	events := []Event{
		{OriginatedTimestamp: time.Now(), EventType: EventTypeAPI, Action: "CREATE", EntityID: "api-1", EventData: "{}"},
		{OriginatedTimestamp: time.Now(), EventType: EventTypeCertificate, Action: "CREATE", EntityID: "cert-1", EventData: "{}"},
		{OriginatedTimestamp: time.Now(), EventType: EventTypeLLMTemplate, Action: "UPDATE", EntityID: "tmpl-1", EventData: "{}"},
	}

	for _, evt := range events {
		// Small delay to avoid primary key conflict (processed_timestamp)
		time.Sleep(10 * time.Millisecond)
		require.NoError(t, hub.PublishEvent("test-org", evt))
	}

	// Receive all events
	received := make([]Event, 0, len(events))
	timeout := time.After(5 * time.Second)
	for len(received) < len(events) {
		select {
		case evt := <-ch:
			received = append(received, evt)
		case <-timeout:
			t.Fatalf("Timed out waiting for events, received %d/%d", len(received), len(events))
		}
	}

	assert.Len(t, received, 3)
}
