# Creates the S3 bucket that holds Terraform state for this folder and ../ (the main stack).
# Run once per AWS account.
#
# This folder stores its own state in that bucket (bootstrap/terraform.tfstate), using the
# backend block below. On a brand-new account the bucket doesn't exist yet, so the first run
# needs local state: comment out the backend block, `terraform init && terraform apply`, put
# the state_bucket output into the backend blocks here and in ../main.tf, uncomment this one,
# then `terraform init -migrate-state`. See "Deploy from scratch" in the README.
#
# Locking uses S3 native lock files (use_lockfile), so no DynamoDB table is needed.

terraform {
  required_version = ">= 1.10"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }

  backend "s3" {
    bucket       = "wordle-tfstate-345265465845"
    key          = "bootstrap/terraform.tfstate"
    region       = "us-east-2"
    encrypt      = true
    use_lockfile = true
  }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project   = var.project
      ManagedBy = "terraform"
      Owner     = var.owner
    }
  }
}

data "aws_caller_identity" "current" {}

locals {
  # Bucket names are globally unique across all AWS accounts, so include the account ID.
  state_bucket = "${var.project}-tfstate-${data.aws_caller_identity.current.account_id}"
}

resource "aws_s3_bucket" "state" {
  bucket = local.state_bucket

  # Deleting this bucket would lose your Terraform state.
  lifecycle {
    prevent_destroy = true
  }
}

# Keeps previous versions of the state file, so a bad apply can be rolled back.
resource "aws_s3_bucket_versioning" "state" {
  bucket = aws_s3_bucket.state.id

  versioning_configuration {
    status = "Enabled"
  }
}

# Old state versions are kept for 90 days, then deleted. The current state is never expired.
resource "aws_s3_bucket_lifecycle_configuration" "state" {
  bucket = aws_s3_bucket.state.id

  rule {
    id     = "expire-old-state-versions"
    status = "Enabled"

    filter {} # applies to every object in the bucket

    noncurrent_version_expiration {
      noncurrent_days = 90
    }
  }

  # Lifecycle rules for noncurrent versions need versioning enabled first.
  depends_on = [aws_s3_bucket_versioning.state]
}

resource "aws_s3_bucket_server_side_encryption_configuration" "state" {
  bucket = aws_s3_bucket.state.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "state" {
  bucket = aws_s3_bucket.state.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# Rejects any request to the bucket that doesn't use HTTPS.
data "aws_iam_policy_document" "https_only" {
  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.state.arn, "${aws_s3_bucket.state.arn}/*"]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "state" {
  bucket = aws_s3_bucket.state.id
  policy = data.aws_iam_policy_document.https_only.json

  # Apply after the public access block, so the two bucket settings aren't changed at once.
  depends_on = [aws_s3_bucket_public_access_block.state]
}
