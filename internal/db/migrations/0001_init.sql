-- 0001_init.sql — initial schema for idlerthing.

-- Lookup tables
CREATE TABLE providers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE locations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE os (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE
);

-- Servers
CREATE TABLE servers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    hostname TEXT NOT NULL,
    server_type INT NOT NULL DEFAULT 1, -- 1 KVM, 2 OVZ, 3 DEDI, 4 LXC, 5 SEMI-DEDI, 6 VMware, 7 NAT
    os_id INT REFERENCES os(id),
    provider_id INT REFERENCES providers(id),
    location_id INT REFERENCES locations(id),
    ram_as_mb INT,
    cpu INT, -- cores
    cpu_model TEXT,
    bandwidth INT, -- TB
    link_speed INT, -- mbps
    network_type TEXT, -- e.g. 'IPv4+IPv6'
    ns1 TEXT,
    ns2 TEXT,
    ssh_port INT DEFAULT 22,
    active INT NOT NULL DEFAULT 1,
    show_public INT NOT NULL DEFAULT 0,
    has_yabs INT NOT NULL DEFAULT 0,
    was_promo INT NOT NULL DEFAULT 0,
    transferrable INT NOT NULL DEFAULT 0,
    owned_since TEXT, -- date
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE server_disks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    server_id INT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    size_as_gb INT NOT NULL,
    media TEXT NOT NULL DEFAULT 'SSD' -- SSD/HDD/NVMe
);

