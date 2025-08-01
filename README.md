# EC2 Detailed Monitoring Checker

A CLI tool for bulk checking and disabling detailed monitoring settings on AWS EC2 instances.

[日本語版 README](README_ja.md)

## Features

- Check detailed monitoring status across multiple AWS profiles and regions
- Bulk disable detailed monitoring for enabled instances
- Smart exclusion of Auto Scaling Group and Elastic Beanstalk instances
- CSV output for results
- Progress bar for operation tracking
- Configurable settings via YAML files
- Robust retry logic and error handling

## Installation

```bash
git clone https://github.com/2matzzz/detailed-monitoring-enabled-ec2-checker.git
cd detailed-monitoring-enabled-ec2-checker
go mod tidy
go build -o ec2-checker .
```

## Usage

### Basic Usage

```bash
# Check instances with detailed monitoring enabled
./ec2-checker check profile1 profile2

# Disable detailed monitoring
./ec2-checker disable profile1 profile2

# Run with verbose logging
./ec2-checker disable -verbose profile1 profile2

# Show help
./ec2-checker -help
```

### Options

- `-config <path>`: Specify configuration file path
- `-dry-run`: Preview changes without making actual modifications
- `-progress <true|false>`: Control progress bar display (default: true)
- `-verbose`: Enable detailed logging
- `-help`: Show help message

## Configuration

You can customize behavior by creating a `config.yaml` file:

```yaml
# API Settings
api_timeout: "30s"
total_timeout: "20m"
max_retries: 3
batch_size: 20
max_concurrency: 10

# Behavior Settings
dry_run: false
show_progress: true
verbose: false

# Logging Settings
log_level: "info"  # debug, info, warn, error
log_format: "console"  # console, json

# Exclusion Settings
exclude_asg_instances: true
exclude_eb_instances: true
```

### Environment Variables

Settings can also be specified via environment variables:

```bash
export EC2_CHECKER_API_TIMEOUT="30s"
export EC2_CHECKER_MAX_CONCURRENCY="10"
export EC2_CHECKER_DRY_RUN="true"
export EC2_CHECKER_LOG_LEVEL="debug"
```

## Output Files

The following CSV files are generated during execution:

- `instances_with_detailed_monitoring.csv`: EC2 instances with detailed monitoring enabled
- `related_asgs.csv`: Related Auto Scaling Groups
- `excluded_asg_instances.csv`: Instances excluded due to ASG membership
- `excluded_eb_instances.csv`: Instances excluded due to Elastic Beanstalk membership

## How It Works

### Instance Exclusion Logic

Instances matching the following criteria are automatically excluded:

1. **Auto Scaling Group Instances** (when `exclude_asg_instances: true`)
   - These instances may have detailed monitoring configured via Launch Templates or Launch Configurations

2. **Elastic Beanstalk Environment Instances** (when `exclude_eb_instances: true`)
   - These instances are managed by EB environment configurations

### Error Handling

- Exponential backoff retry for AWS API rate limits
- Infinite loop prevention in pagination processing
- Automatic retry for network errors and temporary failures
- Error isolation at profile and region levels

### Performance Optimizations

- Concurrent processing for multi-region and multi-profile operations
- Pagination support for efficient handling of large datasets
- Semaphore-controlled concurrent connections
- Memory-efficient CSV streaming writes

## Required Permissions

The AWS profiles you use must have the following permissions:

```json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": [
                "ec2:DescribeInstances",
                "ec2:DescribeRegions",
                "ec2:UnmonitorInstances",
                "autoscaling:DescribeAutoScalingGroups",
                "elasticbeanstalk:DescribeEnvironments",
                "sts:GetCallerIdentity"
            ],
            "Resource": "*"
        }
    ]
}
```

## Troubleshooting

### Common Issues

1. **Permission Errors**
   ```
   Unable to describe instances: operation error EC2: DescribeInstances, https response error StatusCode: 403
   ```
   → Check your IAM permissions

2. **Profile Configuration Errors**
   ```
   Profile "profile-name": unable to describe regions
   ```
   → Verify your `~/.aws/config` and `~/.aws/credentials` settings

3. **Slow Performance**
   → Adjust `max_concurrency` setting or use `-verbose` to identify bottlenecks

### Debug

```bash
# Run with verbose logging
./ec2-checker disable -verbose profile1

# Set debug level via configuration file
echo "log_level: debug" > config.yaml
./ec2-checker -config config.yaml disable profile1
```

## Important Notes

- This tool **disables** detailed monitoring. While designed for cost reduction, understand that monitoring information will be lost
- Auto Scaling Group and Elastic Beanstalk instances are automatically excluded, but this can be disabled via configuration
- Processing large numbers of instances may take time due to AWS API limitations

## License

MIT License

## Contributing

Issue reports and Pull Requests are welcome.

