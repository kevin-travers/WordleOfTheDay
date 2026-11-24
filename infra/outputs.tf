output "lambda_function_name" {
  value = aws_lambda_function.wordle.function_name
}

output "webhook_parameter_name" {
  description = "Put the real Discord webhook URL into this SSM parameter."
  value       = aws_ssm_parameter.webhook_url.name
}

output "log_group_name" {
  value = aws_cloudwatch_log_group.lambda.name
}

output "schedule" {
  value = aws_cloudwatch_event_rule.daily.schedule_expression
}
