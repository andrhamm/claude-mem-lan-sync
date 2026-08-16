CREATE TABLE schema_versions (
        id INTEGER PRIMARY KEY,
        version INTEGER UNIQUE NOT NULL,
        applied_at TEXT NOT NULL
      );
CREATE TABLE sdk_sessions (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        content_session_id TEXT NOT NULL,
        memory_session_id TEXT UNIQUE,
        project TEXT NOT NULL,
        platform_source TEXT NOT NULL DEFAULT 'claude',
        user_prompt TEXT,
        started_at TEXT NOT NULL,
        started_at_epoch INTEGER NOT NULL,
        completed_at TEXT,
        completed_at_epoch INTEGER,
        status TEXT CHECK(status IN ('active', 'completed', 'failed')) NOT NULL DEFAULT 'active'
      , worker_port INTEGER, prompt_counter INTEGER DEFAULT 0, custom_title TEXT);
CREATE INDEX idx_sdk_sessions_claude_id ON sdk_sessions(content_session_id);
CREATE INDEX idx_sdk_sessions_sdk_id ON sdk_sessions(memory_session_id);
CREATE INDEX idx_sdk_sessions_project ON sdk_sessions(project);
CREATE INDEX idx_sdk_sessions_status ON sdk_sessions(status);
CREATE INDEX idx_sdk_sessions_started ON sdk_sessions(started_at_epoch DESC);
CREATE VIRTUAL TABLE user_prompts_fts USING fts5(
        prompt_text,
        content='user_prompts',
        content_rowid='id'
      )
/* user_prompts_fts(prompt_text) */;
CREATE TABLE pending_messages (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        session_db_id INTEGER NOT NULL,
        content_session_id TEXT NOT NULL,
        message_type TEXT NOT NULL CHECK(message_type IN ('observation', 'summarize')),
        tool_name TEXT,
        tool_input TEXT,
        tool_response TEXT,
        cwd TEXT,
        last_user_message TEXT,
        last_assistant_message TEXT,
        prompt_number INTEGER,
        status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending', 'processing')),
        created_at_epoch INTEGER NOT NULL, agent_type TEXT, agent_id TEXT, tool_use_id TEXT,
        FOREIGN KEY (session_db_id) REFERENCES sdk_sessions(id) ON DELETE CASCADE
      );
CREATE INDEX idx_pending_messages_session ON pending_messages(session_db_id);
CREATE INDEX idx_pending_messages_status ON pending_messages(status);
CREATE INDEX idx_pending_messages_claude_session ON pending_messages(content_session_id);
CREATE TABLE "observations" (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        memory_session_id TEXT NOT NULL,
        project TEXT NOT NULL,
        text TEXT,
        type TEXT NOT NULL,
        title TEXT,
        subtitle TEXT,
        facts TEXT,
        narrative TEXT,
        concepts TEXT,
        files_read TEXT,
        files_modified TEXT,
        prompt_number INTEGER,
        discovery_tokens INTEGER DEFAULT 0,
        created_at TEXT NOT NULL,
        created_at_epoch INTEGER NOT NULL, content_hash TEXT, generated_by_model TEXT, relevance_count INTEGER DEFAULT 0, merged_into_project TEXT, agent_type TEXT, agent_id TEXT, metadata TEXT, synced_at INTEGER, origin_device_id TEXT, origin_local_id TEXT, sync_rev TEXT NOT NULL DEFAULT '1',
        FOREIGN KEY(memory_session_id) REFERENCES sdk_sessions(memory_session_id) ON DELETE CASCADE ON UPDATE CASCADE
      );
CREATE INDEX idx_observations_sdk_session ON observations(memory_session_id);
CREATE INDEX idx_observations_project ON observations(project);
CREATE INDEX idx_observations_type ON observations(type);
CREATE INDEX idx_observations_created ON observations(created_at_epoch DESC);
CREATE TABLE "session_summaries" (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        memory_session_id TEXT NOT NULL,
        project TEXT NOT NULL,
        request TEXT,
        investigated TEXT,
        learned TEXT,
        completed TEXT,
        next_steps TEXT,
        files_read TEXT,
        files_edited TEXT,
        notes TEXT,
        prompt_number INTEGER,
        discovery_tokens INTEGER DEFAULT 0,
        created_at TEXT NOT NULL,
        created_at_epoch INTEGER NOT NULL, merged_into_project TEXT, synced_at INTEGER, origin_device_id TEXT, origin_local_id TEXT, sync_rev TEXT NOT NULL DEFAULT '1',
        FOREIGN KEY(memory_session_id) REFERENCES sdk_sessions(memory_session_id) ON DELETE CASCADE ON UPDATE CASCADE
      );
