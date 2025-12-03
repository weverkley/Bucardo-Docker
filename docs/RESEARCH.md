# **Mechanisms of Asynchronous Replication: A Comprehensive Analysis of Bucardo’s Synchronization Architecture and PostgreSQL Integration**

## **1\. Executive Summary and Architectural Philosophy**

The landscape of database high availability and data distribution is dominated by two primary paradigms: physical replication, which operates at the storage or Write-Ahead Log (WAL) level, and logical replication, which operates at the level of database objects such as tables and rows. Bucardo represents a mature, sophisticated implementation of the latter, specifically designed for the PostgreSQL ecosystem.1 Unlike native streaming replication, which demands version homogeneity and replicates an entire cluster bit-for-bit, Bucardo functions as an external, asynchronous daemon written in Perl. It bridges the gap between disparate PostgreSQL versions—supporting migrations from versions as old as 8.4 to modern releases—and enables complex topologies such as multi-master (swap) and partial replication.1

The core utility of Bucardo lies in its decoupling from the database engine’s internal binary formats. By operating as a standard database client that leverages SQL primitives—triggers for capture, tables for queueing, and the COPY protocol for transport—it achieves a high degree of flexibility.4 This architectural choice, however, shifts the burden of consistency management, conflict resolution, and performance tuning from the database kernel to the application layer (the Bucardo daemon) and the database schema.5

This report provides an exhaustive technical analysis of Bucardo’s internal mechanics. We will dissect the process hierarchy that governs synchronization events, the schema instrumentation required to track data changes, the intricate logic of the synchronization loop, and the specific interactions with the core PostgreSQL database, including transaction isolation and signal handling.

## **2\. Process Topology and Signal Orchestration**

Bucardo does not operate as a single monolithic process but rather as a hierarchical fleet of processes managed by a central daemon. This design ensures fault isolation and allows the system to scale across multiple synchronization tasks simultaneously. The architecture is defined by three primary process types: the Master Control Program (MCP), the Controller (CTL), and the Kid (KID), supplemented by the Vacuum (VAC) process.6

### **2.1 The Master Control Program (MCP): The Orchestrator**

At the apex of the Bucardo hierarchy sits the Master Control Program (MCP). The MCP is responsible for the system's initialization, configuration loading, and the lifecycle management of all subordinate processes. It connects to the primary Bucardo database—a central repository of configuration state—to read the definition of databases, herds (groups of tables), and syncs (replication tasks).3

The MCP acts as the primary interface for external administrative commands. When a user issues a command via the bucardo Command Line Interface (CLI), such as bucardo kick \<syncname\>, the CLI does not communicate directly with the worker processes. Instead, it updates the database state or sends a PostgreSQL NOTIFY signal, which the MCP intercepts.6 The MCP is the only process that persists for the entire duration of the Bucardo service's uptime. It maintains a listening socket on the database to receive asynchronous signals, a mechanism that avoids the latency and resource overhead of polling.6

Upon startup, the MCP verifies the integrity of the configuration and forks a Controller (CTL) process for every active sync defined in the sync table. It then enters an event loop, monitoring the operating system's process table to detect if any CTL processes have terminated unexpectedly. If a CTL dies, the MCP logs the failure and attempts to respawn it, ensuring the resilience of the replication topology.5

### **2.2 The Controller (CTL): Sync-Level Management**

For each named sync, the MCP spawns a dedicated Controller process. The CTL is responsible for the scheduling and execution of that specific replication task. While the MCP manages the global state, the CTL focuses on the specific parameters of its assigned sync, such as the source databases, target databases, and timing requirements.7

The CTL operates in two primary modes relative to execution timing:

1. **Interval-Based:** The CTL sleeps for a configured checktime duration. Upon waking, it checks if a sync is required (e.g., if there are pending rows in the bucardo\_delta table).9  
2. **Signal-Based:** The CTL listens for specific PostgreSQL NOTIFY events, such as bucardo\_kick\_$syncname. This allows for near real-time replication where a trigger on the source table sends a notification immediately upon a data change, prompting the CTL to act without waiting for a timer.6

The primary function of the CTL is to manage the Kid (KID) process. When a sync needs to run, the CTL forks a KID. In Bucardo 5, the architecture evolved to a one-to-one relationship where a CTL typically manages a single active KID at a time for its sync, simplifying the logic compared to the multi-kid approach of Bucardo 4\.8 The CTL monitors the KID’s execution; if the KID exceeds a defined timeout or becomes unresponsive, the CTL is responsible for terminating it and reporting the error to the MCP.

### **2.3 The Kid (KID): The Replication Worker**

The KID process performs the actual heavy lifting of data replication. It is the KID that establishes connections to the source and target databases, initiates transactions, queries the delta logs, serializes the data, and streams it to the destinations.5

The KID’s lifecycle is configurable via the kidsalive parameter.

