-- 1510_activity_agency — an Activity row records the agency it is of (2.4.4;
-- the owner's wish that agencies see their own syncs, pushes and changes): a
-- sync's or a push's repository's agency, or the agency a change was made in.
-- NULL (Global's) for a row that names none, and for every row before.
ALTER TABLE activity ADD COLUMN agency_id TEXT;
