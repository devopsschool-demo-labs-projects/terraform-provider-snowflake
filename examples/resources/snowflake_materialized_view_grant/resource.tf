resource "snowflake_materialized_view_grant" "grant" {
  database_name          = "database"
  schema_name            = "schema"
  materialized_view_name = "materialized_view"

  privilege = "SELECT"
  roles = ["role1", "role2"]

  shares = ["share1", "share2"]

  on_future         = false
  with_grant_option = false
}
