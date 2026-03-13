# CLI Commands

The Litebase CLI is the primary interface for developers to interact with their local and remote databases.

## Commands

### `litebase run`
Starts a local Litebase server on the developer's machine, allowing them to test the control and data planes locally without cloud connectivity.

### `litebase init`
Initializes a new Litebase project in the current directory, generating configuration files.

### `litebase login`
Authenticates the user with the remote Litebase cloud instance and securely stores the API token.

### `litebase link`
Connects the current local project to an existing remote project database instance.

### `litebase deploy`
Pushes the local database schema and data to the linked remote instance.

### `litebase snapshot`
Creates a backup snapshot of the current database state, or restores an existing snapshot.
