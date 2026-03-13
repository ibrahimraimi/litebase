# Tables API

Schema management API reference for retrieving structured DDL.

Presently, structural changes can be made by executing standard `CREATE TABLE`, `ALTER TABLE`, and `DROP TABLE` operations directly through the HTTP Execution API's `/query` endpoint.

```bash
curl -X POST -H 'Content-Type: application/json' \
  -d '{"query": "CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY, email TEXT);"}' \
  http://localhost:8081/query
```

Because these are **Write** operations, Litebase will automatically sequence them through the internal WriteQueue to ensure they do not cause `database is locked` conflicts with any concurrently active operations.
