// Package store is the sqlc-generated data layer. Do not edit the generated
// files or reorganize this package by hand — `sqlc generate` (config:
// sqlc.yaml at the repo root) rewrites it from db/queries/*.sql and
// db/migrations.
//
// Generated (one file per db/queries/*.sql, plus shared plumbing):
//
//	*.sql.go     typed methods for each named query
//	models.go    row structs for every table
//	querier.go   the Querier interface (emit_interface: true)
//	db.go        Queries struct + DBTX
//
// Hand-written:
//
//	pool.go             pgx pool construction + uuid helpers
//	isolation_test.go   cross-tenant isolation checks
//	doc.go              this file
//
// Changing the data layer: edit db/schema + db/queries, run
// `atlas migrate diff` and `sqlc generate` — never these .go files.
package store
