-- Bytes reserved by versions that are not expired. A version reserves its
-- declared size when it is created (before any upload link exists) and gives
-- it back if it expires, so the bucket can never grow past storage_budget_bytes.
alter table hackathons
    add column reserved_bytes bigint not null default 0,
    add constraint hackathons_reserved_within_budget check (reserved_bytes >= 0 and reserved_bytes <= storage_budget_bytes);
