-- M8 Docker install path: the one-shot host-action admission also covers the
-- Docker resources image_cache and container (084 already journals them). Only
-- the kind list of the 086 table widens. Primary key, uniqueness, the resource
-- foreign key and every other column CHECK stay exactly as 086 created them.
--
-- The 086 kind CHECK is a column-level constraint, so MariaDB names it after the
-- column and ALTER TABLE ... DROP CONSTRAINT does not see it (error 1091).
-- Redefining the column replaces that constraint in one atomic statement: the
-- column definition below is 086's, apart from the widened kind list.
ALTER TABLE probe_host_action_admissions
    MODIFY kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (kind IN ('data_directory', 'unit_file', 'service', 'image_cache', 'container'));