CREATE INDEX idx_session_summaries_sdk_session ON session_summaries(memory_session_id);
CREATE INDEX idx_session_summaries_project ON session_summaries(project);
CREATE INDEX idx_session_summaries_created ON session_summaries(created_at_epoch DESC);
CREATE INDEX idx_observations_content_hash ON observations(content_hash, created_at_epoch);
CREATE INDEX idx_sdk_sessions_platform_source ON sdk_sessions(platform_source);
CREATE INDEX idx_observations_merged_into ON observations(merged_into_project);
CREATE INDEX idx_summaries_merged_into ON session_summaries(merged_into_project);
CREATE INDEX idx_observations_agent_type ON observations(agent_type);
CREATE INDEX idx_observations_agent_id ON observations(agent_id);
CREATE UNIQUE INDEX ux_observations_session_hash
      ON observations(memory_session_id, content_hash)
    ;
CREATE UNIQUE INDEX ux_sdk_sessions_platform_content ON sdk_sessions(platform_source, content_session_id);
CREATE TABLE "user_prompts" (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        session_db_id INTEGER,
        content_session_id TEXT NOT NULL,
        prompt_number INTEGER NOT NULL,
        prompt_text TEXT NOT NULL,
        created_at TEXT NOT NULL,
        created_at_epoch INTEGER NOT NULL, synced_at INTEGER, origin_device_id TEXT, origin_local_id TEXT, sync_rev TEXT NOT NULL DEFAULT '1',
        FOREIGN KEY(session_db_id) REFERENCES sdk_sessions(id) ON DELETE CASCADE
      );
CREATE INDEX idx_user_prompts_session ON user_prompts(session_db_id);
CREATE INDEX idx_user_prompts_claude_session ON user_prompts(content_session_id);
CREATE INDEX idx_user_prompts_created ON user_prompts(created_at_epoch DESC);
CREATE INDEX idx_user_prompts_prompt_number ON user_prompts(prompt_number);
CREATE INDEX idx_user_prompts_lookup ON user_prompts(session_db_id, prompt_number);
CREATE INDEX idx_user_prompts_content_lookup ON user_prompts(content_session_id, prompt_number);
CREATE TRIGGER user_prompts_ai AFTER INSERT ON user_prompts BEGIN
          INSERT INTO user_prompts_fts(rowid, prompt_text)
          VALUES (new.id, new.prompt_text);
        END;
CREATE TRIGGER user_prompts_ad AFTER DELETE ON user_prompts BEGIN
          INSERT INTO user_prompts_fts(user_prompts_fts, rowid, prompt_text)
          VALUES('delete', old.id, old.prompt_text);
        END;
CREATE TRIGGER user_prompts_au AFTER UPDATE ON user_prompts BEGIN
          INSERT INTO user_prompts_fts(user_prompts_fts, rowid, prompt_text)
          VALUES('delete', old.id, old.prompt_text);
          INSERT INTO user_prompts_fts(rowid, prompt_text)
          VALUES (new.id, new.prompt_text);
        END;
CREATE UNIQUE INDEX ux_pending_session_tool
      ON pending_messages(session_db_id, tool_use_id)
      WHERE tool_use_id IS NOT NULL
    ;
CREATE INDEX idx_observations_unsynced ON observations(id) WHERE synced_at IS NULL;
CREATE INDEX idx_session_summaries_unsynced ON session_summaries(id) WHERE synced_at IS NULL;
CREATE INDEX idx_user_prompts_unsynced ON user_prompts(id) WHERE synced_at IS NULL;
CREATE UNIQUE INDEX ux_observations_origin
        ON observations(origin_device_id, origin_local_id)
        WHERE origin_device_id IS NOT NULL
      ;
CREATE UNIQUE INDEX ux_session_summaries_origin
        ON session_summaries(origin_device_id, origin_local_id)
        WHERE origin_device_id IS NOT NULL
      ;
CREATE UNIQUE INDEX ux_user_prompts_origin
        ON user_prompts(origin_device_id, origin_local_id)
        WHERE origin_device_id IS NOT NULL
      ;
CREATE TABLE sync_state (
        k TEXT PRIMARY KEY,
        v TEXT
      );
CREATE TABLE sync_outbox (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        op_uuid TEXT NOT NULL UNIQUE,
        rev TEXT NOT NULL DEFAULT '1',
        body TEXT NOT NULL,
        canonical_body TEXT,
        operation_sha256 TEXT,
        created_at_epoch INTEGER NOT NULL
      );
