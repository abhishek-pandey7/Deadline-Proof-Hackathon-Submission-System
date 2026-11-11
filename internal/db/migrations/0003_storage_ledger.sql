-- One account-wide count of what the bucket holds, because R2's free tier
-- (10 GB) is per account, not per hackathon. Every new version reserves its
-- size here before any upload link exists; expiry gives it back; previews
-- add their size when stored. A periodic job also measures the real bucket,
-- and the limiter uses whichever is larger, so drift can only make it stricter.
create table storage_ledger (
    id                   boolean primary key default true check (id), -- exactly one row
    cap_bytes            bigint not null check (cap_bytes > 0),
    reserved_bytes       bigint not null default 0 check (reserved_bytes >= 0),
    uploaded_bytes_total bigint not null default 0 check (uploaded_bytes_total >= 0),
    measured_bytes       bigint,
    measured_objects     integer,
    measured_at          timestamptz,
    updated_at           timestamptz not null default now()
);

insert into storage_ledger (cap_bytes, reserved_bytes, uploaded_bytes_total)
select 9000000000,
       coalesce((select sum(file_size) from versions where state <> 'expired'), 0),
       coalesce((select sum(file_size) from versions where completed_at is not null), 0);

-- Size of the stored preview image, so a retried check job adjusts the
-- ledger by the difference instead of counting the preview twice.
alter table versions add column preview_bytes bigint not null default 0 check (preview_bytes >= 0);