* **Ephemeral Mode (kidsalive=0):** The KID is forked by the CTL, performs a single synchronization run, and then exits. This releases database connections and memory but incurs the overhead of process creation and connection establishment for every sync run.  
* **Persistent Mode (kidsalive=1):** The KID remains active after a sync completes, waiting for a signal from the CTL to run again. This reduces latency and database load for high-frequency syncs.7

This isolation of the worker logic into the KID process is a critical stability feature. Since the KID interacts with external networks and processes potentially large and malformed datasets, it is the component most prone to failure (e.g., network partitioning, serialization errors, unique constraint violations). If a KID crashes, the CTL remains unaffected, allowing the system to recover gracefully on the next attempt.5

### **2.4 The Vacuum (VAC): Garbage Collection**

Bucardo’s reliance on a persistent table (bucardo\_delta) to track changes necessitates a garbage collection mechanism. The Vacuum (VAC) process runs independently of the syncs. Its sole responsibility is to prune rows from the bucardo\_delta and bucardo\_track tables that have been successfully replicated to all targets.7

Without the VAC process, the delta tables would grow monotonically. As these tables grow, the index scans required to identify changed rows would degrade in performance, eventually causing the synchronization process to stall—a phenomenon known as "delta bloat".10 The VAC process replaces the external cron jobs required in older versions of Bucardo, integrating this maintenance task directly into the daemon’s lifecycle.

### **2.5 Inter-Process Communication (IPC) Protocol**

Bucardo leverages PostgreSQL’s native LISTEN and NOTIFY system for IPC, effectively using the database as a message bus. This allows the various processes to communicate state changes and commands asynchronously. The following table summarizes the key signals exchanged:

Table 1: Bucardo Internal Signal Inventory 6

| Signal Name | Listener | Emitter | Purpose |
| :---- | :---- | :---- | :---- |
| bucardo\_mcp\_ping | MCP | External/CLI | Connectivity check to verify MCP is alive. |
| bucardo\_reload\_config | MCP | CLI | Instructs MCP to re-read the configuration table. |
| bucardo\_kick\_$syncname | MCP/CTL | Trigger/CLI | Signals that a specific sync should run immediately. |
| bucardo\_syncdone\_$syncname | CTL | KID | KID informs CTL that a sync cycle finished. |
| bucardo\_started | External | MCP | MCP broadcasts that it has successfully initialized. |
| bucardo\_kid\_$$\_ping | KID | CTL | Heartbeat check from Controller to Kid. |

## **3\. The Bucardo Database: Configuration and State Management**

Bucardo maintains a rigid separation between its configuration state and the data it replicates. The configuration is stored in a dedicated database, conventionally named bucardo. This database contains the schema definitions that drive the MCP's behavior.11

### **3.1 The Global Registry**

The bucardo database acts as the central registry for the replication cluster. It stores connection details, mapping internal identifiers to physical connection strings.

* **db Table:** Stores the connection parameters (host, port, user, password, database name) for every PostgreSQL instance participating in the replication. Each database is assigned a logical name (e.g., source\_db, target\_db) used throughout the configuration.13  
* **bucardo\_config Table:** Contains global settings that tune the daemon's behavior. Key parameters include log\_level (verbosity of logging), log\_timer\_format, and isolation\_level (the transaction isolation required for syncs).11

### **3.2 Topology Definitions: Goats and Herds**

Bucardo uses a metaphorical nomenclature to define replication sets.

* **Goat:** A "goat" represents a single replicable object, specifically a table or a sequence. The goat table stores the schema and table name of these objects.5  
* **Herd:** A "herd" (or relgroup) is a named collection of goats. Syncs are configured to act upon a herd, not individual tables. This grouping allows administrators to atomically manage sets of related tables (e.g., all tables belonging to the "Sales" module).5

### **3.3 The Sync Definition**

The sync table is the core operational unit. It links a herd to a set of databases and defines the replication rules. Crucially, it defines the type of synchronization:

* **Pushdelta:** Standard master-slave replication. Changes flow from source databases to target databases.  
* **Swap:** Multi-master replication. Changes can originate at any node and are propagated to all others, with conflict resolution logic applied.3

## **4\. Change Data Capture: The Trigger and Delta Mechanism**

Bucardo employs a trigger-based Change Data Capture (CDC) mechanism. Unlike WAL-based replication, which reads the binary transaction log, Bucardo requires the installation of a schema (also named bucardo) on every source and target database.11 This schema contains the instrumentation necessary to intercept writes and log them for processing.

### **4.1 The bucardo\_delta Table**

The heart of the capture mechanism is the bucardo\_delta table. Every table involved in a sync has a trigger attached to it (e.g., bucardo\_delta\_trigger). This trigger fires AFTER INSERT OR UPDATE OR DELETE.15

