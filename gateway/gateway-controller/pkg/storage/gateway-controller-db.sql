-- SQLite Schema for Gateway-Controller API Configurations
-- Version: 6.0
-- Description: Persistent storage for API configurations with lifecycle metadata and multi-instance sync support

-- Main table for deployments
CREATE TABLE IF NOT EXISTS deployments (
    -- Primary identifier (UUID)
    id TEXT PRIMARY KEY,

    -- Extracted fields for fast querying
    display_name TEXT NOT NULL,
    version TEXT NOT NULL,
    context TEXT NOT NULL,              -- Base path (e.g., "/weather")
    kind TEXT NOT NULL,                 -- Deployment type: "RestApi", "graphql", "grpc", "asyncapi"
    handle TEXT NOT NULL UNIQUE,        -- API handle (e.g., petstore-v1.0)

    -- Deployment status
    status TEXT NOT NULL CHECK(status IN ('pending', 'deployed', 'failed')),

    -- Timestamps
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deployed_at TIMESTAMP,               -- NULL until first deployment

    -- Version tracking for xDS snapshots
    deployed_version INTEGER NOT NULL DEFAULT 0,

    -- Composite unique constraint (API display_name + version must be unique)
    UNIQUE(display_name, version)
);

-- Indexes for fast lookups
-- Composite index for display_name+version lookups (most common query)
CREATE INDEX IF NOT EXISTS idx_name_version ON deployments(display_name, version);

-- Filter by deployment status (translator queries pending configs)
CREATE INDEX IF NOT EXISTS idx_status ON deployments(status);

-- Filter by context path (conflict detection)
CREATE INDEX IF NOT EXISTS idx_context ON deployments(context);

-- Filter by API type (reporting/analytics)
CREATE INDEX IF NOT EXISTS idx_kind ON deployments(kind);

-- Note: Policy definitions are no longer stored in the database.
-- They are loaded from files at controller startup (see policies/ directory).
-- The policy_definitions table has been removed as of schema version 3.

-- Table for custom TLS certificates
CREATE TABLE IF NOT EXISTS certificates (
    -- Primary identifier (UUID)
    id TEXT PRIMARY KEY,
    
    -- Human-readable name for the certificate
    name TEXT NOT NULL UNIQUE,
    
    -- PEM-encoded certificate(s) as BLOB
    certificate BLOB NOT NULL,
    
    -- Certificate metadata (extracted from first cert in bundle)
    subject TEXT NOT NULL,
    issuer TEXT NOT NULL,
    not_before TIMESTAMP NOT NULL,
    not_after TIMESTAMP NOT NULL,
    cert_count INTEGER NOT NULL DEFAULT 1,
    
    -- Timestamps
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Index for fast name lookups
CREATE INDEX IF NOT EXISTS idx_cert_name ON certificates(name);

-- Index for expiry tracking
CREATE INDEX IF NOT EXISTS idx_cert_expiry ON certificates(not_after);


-- Table for deployment-specific configurations
CREATE TABLE IF NOT EXISTS deployment_configs (
    id TEXT PRIMARY KEY,
    configuration TEXT NOT NULL,        -- JSON-serialized APIConfiguration
    source_configuration TEXT,          -- JSON-serialized SourceConfiguration
    FOREIGN KEY(id) REFERENCES deployments(id) ON DELETE CASCADE
);

-- LLM Provider Templates table (added in schema version 4)
CREATE TABLE IF NOT EXISTS llm_provider_templates (
    -- Primary identifier (UUID)
    id TEXT PRIMARY KEY,

    -- Template handle (must be unique)
    handle TEXT NOT NULL UNIQUE,

    -- Full template configuration as JSON
    configuration TEXT NOT NULL,

    -- Timestamps
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Index for fast name lookups
CREATE INDEX IF NOT EXISTS idx_template_handle ON llm_provider_templates(handle);

-- Entity States table (added in schema version 6)
-- Tracks the current version for each entity type per organization
CREATE TABLE IF NOT EXISTS entity_states (
    entity_type TEXT NOT NULL,
    organization_id TEXT NOT NULL DEFAULT 'default',
    version_id TEXT NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (entity_type, organization_id)
);

CREATE INDEX IF NOT EXISTS idx_entity_states_updated ON entity_states(updated_at);

-- API Events table (added in schema version 6)
-- Stores events for API entity changes
CREATE TABLE IF NOT EXISTS api_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    processed_timestamp TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    originated_timestamp TIMESTAMP NOT NULL,
    organization_id TEXT NOT NULL DEFAULT 'default',
    action TEXT NOT NULL CHECK(action IN ('CREATE', 'UPDATE', 'DELETE')),
    entity_id TEXT NOT NULL,
    event_data TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_api_events_lookup ON api_events(organization_id, processed_timestamp);

-- Certificate Events table (added in schema version 6)
-- Stores events for certificate entity changes
CREATE TABLE IF NOT EXISTS certificate_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    processed_timestamp TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    originated_timestamp TIMESTAMP NOT NULL,
    organization_id TEXT NOT NULL DEFAULT 'default',
    action TEXT NOT NULL CHECK(action IN ('CREATE', 'UPDATE', 'DELETE')),
    entity_id TEXT NOT NULL,
    event_data TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_cert_events_lookup ON certificate_events(organization_id, processed_timestamp);

-- LLM Template Events table (added in schema version 6)
-- Stores events for LLM template entity changes
CREATE TABLE IF NOT EXISTS llm_template_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    processed_timestamp TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    originated_timestamp TIMESTAMP NOT NULL,
    organization_id TEXT NOT NULL DEFAULT 'default',
    action TEXT NOT NULL CHECK(action IN ('CREATE', 'UPDATE', 'DELETE')),
    entity_id TEXT NOT NULL,
    event_data TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_llm_events_lookup ON llm_template_events(organization_id, processed_timestamp);

-- Set schema version to 6
PRAGMA user_version = 6;
