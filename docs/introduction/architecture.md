# Core Engine

The **Data Plane** (`apps/data-plane`) is the core SQLite execution engine that governs strict, isolated access to local SQLite instances.

## Engine Features

### Write-Ahead Logging (WAL)
Litebase ensures strict concurrency controls by bootstrapping every SQLite instance with `PRAGMA journal_mode(WAL)`. This allows readers to continue safely reading from the database while a write is occurring in the background.

### Write Serialization (Queueing)
To prevent the notorious SQLite `database is locked` constraints during high traffic, Litebase routes all database writes (`INSERT`, `UPDATE`, `DELETE`, `CREATE`, etc.) into an asynchronous Go channel queue. Only one transaction modifies the base file at a time, ensuring safety.

### Direct Reads
Wait states are heavily mitigated because `SELECT` statements bypass the write-queue entirely and leverage the direct `database/sql` connection pool, providing massive read capabilities.