When the trigger fires, it executes a PL/pgSQL function that captures the Primary Key (PK) of the modified row. This PK is inserted into the bucardo\_delta table. The schema of bucardo\_delta is optimized for this specific purpose:

Table 2: Schema of bucardo\_delta 16

| Column Name | Data Type | Description |
| :---- | :---- | :---- |
| tablename | OID | The Object Identifier of the table where the change occurred. |
| rowid | TEXT | The Primary Key of the changed row, cast to text. |
| txntime | TIMESTAMPTZ | The timestamp of the transaction that committed the change. |

**Insight:** The use of TEXT for rowid allows Bucardo to support any primary key type (integer, UUID, string) in a single unified tracking table. However, this imposes a requirement: every replicated table *must* have a Primary Key or a unique index. Without a unique identifier, the triggers cannot log which specific row changed, and the KID cannot construct the target queries.17

### **4.2 The bucardo\_track Table**

In multi-master (swap) scenarios, simply tracking changes is insufficient. If Node A updates a row, that change is replicated to Node B. Node B's trigger will see this as a local write and log it to its own bucardo\_delta. Without a mechanism to distinguish "replicated writes" from "original writes," the system would enter an infinite replication loop (the "echo" problem).

Bucardo solves this with the bucardo\_track table. When a KID pushes a change to a target database, it simultaneously inserts a record into the target's bucardo\_track table. This record tells the system: "This row change came from an external source." The sync logic checks this table to filter out changes that it previously propagated, ensuring that only true, new local writes are replicated back to the cluster.11

### **4.3 Trigger Overhead and Performance**

The trigger-based approach introduces a performance penalty on the source database. Every write operation now involves an additional write to the bucardo\_delta table. In high-throughput environments, this can double the write load on the disk I/O subsystem. Furthermore, the bucardo\_delta table itself becomes a hotspot. If it is not aggressively vacuumed (by the VAC process), it can become bloated, slowing down the trigger execution and, by extension, the application's write latency.10

## **5\. The Synchronization Loop: Anatomy of a Sync**

The execution of a sync by a KID process follows a rigid, step-by-step algorithmic loop designed to ensure data consistency. This section details the sequence of operations from the moment a KID wakes up to the final commit.

### **5.1 Connection Establishment and Isolation**

Upon spawning, the KID establishes connections to the source and target databases using the Perl DBI module and the DBD::Pg driver.12 Immediately upon connecting, the KID sets the transaction isolation level.

In versions prior to PostgreSQL 9.1, Bucardo used SET TRANSACTION ISOLATION LEVEL SERIALIZABLE. However, starting with PostgreSQL 9.1, the semantics of SERIALIZABLE changed to a true Serializable Snapshot Isolation (SSI) level, which is more aggressive in detecting conflicts. Consequently, modern Bucardo versions typically default to REPEATABLE READ.

Why Repeatable Read?  
The KID must operate on a consistent snapshot of the data. If it used READ COMMITTED, a row queried at the start of the sync might change or disappear before the sync finishes, leading to data corruption or "phantom reads." REPEATABLE READ ensures that the KID sees a frozen view of the database as it existed at the start of the transaction.20

### **5.2 Phase 1: The Delta Gather**

The KID begins by querying the bucardo\_delta table on the source database(s). It retrieves a list of all distinct rowids (Primary Keys) associated with the table OIDs in the current sync.

* **Batching:** If the number of delta rows is massive (e.g., millions of rows), fetching them all into memory could exhaust the KID's resources. Bucardo can process these in batches, although the default behavior often attempts to process the entire set to ensure transactional atomicity for the batch.14  
* **Consolidation:** In a swap sync, the KID queries the delta tables of *all* participating databases. It merges these lists to identify the superset of all rows that have changed anywhere in the cluster.3

### **5.3 Phase 2: Conflict Detection and Resolution**

For a pushdelta (master-slave) sync, conflict detection is skipped; the source is authoritative. In a swap sync, the KID checks if the same rowid appears in the delta lists of multiple databases. If a row was modified on both Node A and Node B since the last sync, a conflict is declared. The KID then executes the configured conflict resolution strategy (detailed in Section 7\) to determine the "winning" version of the row.3

### **5.4 Phase 3: Data Transport (Delete and Copy)**

Once the list of rows to replicate is finalized, the KID must apply these changes to the target databases. Bucardo uses a "Delete-Then-Copy" strategy (or "Delete-Then-Insert") rather than using UPDATE statements.

1. **Deletion:** The KID issues a DELETE command on the target database for the specific list of Primary Keys identified in the delta. This removes the old versions of the rows.  
   * *Constraint Handling:* This step effectively handles both UPDATEs (by removing the old row) and DELETEs (by removing the row that was deleted on the source).  
2. **Copy:** The KID then streams the new data for these rows from the source into the target using the PostgreSQL COPY FROM STDIN command.1

