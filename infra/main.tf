terraform {
  required_version = ">= 1.11" # write-only arguments (value_wo) need 1.11+

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
    archive = {
      source  = "hashicorp/archive"
      version = "~> 2.7"
    }
  }

  # State lives in the bucket created by infra/bootstrap (its state_bucket output).
  # Backend blocks can't use variables, so keep `region` in sync with var.region by hand.
  backend "s3" {
    bucket       = "wordle-tfstate-345265465845"
    key          = "wordle/terraform.tfstate"
    region       = "us-east-2"
    encrypt      = true
    use_lockfile = true # locks with wordle/terraform.tfstate.tflock in the same bucket
  }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project   = var.project
      ManagedBy = "terraform"
      Owner     = "Kevin Travers"
    }
  }
}

locals {
  function_name = "${var.project}-of-the-day"
}

# ---------------------------------------------------------------------------
# SSM parameter: Discord webhook URL
# ---------------------------------------------------------------------------
# Terraform creates the parameter with a placeholder value. Set the real URL by hand:
#   aws ssm put-parameter --name /wordle/discord-webhook-url --type SecureString --overwrite --value 'https://discord.com/api/webhooks/...'
#
# value_wo is write-only: Terraform sends it to AWS but never stores it, and when it
# refreshes it records no value at all, so the real URL never ends up in state.
# Terraform sends value_wo only on create, or when value_wo_version changes, so your
# manual URL is never overwritten. (Bumping value_wo_version would reset it to the placeholder.)
resource "aws_ssm_parameter" "webhook_url" {
  name        = "/${var.project}/discord-webhook-url"
  description = "Discord webhook URL for the word-of-the-day bot. The value is set manually."
  type        = "SecureString"

  # MUST match webhookPlaceholder in main.go, which treats this value as "not set yet".
  value_wo         = "REPLACE_ME"
  value_wo_version = 1
}

# ---------------------------------------------------------------------------
# CloudWatch log group for the Lambda
# ---------------------------------------------------------------------------
# Created by Terraform (rather than automatically by Lambda) so the retention is set
# and `terraform destroy` removes it. The name must match /aws/lambda/<function name>.
resource "aws_cloudwatch_log_group" "lambda" {
  name              = "/aws/lambda/${local.function_name}"
  retention_in_days = var.log_retention_days
}

# ---------------------------------------------------------------------------
# IAM role and permissions for the Lambda
# ---------------------------------------------------------------------------
data "aws_iam_policy_document" "lambda_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "lambda" {
  name               = "${local.function_name}-role"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
}

# Least privilege: write to its own log group and read its one parameter, nothing else.
# Decrypting the SecureString needs no extra KMS permission, because it uses the
# AWS-managed aws/ssm key, which already allows use through SSM within this account.
data "aws_iam_policy_document" "lambda" {
  statement {
    sid       = "WriteLogs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.lambda.arn}:*"]
  }

  statement {
    sid       = "ReadWebhookParam"
    actions   = ["ssm:GetParameter"]
    resources = [aws_ssm_parameter.webhook_url.arn]
  }
}

resource "aws_iam_role_policy" "lambda" {
  name   = "${local.function_name}-policy"
  role   = aws_iam_role.lambda.id
  policy = data.aws_iam_policy_document.lambda.json
}

# ---------------------------------------------------------------------------
# Lambda function (Go, custom runtime)
# ---------------------------------------------------------------------------
# Build the binary first, from the project root:
#   GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -o build/bootstrap .
data "archive_file" "lambda" {
  type        = "zip"
  source_file = "${path.module}/${var.lambda_binary_path}"
  output_path = "${path.module}/../build/lambda.zip"
}

resource "aws_lambda_function" "wordle" {
  function_name = local.function_name
  role          = aws_iam_role.lambda.arn

  runtime       = "provided.al2023" # Go runs as a custom runtime; the go1.x runtime was retired.
  handler       = "bootstrap"       # Ignored by provided.* runtimes, but required by the API.
  architectures = ["arm64"]         # Graviton: about 20% cheaper than x86.
  memory_size   = var.lambda_memory_size
  timeout       = var.lambda_timeout

  filename         = data.archive_file.lambda.output_path
  source_code_hash = data.archive_file.lambda.output_base64sha256 # Redeploys when the binary changes.

  environment {
    variables = {
      WEBHOOK_PARAM_NAME = aws_ssm_parameter.webhook_url.name
    }
  }

  logging_config {
    log_format = "Text"
    log_group  = aws_cloudwatch_log_group.lambda.name
  }

  # Make sure the role can write logs before the first invocation.
  depends_on = [aws_iam_role_policy.lambda]
}

# ---------------------------------------------------------------------------
# Daily trigger (CloudWatch Events / EventBridge rule)
# ---------------------------------------------------------------------------
resource "aws_cloudwatch_event_rule" "daily" {
  name                = "${local.function_name}-daily"
  description         = "Triggers the Wordle word-of-the-day Lambda once a day."
  schedule_expression = var.schedule_expression
}

resource "aws_cloudwatch_event_target" "lambda" {
  rule = aws_cloudwatch_event_rule.daily.name
  arn  = aws_lambda_function.wordle.arn
}

# Allows EventBridge, and only this rule, to invoke the function.
resource "aws_lambda_permission" "allow_eventbridge" {
  statement_id  = "AllowEventBridgeInvoke"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.wordle.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.daily.arn
}
