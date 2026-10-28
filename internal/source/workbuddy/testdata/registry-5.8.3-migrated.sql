-- Schema only: the sessions table as created by a real, incrementally migrated WorkBuddy 5.8.3
-- (same 40 columns as registry-5.8.3.sql, different column order).
CREATE TABLE sessions (
                id TEXT PRIMARY KEY,
                cwd TEXT NOT NULL,
                user_id TEXT NOT NULL,
                title TEXT,
                custom_title TEXT,
                status TEXT NOT NULL DEFAULT 'Pending',
                created_at INTEGER NOT NULL,
                updated_at INTEGER NOT NULL,
                deleted_at INTEGER,
                is_playground INTEGER NOT NULL DEFAULT 0
            , model TEXT, last_activity_at INTEGER, source_mode TEXT, is_background_automation INTEGER, expert_id TEXT, expert_locale TEXT, expert_runtime_identity TEXT, expert_marketplace TEXT, permission_mode TEXT, use_sandbox_cli INTEGER, mode TEXT, project_id TEXT, plugin_context_json TEXT, last_user_prompt_expert_selection TEXT, context_window INTEGER, addon_selection TEXT, session_settings TEXT, thought_level TEXT, buddy_snapshot_id TEXT, buddy_binding_json TEXT, transport TEXT NOT NULL DEFAULT 'local', conversation_origin TEXT, visibility TEXT, group_id TEXT, group_title TEXT, agent_dirty INTEGER, agent_dirty_at INTEGER, agent_last_synced INTEGER, verified_at INTEGER, unread INTEGER NOT NULL DEFAULT 0);