The Performance Advantage of COPY:  
Bucardo leverages COPY because it is significantly faster than batched INSERT statements. COPY bypasses much of the SQL parsing and planning overhead for individual statements, streaming raw data directly into the table relations. Benchmarks suggest COPY can be an order of magnitude faster for bulk loading.23 This architectural choice allows Bucardo to synchronize thousands of changes per second, provided the network bandwidth is sufficient.

### **5.5 Phase 4: Track Update and Commit**

After the data is successfully copied, the KID inserts records into the bucardo\_track table on the target databases. This marks the changes as "replicated" so that the target's triggers do not re-capture them. Finally, the KID issues a COMMIT on all database connections.

The "2PC" Limitation:  
Bucardo does not use a strict Two-Phase Commit (2PC) protocol (XA transactions) to guarantee that all databases commit simultaneously. It issues commits sequentially. If the commit succeeds on Node A but the network fails before the commit reaches Node B, the cluster enters an inconsistent state. Bucardo relies on the next sync cycle to retry and repair this divergence, adhering to an "Eventual Consistency" model rather than "Strong Consistency".25

## **6\. Data Transport and Transformation: COPY vs INSERT**

The choice of data transport mechanism is a defining characteristic of Bucardo's interaction with PostgreSQL. While many ETL tools rely on INSERT statements, Bucardo’s extensive use of COPY reflects its optimization for PostgreSQL internals.

### **6.1 The Mechanics of COPY**

The COPY command in PostgreSQL is designed for bulk data transfer. When the KID retrieves the "winning" rows from the source, it reads them into Perl memory structures. It then formats this data into a COPY-compatible stream (typically delimiter-separated values with specific escaping for NULLs and special characters).14

The KID then opens a stream to the target database:

Perl

$dbh-\>do("COPY target\_table (col1, col2) FROM STDIN");  
$dbh-\>pg\_putcopydata($row\_data);  
$dbh-\>pg\_putcopyend();

This method fills the TCP send/receive buffers efficiently and minimizes the context switching between the database process and the operating system kernel, which is a common bottleneck in row-by-row INSERT operations.23

### **6.2 Handling Large Objects and TOAST**

PostgreSQL stores large field values (like long text or binary BYTEA data) in separate storage areas known as TOAST tables. When Bucardo replicates a row, it must fetch the full content of these columns. Since bucardo\_delta only stores the Primary Key, the KID performs a SELECT \* for the changed rows.

For tables with massive BYTEA columns, this can create significant memory pressure on the KID process and network saturation. Bucardo handles this by ensuring that the COPY stream correctly encodes binary data (often using hex encoding or escape sequences depending on the Postgres version) to prevent corruption during transport.28

### **6.3 The Onetimecopy Feature**

In scenarios where the delta tables are missing or the target is completely empty (e.g., adding a new node to the cluster), the delta logic is inefficient. Bucardo provides the onetimecopy configuration option. When enabled (onetimecopy=1), the KID bypasses the delta check entirely. It truncates the target table and initiates a full table COPY from the source. This is the fastest method for initial provisioning or "zero-downtime" migration, as it avoids the read-overhead of checking deltas.29

## **7\. Multi-Master Conflict Resolution Algorithms**

One of Bucardo’s most distinct features is its support for multi-master replication with configurable conflict resolution. Unlike physical replication, which enforces a single writer (primary), Bucardo allows writes to occur on multiple nodes simultaneously, creating the potential for data collision.

### **7.1 Collision Detection**

Collision detection relies on the bucardo\_delta tables. If the KID finds the same Primary Key K in the delta table of Database A and Database B during the same sync interval, a conflict is raised.

### **7.2 Built-in Resolution Strategies**

Bucardo offers several pre-defined strategies configured via the conflict\_strategy parameter 22:

* **bucardo\_latest:** This is the default strategy. The KID compares the txntime of the conflicting rows in bucardo\_delta. The row with the most recent timestamp is deemed the winner and overwrites the other. This strategy requires that the system clocks on all server nodes be synchronized via NTP to be effective.  
* **bucardo\_latest\_all\_tables:** Similar to bucardo\_latest, but it checks the timestamps across all tables in the sync to ensure referential integrity if related tables were updated.  
* **bucardo\_abort:** The sync is immediately halted. This is a safety mechanism for environments where data loss is unacceptable and manual intervention is preferred.  
* **bucardo\_source / bucardo\_target:** A static priority rule where one database is always authoritative.

### **7.3 Custom Conflict Handlers (customcode)**

For complex business requirements, Bucardo allows the injection of Perl code snippets, known as customcode, into the conflict resolution pipeline. These codes are stored in the customcode table and mapped to specific syncs.

When a conflict occurs, the KID invokes the Perl subroutine, passing a hash reference containing context about the conflict:

