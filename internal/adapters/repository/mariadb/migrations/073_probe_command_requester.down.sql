CREATE TEMPORARY TABLE guard_probe_command_requester (n INTEGER CHECK (n = 0));
INSERT INTO guard_probe_command_requester SELECT COUNT(*) FROM probe_commands WHERE requested_by <> 0;
DROP TEMPORARY TABLE guard_probe_command_requester;
ALTER TABLE probe_commands DROP COLUMN requested_by;
