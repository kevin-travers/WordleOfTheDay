variable "region" {
  description = "AWS region to deploy into. The org's SCP only allows regional services in us-east-2."
  type        = string
  default     = "us-east-2"
}

variable "project" {
  description = "Name prefix for resources."
  type        = string
  default     = "wordle"
}

variable "schedule_expression" {
  description = "When to post the word of the day. EventBridge cron, always in UTC: cron(min hour day-of-month month day-of-week year)."
  type        = string
  default     = "cron(0 0 * * ? *)" # daily at midnight UTC, the moment the word changes
}

variable "log_retention_days" {
  description = "How long to keep Lambda logs in CloudWatch."
  type        = number
  default     = 7
}

variable "lambda_binary_path" {
  description = "Path to the compiled Go binary. It must be named 'bootstrap' for the provided.al2023 runtime."
  type        = string
  default     = "../build/bootstrap"
}

variable "lambda_memory_size" {
  description = "Lambda memory in MB (128-10240). CPU scales with memory."
  type        = number
  default     = 128

  validation {
    condition     = var.lambda_memory_size >= 128 && var.lambda_memory_size <= 10240 && floor(var.lambda_memory_size) == var.lambda_memory_size
    error_message = "lambda_memory_size must be a whole number of MB from 128 to 10240."
  }
}

variable "lambda_timeout" {
  description = "Lambda timeout in seconds (1-900)."
  type        = number
  default     = 15

  validation {
    condition     = var.lambda_timeout >= 1 && var.lambda_timeout <= 900 && floor(var.lambda_timeout) == var.lambda_timeout
    error_message = "lambda_timeout must be a whole number of seconds from 1 to 900."
  }
}
