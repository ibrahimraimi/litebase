# Run Your First Query

Once the data-plane engine is running locally (e.g. on port `8081`), you can interact with the engine immediately using standard HTTP requests.

Here is an example using `curl` to create a table and insert a row:

```bash
curl -X POST -H 'Content-Type: application/json' \
  -d '{"query": "CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY, email TEXT);"}' \
  http://localhost:8081/query

curl -X POST -H 'Content-Type: application/json' \
  -d '{"query": "INSERT INTO users (email) VALUES (\"test-user@example.com\");"}' \
  http://localhost:8081/query
```

You can then fetch the results:

```bash
curl -X POST -H 'Content-Type: application/json' \
  -d '{"query": "SELECT * FROM users;"}' \
  http://localhost:8081/query
```