Table 3: Custom Conflict Handler Hash Keys 22

| Key | Description |
| :---- | :---- |
| tablename | The name of the table experiencing the conflict. |
| conflicts | A hash of the conflicting Primary Keys and their source databases. |
| dbinfo | Information about the databases involved. |
| dbh | Active DBI database handles (safe versions) to the databases. |

The custom code can manipulate this hash to instruct the KID on how to proceed. It can set:

* **tablewinner:** Specifies which database's version should be used for the current table.  
* **syncwinner:** Specifies a winner for the entire sync operation.  
* **tablewinner\_always:** Caches the decision, so subsequent conflicts on the same table in this run are resolved automatically without re-invoking the code.22

This extensibility allows for logic such as "additive conflicts" (e.g., if both sides added $10 to a balance, the resolved value should be \+$20, not just the latest \+$10), although implementing such logic requires careful coding within the Perl hook to query the values and calculate the result.

## **8\. PostgreSQL Interaction and Transaction Isolation**

Bucardo’s reliability depends heavily on its interaction with the core PostgreSQL transaction manager. It must ensure that the data it reads is consistent and that its own bookkeeping (deltas and tracks) is atomic with the data replication.

### **8.1 Transaction Isolation Levels**

The choice of transaction isolation level is pivotal. As noted in Section 5.1, Bucardo typically uses REPEATABLE READ. This protects against the following anomalies during the sync process:

* **Dirty Reads:** Reading uncommitted data (prevented by Postgres defaults).  
* **Non-Repeatable Reads:** A row retrieved once changes if retrieved again in the same transaction.  
* **Phantom Reads:** New rows satisfying a query condition appear during the transaction.

By using REPEATABLE READ, Bucardo effectively "freezes" the database state for the KID process. However, this has a side effect: if a sync takes a long time to complete (e.g., due to a slow network or massive data volume), it holds an open transaction on the source database. In PostgreSQL, open transactions prevent the vacuum daemon from cleaning up dead tuples (old versions of rows) that were deleted or updated after the transaction started. This can lead to **table bloat** on the source database if syncs hang or run excessively long.20

### **8.2 Sequence Management**

PostgreSQL sequences are non-transactional counters. Bucardo does not replicate sequences in the same way it replicates tables because sequences do not generate delta rows when incremented.

In a multi-master setup, if two nodes both insert a new row using a local sequence to generate the ID, they might both generate ID 500\. When Bucardo attempts to swap these rows, a Unique Key Violation will occur.

Bucardo does not automatically manage this. Administrators are expected to configure **staggered sequences** (Interleaved IDs) on the database nodes manually.32

* Node A: START WITH 1 INCREMENT BY 2 (Generates 1, 3, 5...)  
* Node B: START WITH 2 INCREMENT BY 2 (Generates 2, 4, 6...)  
  This configuration ensures that keys never collide, allowing Bucardo to replicate rows freely without primary key conflicts.

### **8.3 Interaction with session\_replication\_role**

When Bucardo applies changes to the target database, it often needs to bypass the triggers that are normally active (specifically, the bucardo\_delta triggers, to prevent the "echo" problem mentioned in Section 4.2).

Bucardo achieves this by manipulating the session\_replication\_role setting in PostgreSQL. Before applying changes, the KID may set this variable to replica. Triggers in PostgreSQL can be configured to fire ALWAYS, NEVER, or REPLICA. By default, standard user triggers do not fire when the role is replica. This ensures that the incoming data is applied without side effects and without re-triggering capture logic, although Bucardo often manages its own triggers explicitly by disabling/enabling them if session\_replication\_role is not sufficient or if finer-grained control is needed.33

## **9\. Performance Engineering and Optimization**

Performance in Bucardo is a function of delta size, network latency, and the overhead of the Perl interpreter. Several mechanisms exist to tune this performance.

### **9.1 Mitigating Delta Bloat**

The performance of the "Delta Gather" phase is directly related to the size of the bucardo\_delta table. If the VAC process falls behind, or if a specific target database is offline for days, millions of rows can accumulate. The query SELECT DISTINCT rowid FROM bucardo\_delta then becomes a slow sequential scan or an expensive index scan.

To mitigate this, Bucardo allows setting bucardo\_vacuum\_delta parameters. Additionally, splitting syncs into smaller "herds" can prevent one slow table from blocking the cleanup of deltas for other faster tables.

### **9.2 The kidsalive Trade-off**

As discussed, kidsalive controls process persistence.

* **Scenario for kidsalive=0:** Low-memory environments or syncs that run hourly. The overhead of starting a Perl interpreter and connecting to Postgres (\~50-100ms) is negligible compared to the 1-hour interval.  
* **Scenario for kidsalive=1:** High-frequency syncs (e.g., every 5 seconds). The connection overhead would consume a significant percentage of the sync time. Keeping the KID alive maintains the database connection pool, allowing for near-instant response to "kick" signals.

