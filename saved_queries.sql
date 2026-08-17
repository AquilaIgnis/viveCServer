-- @block
-- show all tables
SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename;


-- @block
-- accounts (skip password_hash)
SELECT id, email, created_at, change_seq, storage_bytes
FROM accounts
ORDER BY created_at DESC;


-- @block
-- Counts per account, one row per user:
SELECT a.email,
        count(DISTINCT n.id) FILTER (WHERE n.deleted_at IS NULL) AS notebooks,
        count(DISTINCT s.id) FILTER (WHERE s.deleted_at IS NULL) AS sections,
        count(DISTINCT p.id) FILTER (WHERE p.deleted_at IS NULL) AS pages,
        a.change_seq,
        pg_size_pretty(a.storage_bytes) AS storage
FROM accounts a
LEFT JOIN notebooks n ON n.account_id = a.id
LEFT JOIN sections  s ON s.account_id = a.id
LEFT JOIN pages     p ON p.account_id = a.id
GROUP BY a.id
ORDER BY a.email;

--@block
 -- The full tree for one user:
SELECT n.name AS notebook, s.name AS section, p.title AS page,
        p.sort_index, p.version, p.change_seq
FROM accounts a
JOIN notebooks n ON n.account_id = a.id AND n.deleted_at IS NULL
LEFT JOIN sections s ON s.account_id = a.id AND s.notebook_id = n.id AND s.deleted_at IS NULL
LEFT JOIN pages    p ON p.account_id = a.id AND p.section_id  = s.id AND p.deleted_at IS NULL
WHERE a.email = 'sample@mail.com'
ORDER BY n.sort_index, s.sort_index, p.sort_index;


--@block
-- Recent activity — what changed last, and from which device:
SELECT 'page' AS kind, p.title AS name, p.change_seq, p.server_updated_at, d.name AS device
FROM pages p
JOIN accounts a ON a.id = p.account_id
LEFT JOIN devices d ON d.id = p.last_writer
WHERE a.email = 'sample@mail.com'
ORDER BY p.change_seq DESC
LIMIT 20;

--@block
-- Their devices:
SELECT d.name, d.platform, d.created_at, d.last_seen_at, d.last_pulled_seq, d.revoked_at
FROM devices d JOIN accounts a ON a.id = d.account_id
WHERE a.email = 'sample@mail.com'
ORDER BY d.created_at;
