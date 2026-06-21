-- Make audit_logs.org_id nullable + ON DELETE SET NULL so deleting an
-- organization preserves its audit trail. Add org_name_snapshot so the
-- post-cascade rows remain human-readable.
--
-- Without this, an attacker (or compromised owner) calling DELETE
-- /api/orgs/{id} cascades the org's audit_logs rows out of existence —
-- including the org.delete event itself. The audit trail must survive
-- destructive ops to be useful for post-incident forensics.

-- Drop existing FK to alter ON DELETE behavior and lift NOT NULL.
ALTER TABLE audit_logs DROP CONSTRAINT audit_logs_org_id_fkey;
ALTER TABLE audit_logs ALTER COLUMN org_id DROP NOT NULL;
ALTER TABLE audit_logs ADD CONSTRAINT audit_logs_org_id_fkey
  FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE SET NULL;

ALTER TABLE audit_logs ADD COLUMN org_name_snapshot TEXT;
