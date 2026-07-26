-- 0007_orphan_cleanup.sql — remove polymorphic rows whose parent service no
-- longer exists (service deletes before this fix left orphans behind).

-- pricings: parent table depends on service_type; out-of-range types go too.
DELETE FROM pricings WHERE
    (service_type = 1 AND NOT EXISTS (SELECT 1 FROM servers s WHERE s.id = service_id))
 OR (service_type = 2 AND NOT EXISTS (SELECT 1 FROM shared_hosting s WHERE s.id = service_id))
 OR (service_type = 3 AND NOT EXISTS (SELECT 1 FROM reseller_hosting s WHERE s.id = service_id))
 OR (service_type = 4 AND NOT EXISTS (SELECT 1 FROM domains s WHERE s.id = service_id))
 OR (service_type = 5 AND NOT EXISTS (SELECT 1 FROM misc_services s WHERE s.id = service_id))
 OR (service_type = 6 AND NOT EXISTS (SELECT 1 FROM seedboxes s WHERE s.id = service_id))
 OR (service_type < 1 OR service_type > 6);

-- ips: same parent rule.
DELETE FROM ips WHERE
    (service_type = 1 AND NOT EXISTS (SELECT 1 FROM servers s WHERE s.id = service_id))
 OR (service_type = 2 AND NOT EXISTS (SELECT 1 FROM shared_hosting s WHERE s.id = service_id))
 OR (service_type = 3 AND NOT EXISTS (SELECT 1 FROM reseller_hosting s WHERE s.id = service_id))
 OR (service_type = 4 AND NOT EXISTS (SELECT 1 FROM domains s WHERE s.id = service_id))
 OR (service_type = 5 AND NOT EXISTS (SELECT 1 FROM misc_services s WHERE s.id = service_id))
 OR (service_type = 6 AND NOT EXISTS (SELECT 1 FROM seedboxes s WHERE s.id = service_id))
 OR (service_type < 1 OR service_type > 6);

-- notes: service-keyed rows follow the parent rule; ip-keyed rows whose ip
-- is gone (FK should cascade, this is belt-and-braces) go too.
DELETE FROM notes WHERE
    (service_id IS NOT NULL AND (
        (service_type = 1 AND NOT EXISTS (SELECT 1 FROM servers s WHERE s.id = service_id))
     OR (service_type = 2 AND NOT EXISTS (SELECT 1 FROM shared_hosting s WHERE s.id = service_id))
     OR (service_type = 3 AND NOT EXISTS (SELECT 1 FROM reseller_hosting s WHERE s.id = service_id))
     OR (service_type = 4 AND NOT EXISTS (SELECT 1 FROM domains s WHERE s.id = service_id))
     OR (service_type = 5 AND NOT EXISTS (SELECT 1 FROM misc_services s WHERE s.id = service_id))
     OR (service_type = 6 AND NOT EXISTS (SELECT 1 FROM seedboxes s WHERE s.id = service_id))
     OR (service_type < 1 OR service_type > 6)))
 OR (ip_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM ips i WHERE i.id = ip_id));

-- labels_assigned: same parent rule.
DELETE FROM labels_assigned WHERE
    (service_type = 1 AND NOT EXISTS (SELECT 1 FROM servers s WHERE s.id = service_id))
 OR (service_type = 2 AND NOT EXISTS (SELECT 1 FROM shared_hosting s WHERE s.id = service_id))
 OR (service_type = 3 AND NOT EXISTS (SELECT 1 FROM reseller_hosting s WHERE s.id = service_id))
 OR (service_type = 4 AND NOT EXISTS (SELECT 1 FROM domains s WHERE s.id = service_id))
 OR (service_type = 5 AND NOT EXISTS (SELECT 1 FROM misc_services s WHERE s.id = service_id))
 OR (service_type = 6 AND NOT EXISTS (SELECT 1 FROM seedboxes s WHERE s.id = service_id))
 OR (service_type < 1 OR service_type > 6);
