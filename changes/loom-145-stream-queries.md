### Fixed

- An open conversation stream no longer rereads the whole tasks table and the whole conversation every 500 ms (LOOM-145). Each poll reads that conversation's tasks (now indexed by conversation) and only the messages and jobs added since its last poll, plus jobs it last saw unfinished. A few open tabs on a long conversation no longer put steady load on the database.
