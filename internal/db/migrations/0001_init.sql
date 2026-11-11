-- Core schema for the submission system. See docs/design-doc.docx.

create table hackathons (
    id                    uuid primary key default gen_random_uuid(),
    slug                  text not null unique check (slug ~ '^[a-z0-9-]{1,64}$'),
    name                  text not null check (length(name) between 1 and 120),
    -- The deadline is compared only against Postgres now(), never a client clock.
    deadline_at           timestamptz not null,
    -- Bytes-only grace: a version created before the deadline may finish uploading
    -- until deadline_at + grace_seconds.
    grace_seconds         integer not null default 900 check (grace_seconds between 0 and 3600),
    max_file_bytes        bigint not null default 104857600 check (max_file_bytes > 0),
    max_versions_per_team integer not null default 10 check (max_versions_per_team > 0),
    -- Hard cap on bytes reserved by live versions, so storage never passes the free tier.
    storage_budget_bytes  bigint not null default 8589934592 check (storage_budget_bytes > 0),
    uploads_enabled       boolean not null default true,
    created_at            timestamptz not null default now()
);

create table users (
    id            uuid primary key default gen_random_uuid(),
    email         text not null unique check (email = lower(email) and length(email) between 3 and 254),
    display_name  text not null check (length(display_name) between 1 and 80),
    password_hash text not null,
    is_organizer  boolean not null default false,
    created_at    timestamptz not null default now()
);

create table teams (
    id           uuid primary key default gen_random_uuid(),
    hackathon_id uuid not null references hackathons (id),
    name         text not null check (length(name) between 1 and 80),
    created_at   timestamptz not null default now(),
    unique (hackathon_id, name)
);

-- Membership is the only access rule: a user may act on a team's
-- submissions only if a row here links them.
create table team_members (
    team_id   uuid not null references teams (id) on delete cascade,
    user_id   uuid not null references users (id) on delete cascade,
    joined_at timestamptz not null default now(),
    primary key (team_id, user_id)
);
create index team_members_user_idx on team_members (user_id);

-- Only a SHA-256 of each session token is stored.
create table sessions (
    token_hash bytea primary key check (length(token_hash) = 32),
    user_id    uuid not null references users (id) on delete cascade,
    created_at timestamptz not null default now(),
    expires_at timestamptz not null
);
create index sessions_user_idx on sessions (user_id);

create type version_state as enum ('uploading', 'checking', 'accepted', 'needs_fix', 'expired');

-- Append-only: an edit is a new version. Rows are never deleted.
create table versions (
    id                 uuid primary key default gen_random_uuid(),
    team_id            uuid not null references teams (id),
    number             integer not null check (number > 0),
    idempotency_key    text not null check (length(idempotency_key) between 8 and 128),
    created_by         uuid not null references users (id),
    title              text not null check (length(title) between 1 and 120),
    description        text not null check (length(description) <= 5000),
    links              text[] not null default '{}' check (cardinality(links) <= 5),
    file_name          text not null check (length(file_name) between 1 and 255),
    file_size          bigint not null check (file_size > 0),
    -- Fixed when the version is created; the worker rejects a file that differs.
    file_sha256        bytea not null check (length(file_sha256) = 32),
    object_key         text not null unique,
    upload_id          text,
    part_size          integer not null check (part_size > 0),
    state              version_state not null default 'uploading',
    state_reason       text,
    created_at         timestamptz not null default now(),
    completed_at       timestamptz,
    checks_finished_at timestamptz,
    unique (team_id, number),
    unique (team_id, idempotency_key)
);
create index versions_team_created_idx on versions (team_id, created_at desc);
create index versions_state_idx on versions (state);

-- One row per version and check, so a retried job overwrites instead of adding.
create table check_results (
    version_id uuid not null references versions (id) on delete cascade,
    check_name text not null,
    passed     boolean not null,
    detail     jsonb not null default '{}'::jsonb,
    checked_at timestamptz not null default now(),
    primary key (version_id, check_name)
);

create table late_attempts (
    id           bigint generated always as identity primary key,
    hackathon_id uuid not null references hackathons (id),
    team_id      uuid references teams (id),
    user_id      uuid references users (id),
    kind         text not null check (kind in ('create_after_deadline', 'sign_after_grace', 'complete_after_grace')),
    attempted_at timestamptz not null default now()
);
create index late_attempts_hackathon_idx on late_attempts (hackathon_id, attempted_at);
