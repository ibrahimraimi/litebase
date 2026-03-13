# Create a Database

How to provision a new SQLite database within your project.

In the Litebase architecture, databases are provisioned automatically by the **Data Plane**. 

When you start the local engine:
```bash
litebase run
```
The engine will look for the designated database file (e.g. `main.db`) and create it automatically if it does not yet exist, bootstrapping it with `journal_mode=WAL` for high concurrency.
