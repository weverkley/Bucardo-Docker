package domain

// BucardoConfig represents the top-level structure of the bucardo.json file.
type BucardoConfig struct {
	Databases []Database `json:"databases"`
	Syncs     []Sync     `json:"syncs"`
	LogLevel  string     `json:"log_level,omitempty"`
}

// Database defines a PostgreSQL database connection for Bucardo.
type Database struct {
	ID     int    `json:"id"`
	DBName string `json:"dbname"`
	Host   string `json:"host"`
	User   string `json:"user"`
	Pass   string `json:"pass"`
	Port   *int   `json:"port,omitempty"`
}

// Sync defines a Bucardo synchronization task, detailing what to replicate from where to where.
type Sync struct {
	Name                  string `json:"name"`
	Sources               []int  `json:"sources,omitempty"`                  // A list of database IDs to use as sources.
	Targets               []int  `json:"targets,omitempty"`                  // A list of database IDs to use as targets.
	Bidirectional         []int  `json:"bidirectional,omitempty"`            // A list of database IDs for bidirectional (dbgroup) replication.
	Herd                  string `json:"herd,omitempty"`                     // The name of a herd (group) to sync all tables from the first source.
	Tables                string `json:"tables,omitempty"`                   // A comma-separated list of specific tables to sync.
	Onetimecopy           int    `json:"onetimecopy"`                        // Controls full-copy behavior (0=off, 1=always, 2=if target empty).
	StrictChecking        *bool  `json:"strict_checking,omitempty"`          // If false, allows schema differences like column order.
	ExitOnComplete        *bool  `json:"exit_on_complete,omitempty"`         // If true, the container will exit after this sync completes.
	ExitOnCompleteTimeout *int   `json:"exit_on_complete_timeout,omitempty"` // Timeout in seconds for run-once syncs.
	ConflictStrategy      string `json:"conflict_strategy,omitempty"`        // Defines how to resolve data conflicts (e.g., "bucardo_source").
}
