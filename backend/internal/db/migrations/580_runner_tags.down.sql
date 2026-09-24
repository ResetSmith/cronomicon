-- Reverse 580. Drop the operator tags column (SQLite ALTER TABLE DROP COLUMN,
-- precedented in 330/510/511/512/520/530/540/550/560 down). Operator state;
-- nothing else references it, and register/redeclare do not depend on it.
ALTER TABLE runners DROP COLUMN tags;
