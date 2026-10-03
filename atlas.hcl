env "local" {
  src = "file://db/schema/schema.sql"
  url = getenv("DSTREAM_DB_URL")
  dev = getenv("DSTREAM_ATLAS_DEV_URL")
  migration {
    dir    = "file://db/migrations"
    format = atlas

    // Pin the revision table to `public`, because that is where the binary
    // puts it: cmd/dstream/migrate.go applies these same migrations with
    // Atlas-as-a-library and hardcodes schema "public". The atlas CLI
    // otherwise defaults to a dedicated `atlas_schema_revisions` schema, so
    // the two runners tracked applied migrations in two different places —
    // the CLI would read a database the binary had fully migrated, find no
    // history, report every migration pending, and `make migrate-up` would
    // replay them onto a populated schema and abort on "already exists".
    //
    // Keep this in sync with cmd/dstream/migrate.go. Changing it here is
    // cheap; changing it there would need a data migration on every existing
    // deployment, since the binary is the production/self-host path.
    revisions_schema = "public"
  }
}
