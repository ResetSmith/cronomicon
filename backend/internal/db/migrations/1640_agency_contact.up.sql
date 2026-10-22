-- 1640_agency_contact (2.5.2 OP1.1, OP-28).
--
-- Who the central team calls about an agency's runs when the job or workflow
-- names no contact of its own: free text, like an annotation's contact (an
-- on-call rotation or a mailing list, rarely a Cronomicon user, so no foreign
-- key). Empty is none. Display-only: it routes nothing and pages nobody.
-- Set by the agency's administrators or a global administrator
-- (PUT /agencies/{id}/contact).
ALTER TABLE agencies ADD COLUMN contact TEXT NOT NULL DEFAULT '';
