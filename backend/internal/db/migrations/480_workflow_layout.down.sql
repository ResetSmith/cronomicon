-- Reverse 480. Drop the advisory layout column. SQLite ALTER TABLE DROP COLUMN is
-- supported by mattn/go-sqlite3 (precedented in 280.down / 290.down / 470.down) and
-- is performed as an in-place drop — layout_json is unindexed and unreferenced, so
-- the workflows PK (source,name) and every other constraint survive.
-- TestMigrate480WorkflowLayoutRoundTrip exercises this with a laid-out row present.
ALTER TABLE workflows DROP COLUMN layout_json;
