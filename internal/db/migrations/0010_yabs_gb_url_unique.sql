-- 0010_yabs_gb_url_unique.sql — atomic geekbench-URL dedup.

-- Dedupe pre-existing (server_id, gb_url) duplicates, keeping the lowest id.
DELETE FROM yabs WHERE gb_url != '' AND id NOT IN (
    SELECT MIN(id) FROM yabs WHERE gb_url != ''
    GROUP BY server_id, gb_url
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_yabs_server_gburl
    ON yabs(server_id, gb_url) WHERE gb_url != '';