-- Shared / reseller hosting
CREATE TABLE shared_hosting (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    main_domain TEXT NOT NULL,
    shared_type TEXT,
    provider_id INT REFERENCES providers(id),
    location_id INT REFERENCES locations(id),
    domains_limit INT,
    subdomains_limit INT,
    ftp_limit INT,
    email_limit INT,
    db_limit INT,
    disk_as_gb INT,
    bandwidth INT,
    has_dedicated_ip INT DEFAULT 0,
    ip TEXT,
    active INT NOT NULL DEFAULT 1,
    show_public INT NOT NULL DEFAULT 0,
    was_promo INT NOT NULL DEFAULT 0,
    owned_since TEXT,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE reseller_hosting (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    main_domain TEXT NOT NULL,
    reseller_type TEXT,
    provider_id INT REFERENCES providers(id),
    location_id INT REFERENCES locations(id),
    domains_limit INT,
    subdomains_limit INT,
    ftp_limit INT,
    email_limit INT,
    db_limit INT,
    disk_as_gb INT,
    bandwidth INT,
    has_dedicated_ip INT DEFAULT 0,
    ip TEXT,
    active INT NOT NULL DEFAULT 1,
    show_public INT NOT NULL DEFAULT 0,
    was_promo INT NOT NULL DEFAULT 0,
    owned_since TEXT,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Seedboxes
CREATE TABLE seedboxes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    title TEXT,
    hostname TEXT NOT NULL,
    seed_box_type TEXT,
    provider_id INT REFERENCES providers(id),
    location_id INT REFERENCES locations(id),
    port_speed INT,
    disk_as_gb INT,
    bandwidth INT,
    active INT NOT NULL DEFAULT 1,
    show_public INT NOT NULL DEFAULT 0,
    was_promo INT NOT NULL DEFAULT 0,
    owned_since TEXT,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Domains
CREATE TABLE domains (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    domain TEXT NOT NULL,
    extension TEXT,
    ns1 TEXT,
    ns2 TEXT,
    ns3 TEXT,
    provider_id INT REFERENCES providers(id),
    active INT NOT NULL DEFAULT 1,
    owned_since TEXT,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Misc services
CREATE TABLE misc_services (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    active INT NOT NULL DEFAULT 1,
    owned_since TEXT,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Pricing (polymorphic; service_type: 1 server, 2 shared, 3 reseller, 4 domain, 5 misc, 6 seedbox)
CREATE TABLE pricings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    service_id INT NOT NULL,
    service_type INT NOT NULL,
    currency TEXT NOT NULL DEFAULT 'USD',
    price REAL NOT NULL,
    term INT NOT NULL DEFAULT 1, -- 1 monthly, 2 quarterly, 3 semiannual, 4 annual, 5 biennial, 6 triennial, 7 one-time
    next_due_date TEXT,
    active INT NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (service_id, service_type)
);

-- IPs (polymorphic; service_type as in pricings)
CREATE TABLE ips (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    service_id INT NOT NULL,
    service_type INT NOT NULL DEFAULT 1,
    address TEXT NOT NULL,
    is_ipv4 INT NOT NULL DEFAULT 1,
    country TEXT,
    region TEXT,
    city TEXT,
    org TEXT,
    isp TEXT,
    asn TEXT,
    fetched_at TEXT,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (service_id, service_type, address)
);

CREATE INDEX idx_ips_service_id ON ips(service_id);

-- DNS records
CREATE TABLE dns (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    hostname TEXT NOT NULL,
    dns_type TEXT NOT NULL DEFAULT 'A',
    address TEXT NOT NULL,
    server_id INT REFERENCES servers(id) ON DELETE SET NULL,
    domain_id INT REFERENCES domains(id) ON DELETE SET NULL,
    shared_id INT REFERENCES shared_hosting(id) ON DELETE SET NULL,
    reseller_id INT REFERENCES reseller_hosting(id) ON DELETE SET NULL,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_dns_server_id ON dns(server_id);

-- Labels
CREATE TABLE labels (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    label TEXT NOT NULL UNIQUE
);

CREATE TABLE labels_assigned (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    label_id INT NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
    service_id INT NOT NULL,
    service_type INT NOT NULL,
    UNIQUE (label_id, service_id, service_type)
);

CREATE INDEX idx_labels_assigned_service ON labels_assigned(service_id, service_type);

-- Notes (attach to a service OR an ip)
CREATE TABLE notes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    service_id INT,
    service_type INT,
    ip_id INT REFERENCES ips(id) ON DELETE CASCADE,
    body TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- YABS benchmark results
CREATE TABLE yabs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    server_id INT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    run_at TEXT,
    cpu TEXT,
    cpu_cores INT,
    ram TEXT,
    swap TEXT,
    distro TEXT,
    kernel TEXT,
    uptime TEXT,
    geekbench_version INT,
    gb_single INT,
    gb_multi INT,
    gb_url TEXT,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_yabs_server_id ON yabs(server_id);

CREATE TABLE yabs_disk_speed (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    yabs_id INT NOT NULL REFERENCES yabs(id) ON DELETE CASCADE,
    block_size TEXT, -- e.g. '4k', '64k', '512k', '1m'
    read_mbps REAL,
    write_mbps REAL
);

CREATE TABLE yabs_network_speed (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    yabs_id INT NOT NULL REFERENCES yabs(id) ON DELETE CASCADE,
    location TEXT,
    provider TEXT,
    send_mbps REAL,
    recv_mbps REAL,
    latency_ms REAL
);

-- Settings (single row, id always 1)
CREATE TABLE settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    default_currency TEXT DEFAULT 'USD',
    dashboard_currency TEXT DEFAULT 'USD',
    due_soon_amount INT DEFAULT 14,
    recently_added_amount INT DEFAULT 5,
    theme TEXT DEFAULT 'dark',
    servers_public INT DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO settings (id) VALUES (1);

-- Auth
CREATE TABLE users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    email TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    api_token_hash TEXT, -- sha256 hex of token
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE sessions (
    token TEXT PRIMARY KEY,
    user_id INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_token TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_sessions_expires_at ON sessions(expires_at);

CREATE TABLE user_prefs (
    user_id INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key TEXT NOT NULL,
    value TEXT,
    PRIMARY KEY (user_id, key)
);
