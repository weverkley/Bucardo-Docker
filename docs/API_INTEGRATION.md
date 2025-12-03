# API Integration Guide

This document describes the REST API available on port `8080` of the Bucardo Docker container. This API allows you to dynamically read, modify, and apply the replication configuration without manually editing the `bucardo.json` file inside the container.

**Base URL:** `http://localhost:8080` (or your container's IP)

## Core Concepts

The API operates directly on the underlying `bucardo.json` configuration file.
- **Modifying Syncs:** When you create, update, or delete a sync via the API, the change is written to the configuration file immediately.
- **Applying Changes:** Changes to the configuration do **not** take effect in the running Bucardo process immediately. You must call the `/restart` endpoint to reload the configuration and reconcile the Bucardo state (e.g., creating/removing syncs in the database).

## Endpoints

### 1. Sync Management

Manage individual replication tasks (Syncs).

#### List All Syncs
Retrieves the list of all configured syncs.

*   **Method:** `GET`
*   **URL:** `/syncs`
*   **Response:** `200 OK` (JSON Array of Sync Objects)

#### Get Sync Details
Retrieves configuration for a specific sync.

*   **Method:** `GET`
*   **URL:** `/syncs/{name}`
*   **Response:** `200 OK` (Sync Object) or `404 Not Found`

#### Create New Sync
Adds a new sync to the configuration.

*   **Method:** `POST`
*   **URL:** `/syncs`
*   **Body:** JSON Sync Object
    ```json
    {
      "name": "my_new_sync",
      "sources": [1],
      "targets": [2],
      "tables": "public.users, public.orders",
      "onetimecopy": 2,
      "conflict_strategy": "bucardo_source"
    }
    ```
*   **Response:** `201 Created`

#### Update Sync
Updates an existing sync. Note that changing the table list will cause a destructive re-creation of the sync upon restart.

*   **Method:** `PUT`
*   **URL:** `/syncs/{name}`
*   **Body:** JSON Sync Object
    ```json
    {
      "name": "my_new_sync",
      "sources": [1],
      "targets": [2],
      "tables": "public.users, public.orders, public.products",
      "onetimecopy": 2,
      "conflict_strategy": "bucardo_latest"
    }
    ```
*   **Response:** `200 OK`

#### Delete Sync
Removes a sync from the configuration.

*   **Method:** `DELETE`
*   **URL:** `/syncs/{name}`
*   **Response:** `200 OK`

### 2. Full Configuration

Manage the entire configuration file at once.

#### Get Full Config
Retrieves the complete `bucardo.json` content, including databases and global settings.

*   **Method:** `GET`
*   **URL:** `/config`
*   **Response:** `200 OK` (Full Configuration Object)

#### Update Full Config
Replaces the entire `bucardo.json` content.

*   **Method:** `POST`
*   **URL:** `/config`
*   **Body:** Full Configuration Object
*   **Response:** `200 OK`

### 3. Lifecycle Management

Control the application state.

#### Restart and Apply Changes (Important)
Reloads the configuration from disk, reconciles the Bucardo state (adding/removing syncs as needed), and restarts the Bucardo daemon. **Call this after modifying syncs.**

*   **Method:** `POST`
*   **URL:** `/restart`
*   **Response:** `200 OK` ("Application reloaded and restarted")

#### Start Bucardo
Starts the Bucardo daemon if it is stopped.

*   **Method:** `POST`
*   **URL:** `/start`
*   **Response:** `200 OK`

#### Stop Bucardo
Stops the Bucardo daemon.

*   **Method:** `POST`
*   **URL:** `/stop`
*   **Response:** `200 OK`

### 4. Real-time Logging

Stream application and Bucardo replication logs in real-time via WebSocket.

#### Log Stream
*   **URL:** `ws://localhost:8080/logs`
*   **Protocol:** WebSocket
*   **Message Format:** JSON string representing a log entry.

**Example Log Message:**
```json
{
  "time": "2023-10-27T10:00:00.123Z",
  "level": "INFO",
  "msg": "(72) [Wed Dec 3 11:07:23 2025] KID (aarca_users_sync) Rows copied to (postgres) db2.public.\"Users\": 2",
  "component": "bucardo_log"
}
```

**Integration:**
Any WebSocket client can connect to this endpoint. The server sends log messages as soon as they are generated.

```javascript
const socket = new WebSocket('ws://localhost:8080/logs');

socket.onmessage = function(event) {
  const logEntry = JSON.parse(event.data);
  console.log(`[${logEntry.level}] ${logEntry.msg}`);
};
```

---

## Data Models

### Sync Object

| Field | Type | Description |
| :--- | :--- | :--- |
| `name` | string | **Required.** Unique identifier for the sync. |
| `sources` | array[int] | Database IDs to act as sources. |
| `targets` | array[int] | Database IDs to act as targets. |
| `tables` | string | Comma-separated list of tables (e.g., `"public.table1, public.table2"`). |
| `herd` | string | Name of an existing herd (alternative to `tables`). |
| `onetimecopy` | int | `0`=off, `1`=always, `2`=empty targets only. |
| `strict_checking` | bool | Enforce schema matching. Default: `true`. |
| `conflict_strategy` | string | E.g., `"bucardo_source"`, `"bucardo_target"`. |
| `exit_on_complete` | bool | Run once and exit (for batch jobs). |

---

## Integration Workflow Example

To programmatically add a new table to replication:

1.  **Create the Sync definition:**
    POST the new sync details to `/syncs`.
    ```bash
    curl -X POST http://localhost:8080/syncs \
      -H "Content-Type: application/json" \
      -d '{"name":"sales_sync", "sources":[1], "targets":[2], "tables":"sales.orders", "onetimecopy":2}'
    ```

2.  **Apply the Changes:**
    Trigger a restart to let the container configure Bucardo.
    ```bash
    curl -X POST http://localhost:8080/restart
    ```

3.  **Verify:**
    Check if the sync exists.
    ```bash
    curl http://localhost:8080/syncs/sales_sync
    ```
