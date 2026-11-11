package db

import "io/fs"

// MigrationFilesForTest exposes the embedded migrations to the db_test package.
func MigrationFilesForTest() fs.FS { return migrationFiles }
