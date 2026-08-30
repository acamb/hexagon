-- The host interface a session's published ports are bound to.
--
-- Empty means the loopback interface, which is what every session created
-- before this column had and the only thing Hexagon could publish on. A session
-- on a remote machine needs the ports reachable from somewhere other than that
-- machine, so the address is now a choice — and, like the ports themselves, one
-- fixed when the container is created.
ALTER TABLE sessions ADD COLUMN port_address TEXT NOT NULL DEFAULT '';
