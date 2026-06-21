-- Index org_members by user_id to support ListOrgsForUser and
-- CountOrgMembershipsForUser. Without this, those queries seq-scan the
-- whole table — every magic-link verify, every /api/me, every org switch.
CREATE INDEX IF NOT EXISTS org_members_user_idx ON org_members (user_id);
