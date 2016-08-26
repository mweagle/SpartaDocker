# SpartaDocker
[Sparta](http://gosparta.io) application that provisions:
  - A Lambda function (_that pushes to_)
  - An SQS queue (_which is read from_)
  - An ECS-backed cluster of workers (_that logs the message to CloudWatch Logs_)

## Usage

```bash
export S3_BUCKET="{{My_S3_Bucket}}"
export SPARTA_SSH_KEY="{{SSH_KeyName_forEC2Instances}}"

docker -v && go get ./... && make provision
```

## Notes
  - This service has been tested with Docker, OSX v.1.12.0
  - This service provisions a CloudWatch LogGroup to which the ECS worker writes logs
