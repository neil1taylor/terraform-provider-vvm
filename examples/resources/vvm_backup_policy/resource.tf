resource "vvm_backup_policy" "nightly" {
  name                        = "nightly"
  schedule_cron               = "0 2 * * *"
  retain_days                 = 14
  synthetic_full_cadence_days = 7
  skew_policy                 = "flag-and-keep"
  cos_bucket                  = var.cos_bucket
  cos_region                  = "eu-gb"
}
