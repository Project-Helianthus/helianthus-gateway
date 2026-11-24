## Protocol boundary check with a debt ledger

The gateway CI now fails on any new protocol name, protocol import, register table or vendor identifier in the gateway. Each finding that existed before the check is recorded once as debt with the queue row that deletes it. Recorded debt can only be removed against the base revision of a change, never added, and one operator-approved exception admits the host constructor imports of the drivers and the Matter profile file (#1741).
