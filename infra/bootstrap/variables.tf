variable "region" {
  description = "AWS region for the state bucket. Must match `region` in the backend block of ../main.tf."
  type        = string
  default     = "us-east-2"
}

variable "project" {
  description = "Name prefix for resources."
  type        = string
  default     = "wordle"
}

variable "owner" {
  description = "Value for the Owner tag on every resource."
  type        = string
  default     = "Kevin Travers"
}
