# Query API

The Core Engine HTTP API accepts raw SQL statements for straightforward integrations.

## Execution Endpoint

### `POST /query`
Executes an arbitrary SQL statement against the active instance.

#### Request payload

```json
{
  "query": "INSERT INTO users (email) VALUES (\"test@litebase.com\")"
}
```

#### Response payload
The response will automatically infer layout based on whether your query was a read or a write.

**For Writes:**
```json
{
  "rows_affected": 1
}
```

**For Reads:**
```json
{
  "columns": ["id", "email"],
  "rows": [
    [1, "test@litebase.com"]
  ]
}
```