### **9.3 Batching and Memory Management**

Bucardo handles memory management in Perl. For massive syncs, loading all delta rows into a Perl hash can cause the process to hit system RAM limits (OOM Killer). Bucardo includes logic to chunk these operations, reading a subset of deltas, processing them, and then reading the next, although this logic is complex and relies on the configuration of statement\_chunk\_size limits in the underlying DBI driver or specific Bucardo batching configurations.14

## **10\. Operational Command and Control: CLI Internals**

The bucardo command-line tool is the primary administrative interface. It is a Perl script that translates user intent into database updates and NOTIFY signals.

### **10.1 Key Commands**

* **install:** Bootstraps the system. It connects to the target Postgres instance, creates the bucardo role, database, and schema. It heavily relies on the user having superuser privileges during this phase.12  
* **add database / add table / add sync:** These commands populate the configuration tables. They perform validation, checking if the tables exist and have valid primary keys.13  
* **kick \<syncname\>:** As detailed in Section 2, this sends a NOTIFY signal. It creates an immediate demand for a sync run.  
* **status:** Queries the bucardo\_track and bucardo\_rate tables to display the last run time, number of rows processed, and current status of syncs.  
* **validate:** Forces a check of the configuration against the actual database schemas. This is crucial if a schema change (DDL) occurred on a source table, as Bucardo might still be holding cached OIDs or column definitions that are now invalid.5

### **10.2 Logging and Debugging**

Bucardo writes extensive logs, typically to a file in /var/log/bucardo/log.bucardo or to syslog. The verbosity is controlled by log\_level.

* **TERSE:** Minimal output (start/stop/error).  
* **NORMAL:** Standard operational logs.  
* **VERBOSE / DEBUG:** Outputs detailed SQL statements generated by the KID, including the exact COPY commands and delta queries. This is essential for troubleshooting "stuck" syncs or understanding why a specific row conflict was resolved in a certain way.14

## **11\. Fault Tolerance and Reliability Engineering**

Bucardo is designed to survive the inherent instability of distributed systems, but it has specific failure modes that operators must understand.

### **11.1 Network Partitioning**

If the network link between the MCP/KID and a target database goes down:

1. The KID will attempt to connect and fail (timeout).  
2. The KID will exit (or retry depending on configuration).  
3. The delta rows for that target will *remain* in the bucardo\_delta table on the source.  
4. The VAC process will *not* remove these rows because they have not been confirmed as replicated.  
5. Once the network returns, the next KID run will pick up all the accumulated deltas and replicate them.

**Risk:** If the outage is prolonged, the bucardo\_delta table can grow large enough to impact the performance of the source application (due to trigger write overhead and index maintenance).

### **11.2 The "Split-Brain" Risk**

In a multi-master setup, if the network is severed but both sides remain active and accept writes, the databases will diverge. When the network returns, Bucardo will detect massive conflicts. While the resolution strategies (bucardo\_latest) will eventually converge the data, there is a risk of logical data loss if business rules were violated during the split (e.g., the same seat on a flight sold on both nodes). Bucardo resolves the data conflict but cannot inherently resolve the business logic violation; this relies on the customcode hooks or application-level constraints.22

## **12\. Historical Evolution and Version Disparities**

The transition from Bucardo 4 to Bucardo 5 introduced significant architectural shifts that are relevant for understanding legacy deployments versus modern best practices.

* **Multi-Source Support:** Bucardo 4 was primarily limited to two-source master-master. Bucardo 5 introduced true N-way multi-master support, allowing for star and mesh topologies.8  
* **Process Model:** Bucardo 4 used a "one kid per target" model, which could spawn dozens of processes for a single sync with many targets. Bucardo 5 streamlined this to a "one kid per sync" model (mostly), reducing the process table overhead.8  
* **PostgreSQL Version Support:** Newer versions of Bucardo have had to adapt to changes in PostgreSQL, such as the removal of implicit casts and the renaming of XLOG functions. The shift from SERIALIZABLE to REPEATABLE READ in configuration defaults (post-Postgres 9.1) is a prime example of this evolution.20

## **13\. Conclusion**

Bucardo occupies a vital niche in the PostgreSQL ecosystem. It offers a replication solution that is less invasive than physical replication (requiring no kernel-level changes or WAL shipping) yet more flexible than native logical replication (offering multi-master conflict resolution and version bridging). Its architecture is a study in the trade-offs of distributed systems: it sacrifices the raw speed and strict atomicity of synchronous replication for the resilience and flexibility of asynchronous, trigger-based synchronization.

