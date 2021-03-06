package migrations

import "github.com/concourse/atc/dbng/migration"

func CreatePipes(tx migration.LimitedTx) error {
	_, err := tx.Exec(`
		CREATE TABLE pipes (
			id text PRIMARY KEY,
			url text
		)
	`)
	return err
}
