output "state_bucket" {
  description = "Put this into the `bucket` line of the backend block in ../main.tf."
  value       = aws_s3_bucket.state.id
}