CREATE TABLE sync_entity_heads (
        entity_id TEXT PRIMARY KEY,
        kind TEXT NOT NULL CHECK (kind IN ('observation', 'summary', 'prompt')),
        origin_device_id TEXT NOT NULL,
        origin_local_id TEXT NOT NULL,
        entity_rev TEXT NOT NULL,
        operation_sha256 TEXT NOT NULL,
        deleted INTEGER NOT NULL CHECK (deleted IN (0, 1)),
        updated_at_epoch INTEGER NOT NULL
      );
CREATE TABLE sync_content_outbox (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        entity_id TEXT NOT NULL,
        kind TEXT NOT NULL CHECK (kind IN ('observation', 'summary', 'prompt')),
        origin_local_id TEXT NOT NULL,
        entity_rev TEXT NOT NULL,
        body TEXT NOT NULL,
        operation_sha256 TEXT NOT NULL,
        deleted INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0, 1)),
        created_at_epoch INTEGER NOT NULL,
        UNIQUE(entity_id, entity_rev)
      );
CREATE TABLE sync_dead_letter (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        lane TEXT NOT NULL CHECK (lane IN ('content', 'mutation')),
        queue_key TEXT NOT NULL,
        kind TEXT,
        origin_local_id TEXT,
        entity_rev TEXT,
        reason TEXT NOT NULL,
        raw_body TEXT,
        created_at_epoch INTEGER NOT NULL,
        UNIQUE(lane, queue_key, entity_rev, reason)
      );
CREATE TABLE sync_launch_exclusions (
        kind TEXT NOT NULL CHECK (kind IN ('observation', 'summary', 'prompt')),
        origin_local_id TEXT NOT NULL,
        through_rev TEXT NOT NULL,
        PRIMARY KEY (kind, origin_local_id)
      );
CREATE VIRTUAL TABLE observations_fts USING fts5(
        title,
        subtitle,
        narrative,
        text,
        facts,
        concepts,
        content='observations',
        content_rowid='id'
      )
/* observations_fts(title,subtitle,narrative,text,facts,concepts) */;
CREATE TRIGGER observations_ai AFTER INSERT ON observations BEGIN
        INSERT INTO observations_fts(rowid, title, subtitle, narrative, text, facts, concepts)
        VALUES (new.id, new.title, new.subtitle, new.narrative, new.text, new.facts, new.concepts);
      END;
CREATE TRIGGER observations_ad AFTER DELETE ON observations BEGIN
        INSERT INTO observations_fts(observations_fts, rowid, title, subtitle, narrative, text, facts, concepts)
        VALUES('delete', old.id, old.title, old.subtitle, old.narrative, old.text, old.facts, old.concepts);
      END;
CREATE TRIGGER observations_au AFTER UPDATE ON observations BEGIN
        INSERT INTO observations_fts(observations_fts, rowid, title, subtitle, narrative, text, facts, concepts)
        VALUES('delete', old.id, old.title, old.subtitle, old.narrative, old.text, old.facts, old.concepts);
        INSERT INTO observations_fts(rowid, title, subtitle, narrative, text, facts, concepts)
        VALUES (new.id, new.title, new.subtitle, new.narrative, new.text, new.facts, new.concepts);
      END;
CREATE VIRTUAL TABLE session_summaries_fts USING fts5(
        request,
        investigated,
        learned,
        completed,
        next_steps,
        notes,
        content='session_summaries',
        content_rowid='id'
      )
/* session_summaries_fts(request,investigated,learned,completed,next_steps,notes) */;
CREATE TRIGGER session_summaries_ai AFTER INSERT ON session_summaries BEGIN
        INSERT INTO session_summaries_fts(rowid, request, investigated, learned, completed, next_steps, notes)
        VALUES (new.id, new.request, new.investigated, new.learned, new.completed, new.next_steps, new.notes);
      END;
CREATE TRIGGER session_summaries_ad AFTER DELETE ON session_summaries BEGIN
        INSERT INTO session_summaries_fts(session_summaries_fts, rowid, request, investigated, learned, completed, next_steps, notes)
        VALUES('delete', old.id, old.request, old.investigated, old.learned, old.completed, old.next_steps, old.notes);
      END;
CREATE TRIGGER session_summaries_au AFTER UPDATE ON session_summaries BEGIN
        INSERT INTO session_summaries_fts(session_summaries_fts, rowid, request, investigated, learned, completed, next_steps, notes)
        VALUES('delete', old.id, old.request, old.investigated, old.learned, old.completed, old.next_steps, old.notes);
        INSERT INTO session_summaries_fts(rowid, request, investigated, learned, completed, next_steps, notes)
        VALUES (new.id, new.request, new.investigated, new.learned, new.completed, new.next_steps, new.notes);
      END;
