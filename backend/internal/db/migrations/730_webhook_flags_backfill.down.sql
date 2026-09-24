-- Down for 730. The up is a data back-fill, so there is nothing structural to
-- reverse, and it deliberately does NOT restore the zeros: on the way down the
-- flags stop being read again, and re-zeroing them would destroy an operator's
-- real choice (a deliberate "webhook off", or "push only") if they then migrated
-- back up. Leaving the values alone is the information-preserving direction.
SELECT 1;
