env "local" {
  src = "file://db/schema/schema.sql"
  url = getenv("DSTREAM_DB_URL")
  dev = getenv("DSTREAM_ATLAS_DEV_URL")
  migration {
    dir    = "file://db/migrations"
    format = atlas
  }
}
