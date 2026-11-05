package storage

import "io/fs"

func migrationsFS() fs.FS {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err) // the embedded directory is a compile-time constant
	}
	return sub
}