For the database administrator, successful operation of Bucardo requires a shift in perspective. One must not only monitor the database metrics but also the health of the bucardo\_delta tables, the status of the Perl daemon, and the synchronization of system clocks. When properly tuned—leveraging COPY for transport, kidsalive for latency, and customcode for logic—Bucardo provides a robust mechanism for keeping disparate PostgreSQL islands in sync across a chaotic network sea.

#### **Works cited**

1. Bucardo \- AWS Prescriptive Guidance, accessed December 2, 2025, [https://docs.aws.amazon.com/prescriptive-guidance/latest/migration-databases-postgresql-ec2/bucardo-considerations.html](https://docs.aws.amazon.com/prescriptive-guidance/latest/migration-databases-postgresql-ec2/bucardo-considerations.html)  
2. Bucardo \- PostgreSQL wiki, accessed December 2, 2025, [https://wiki.postgresql.org/wiki/Bucardo](https://wiki.postgresql.org/wiki/Bucardo)  
3. Bucardo Overview, accessed December 2, 2025, [https://bucardo.org/Bucardo/Overview](https://bucardo.org/Bucardo/Overview)  
4. PG Phriday: Replication Engine Potpourri \- EDB, accessed December 2, 2025, [https://www.enterprisedb.com/blog/pg-phriday-replication-engine-potpourri](https://www.enterprisedb.com/blog/pg-phriday-replication-engine-potpourri)  
5. Choosing a Logical Replication System \- Bucardo, accessed December 2, 2025, [https://bucardo.org/Bucardo/presentations/2015-Choosing-Logical-Replication.pdf](https://bucardo.org/Bucardo/presentations/2015-Choosing-Logical-Replication.pdf)  
6. Bucardo listen, accessed December 2, 2025, [https://bucardo.org/Bucardo/internals/listen\_notify](https://bucardo.org/Bucardo/internals/listen_notify)  
7. Bucardo Documentation Glossary, accessed December 2, 2025, [https://bucardo.org/Bucardo/glossary](https://bucardo.org/Bucardo/glossary)  
8. \[Bucardo-general\] Newbie Questions, accessed December 2, 2025, [https://bucardo.org/pipermail/bucardo-general/2012-September/001525.html](https://bucardo.org/pipermail/bucardo-general/2012-September/001525.html)  
9. bucardo — Debian bullseye, accessed December 2, 2025, [https://manpages.debian.org/bullseye/bucardo/bucardo.1p](https://manpages.debian.org/bullseye/bucardo/bucardo.1p)  
10. Delta table keeps growing · Issue \#68 \- GitHub, accessed December 2, 2025, [https://github.com/bucardo/bucardo/issues/68](https://github.com/bucardo/bucardo/issues/68)  
11. Bucardo schema, accessed December 2, 2025, [https://bucardo.org/Bucardo/schema/](https://bucardo.org/Bucardo/schema/)  
12. Migrating legacy PostgreSQL databases to Amazon RDS or Aurora PostgreSQL using Bucardo \- AWS, accessed December 2, 2025, [https://aws.amazon.com/blogs/database/migrating-legacy-postgresql-databases-to-amazon-rds-or-aurora-postgresql-using-bucardo/](https://aws.amazon.com/blogs/database/migrating-legacy-postgresql-databases-to-amazon-rds-or-aurora-postgresql-using-bucardo/)  
13. Bucardo Tutorial, accessed December 2, 2025, [https://bucardo.org/Bucardo/pgbench\_example](https://bucardo.org/Bucardo/pgbench_example)  
14. bucardo/Changes at master \- GitHub, accessed December 2, 2025, [https://github.com/bucardo/bucardo/blob/master/Changes](https://github.com/bucardo/bucardo/blob/master/Changes)  
15. \[Bucardo-general\] removing a sync and triggers \- help, accessed December 2, 2025, [https://bucardo.org/pipermail/bucardo-general/2013-September/002032.html](https://bucardo.org/pipermail/bucardo-general/2013-September/002032.html)  
16. Table: bucardo.bucardo\_delta, accessed December 2, 2025, [https://bucardo.org/Bucardo/schema/bucardo\_delta](https://bucardo.org/Bucardo/schema/bucardo_delta)  
17. Pulling off zero-downtime PostgreSQL migrations with Bucardo and Terraform \- Medium, accessed December 2, 2025, [https://medium.com/hellogetsafe/pulling-off-zero-downtime-postgresql-migrations-with-bucardo-and-terraform-1527cca5f989](https://medium.com/hellogetsafe/pulling-off-zero-downtime-postgresql-migrations-with-bucardo-and-terraform-1527cca5f989)  
18. The design of Bucardo version 1 \- End Point Dev, accessed December 2, 2025, [https://www.endpointdev.com/blog/2009/05/design-of-bucardo-version-1/](https://www.endpointdev.com/blog/2009/05/design-of-bucardo-version-1/)  
19. Bucardo replication workarounds for extremely large Postgres updates \- End Point Dev, accessed December 2, 2025, [https://www.endpointdev.com/blog/2016/05/bucardo-replication-workarounds-for/](https://www.endpointdev.com/blog/2016/05/bucardo-replication-workarounds-for/)  
20. PostgreSQL Serializable and Repeatable Read Switcheroo \- End Point Dev, accessed December 2, 2025, [https://www.endpointdev.com/blog/2011/09/postgresql-allows-for-different/](https://www.endpointdev.com/blog/2011/09/postgresql-allows-for-different/)  
21. Documentation: 6.5: Serializable Isolation Level \- PostgreSQL, accessed December 2, 2025, [https://www.postgresql.org/docs/6.5/mvcc3123.htm](https://www.postgresql.org/docs/6.5/mvcc3123.htm)  
22. Bucardo Conflict Handling, accessed December 2, 2025, [https://bucardo.org/Bucardo/operations/conflict\_handling](https://bucardo.org/Bucardo/operations/conflict_handling)  
23. How does COPY work and why is it so much faster than INSERT? \- Stack Overflow, accessed December 2, 2025, [https://stackoverflow.com/questions/46715354/how-does-copy-work-and-why-is-it-so-much-faster-than-insert](https://stackoverflow.com/questions/46715354/how-does-copy-work-and-why-is-it-so-much-faster-than-insert)  
24. PostgreSQL: COPY vs Batch Insert — Which One Should You Use? | by Vishal Priyadarshi, accessed December 2, 2025, [https://medium.com/@vishalpriyadarshi/postgresql-copy-vs-batch-insert-which-one-should-you-use-c96bc36c2765](https://medium.com/@vishalpriyadarshi/postgresql-copy-vs-batch-insert-which-one-should-you-use-c96bc36c2765)  
25. Two-phase commit protocol \- Wikipedia, accessed December 2, 2025, [https://en.wikipedia.org/wiki/Two-phase\_commit\_protocol](https://en.wikipedia.org/wiki/Two-phase_commit_protocol)  
26. Parallel Commits: An atomic commit protocol for globally distributed transactions, accessed December 2, 2025, [https://www.cockroachlabs.com/blog/parallel-commits/](https://www.cockroachlabs.com/blog/parallel-commits/)  
27. Solved: Copy activity fails to save data as delta table bu... \- Microsoft Fabric Community, accessed December 2, 2025, [https://community.fabric.microsoft.com/t5/Data-Engineering/Copy-activity-fails-to-save-data-as-delta-table-but-works-if/td-p/4048506](https://community.fabric.microsoft.com/t5/Data-Engineering/Copy-activity-fails-to-save-data-as-delta-table-but-works-if/td-p/4048506)  
28. Bucardo Changes, accessed December 2, 2025, [https://bucardo.org/Bucardo/Changes/](https://bucardo.org/Bucardo/Changes/)  
29. How we used Bucardo for a zero-downtime PostgreSQL migration · Smartcar blog, accessed December 2, 2025, [https://smartcar.com/blog/zero-downtime-migration](https://smartcar.com/blog/zero-downtime-migration)  
30. Onetimecopy \- Bucardo, accessed December 2, 2025, [https://bucardo.org/Bucardo/operations/onetimecopy](https://bucardo.org/Bucardo/operations/onetimecopy)  
31. Zero downtime Postgres migration, done right \- Blueground: Engineering blog, accessed December 2, 2025, [https://engineering.theblueground.com/zero-downtime-postgres-migration-done-right/](https://engineering.theblueground.com/zero-downtime-postgres-migration-done-right/)  
32. How to handle sequences in Bucardo Postgresql multi master \- Stack Overflow, accessed December 2, 2025, [https://stackoverflow.com/questions/30752829/how-to-handle-sequences-in-bucardo-postgresql-multi-master](https://stackoverflow.com/questions/30752829/how-to-handle-sequences-in-bucardo-postgresql-multi-master)  
33. Planet PostgreSQL \- RSSing.com, accessed December 2, 2025, [https://hemming-in.rssing.com/chan-2212310/all\_p198.html](https://hemming-in.rssing.com/chan-2212310/all_p198.html)  
34. utility script for controlling the Bucardo program \- Ubuntu Manpage, accessed December 2, 2025, [https://manpages.ubuntu.com/manpages/focal/man1/bucardo.1p.html](https://manpages.ubuntu.com/manpages/focal/man1/bucardo.1p.html)  
35. Documentation \- Bucardo, accessed December 2, 2025, [https://bucardo.org/Bucardo/](https://bucardo.org/Bucardo/)  
36. Version 5 of Bucardo database replication system | End Point Dev, accessed December 2, 2025, [https://www.endpointdev.com/blog/2014/06/bucardo-5-multimaster-postgres-released/](https://www.endpointdev.com/blog/2014/06/bucardo-5-multimaster-postgres-released/)