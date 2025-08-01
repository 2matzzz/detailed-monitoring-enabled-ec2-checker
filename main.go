package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticbeanstalk"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/elasticbeanstalk/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/transport/http"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

// Custom error types for better error handling
type AppError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Profile string `json:"profile,omitempty"`
	Region  string `json:"region,omitempty"`
	Cause   error  `json:"cause,omitempty"`
}

func (e *AppError) Error() string {
	if e.Profile != "" && e.Region != "" {
		return fmt.Sprintf("[%s] Profile: %s, Region: %s - %s", e.Type, e.Profile, e.Region, e.Message)
	} else if e.Profile != "" {
		return fmt.Sprintf("[%s] Profile: %s - %s", e.Type, e.Profile, e.Message)
	}
	return fmt.Sprintf("[%s] %s", e.Type, e.Message)
}

func (e *AppError) Unwrap() error {
	return e.Cause
}

// Error type constructors
func NewConfigError(message string, cause error) *AppError {
	return &AppError{Type: "CONFIG_ERROR", Message: message, Cause: cause}
}

func NewAWSError(message, profile, region string, cause error) *AppError {
	return &AppError{Type: "AWS_ERROR", Message: message, Profile: profile, Region: region, Cause: cause}
}

func NewValidationError(message string) *AppError {
	return &AppError{Type: "VALIDATION_ERROR", Message: message}
}

// Retry strategy with exponential backoff
type RetryStrategy struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	Multiplier float64
	Jitter     bool
}

func NewRetryStrategy(maxRetries int) *RetryStrategy {
	return &RetryStrategy{
		MaxRetries: maxRetries,
		BaseDelay:  1 * time.Second,
		MaxDelay:   30 * time.Second,
		Multiplier: 2.0,
		Jitter:     true,
	}
}

func (rs *RetryStrategy) Execute(ctx context.Context, operation func() error) error {
	var lastErr error

	for attempt := 0; attempt <= rs.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := rs.calculateDelay(attempt)
			log.Debug().Dur("delay", delay).Int("attempt", attempt).Msg("Retrying after delay")

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if err := operation(); err != nil {
			lastErr = err

			// Don't retry on context cancellation or certain AWS errors
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}

			// Check for non-retryable AWS errors
			if rs.isNonRetryableError(err) {
				return err
			}

			if attempt < rs.MaxRetries {
				log.Warn().Err(err).Int("attempt", attempt+1).Int("max_retries", rs.MaxRetries).Msg("Operation failed, retrying")
			}
			continue
		}

		return nil
	}

	return fmt.Errorf("operation failed after %d attempts: %w", rs.MaxRetries, lastErr)
}

func (rs *RetryStrategy) calculateDelay(attempt int) time.Duration {
	delay := time.Duration(float64(rs.BaseDelay) * rs.Multiplier * float64(attempt))

	if delay > rs.MaxDelay {
		delay = rs.MaxDelay
	}

	if rs.Jitter {
		// Add jitter to avoid thundering herd
		jitter := time.Duration(float64(delay) * 0.1 * (2.0*float64(time.Now().UnixNano()%1000)/1000.0 - 1.0))
		delay += jitter
	}

	return delay
}

func (rs *RetryStrategy) isNonRetryableError(err error) bool {
	// AWS SDK v2 error handling
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "UnauthorizedOperation", "AccessDenied", "InvalidUserID.NotFound",
			"InvalidInstanceID.NotFound", "ValidationException":
			return true
		}
	}

	var respErr *http.ResponseError
	if errors.As(err, &respErr) {
		// Don't retry on 4xx errors except rate limiting
		if respErr.HTTPStatusCode() >= 400 && respErr.HTTPStatusCode() < 500 {
			if respErr.HTTPStatusCode() != 429 { // 429 is rate limiting, should retry
				return true
			}
		}
	}

	return false
}

// Interfaces for dependency injection and testability
type EC2Client interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeRegions(ctx context.Context, params *ec2.DescribeRegionsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error)
	UnmonitorInstances(ctx context.Context, params *ec2.UnmonitorInstancesInput, optFns ...func(*ec2.Options)) (*ec2.UnmonitorInstancesOutput, error)
}

type AutoScalingClient interface {
	DescribeAutoScalingGroups(ctx context.Context, params *autoscaling.DescribeAutoScalingGroupsInput, optFns ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
}

type STSClient interface {
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

type ElasticBeanstalkClient interface {
	DescribeEnvironments(ctx context.Context, params *elasticbeanstalk.DescribeEnvironmentsInput, optFns ...func(*elasticbeanstalk.Options)) (*elasticbeanstalk.DescribeEnvironmentsOutput, error)
	DescribeConfigurationSettings(ctx context.Context, params *elasticbeanstalk.DescribeConfigurationSettingsInput, optFns ...func(*elasticbeanstalk.Options)) (*elasticbeanstalk.DescribeConfigurationSettingsOutput, error)
}

// Service layer for better testability
type AWSService struct {
	ec2Client EC2Client
	asgClient AutoScalingClient
	stsClient STSClient
	ebClient  ElasticBeanstalkClient
	config    *Config
	retry     *RetryStrategy
}

func NewAWSService(ec2Client EC2Client, asgClient AutoScalingClient, stsClient STSClient, ebClient ElasticBeanstalkClient, config *Config) *AWSService {
	return &AWSService{
		ec2Client: ec2Client,
		asgClient: asgClient,
		stsClient: stsClient,
		ebClient:  ebClient,
		config:    config,
		retry:     NewRetryStrategy(config.MaxRetries),
	}
}

func (s *AWSService) GetAccountID(ctx context.Context) (string, error) {
	var output *sts.GetCallerIdentityOutput

	err := s.retry.Execute(ctx, func() error {
		apiCtx, cancel := context.WithTimeout(ctx, s.config.APITimeout)
		defer cancel()

		var err error
		output, err = s.stsClient.GetCallerIdentity(apiCtx, &sts.GetCallerIdentityInput{})
		return err
	})

	if err != nil {
		return "", NewAWSError("failed to get account ID", "", "", err)
	}
	if output.Account == nil {
		return "", NewValidationError("account ID is nil")
	}
	return *output.Account, nil
}

func (s *AWSService) GetRegions(ctx context.Context) ([]string, error) {
	var output *ec2.DescribeRegionsOutput

	err := s.retry.Execute(ctx, func() error {
		apiCtx, cancel := context.WithTimeout(ctx, s.config.APITimeout)
		defer cancel()

		var err error
		output, err = s.ec2Client.DescribeRegions(apiCtx, &ec2.DescribeRegionsInput{})
		return err
	})

	if err != nil {
		return nil, NewAWSError("failed to describe regions", "", "", err)
	}

	var regions []string
	for _, region := range output.Regions {
		if region.RegionName != nil {
			regions = append(regions, *region.RegionName)
		}
	}
	return regions, nil
}

func (s *AWSService) GetInstances(ctx context.Context, cache *EnvironmentCache, region, accountID string) ([]InstanceInfo, error) {
	var instances []InstanceInfo
	var nextToken *string

	// Use pagination to handle large numbers of instances efficiently
	for {
		var output *ec2.DescribeInstancesOutput

		// Use retry strategy for API calls
		err := s.retry.Execute(ctx, func() error {
			apiCtx, cancel := context.WithTimeout(ctx, s.config.APITimeout)
			defer cancel()

			var err error
			output, err = s.ec2Client.DescribeInstances(apiCtx, &ec2.DescribeInstancesInput{
				NextToken:  nextToken,
				MaxResults: aws.Int32(100), // Optimize API calls with pagination
			})
			return err
		})

		if err != nil {
			return nil, NewAWSError("failed to describe instances", "", region, err)
		}

		// Process current page
		for _, reservation := range output.Reservations {
			for _, instance := range reservation.Instances {
				if instance.State != nil && instance.State.Name == ec2types.InstanceStateNameRunning {
					exclude, err := isExcludedInstance(ctx, instance, cache)
					if err != nil {
						log.Error().Err(err).Str("accountID", accountID).Str("region", region).Msg("Error checking excluded instance")
						continue
					}
					if exclude {
						continue
					}

					var name, asgName string
					for _, tag := range instance.Tags {
						if tag.Key != nil && tag.Value != nil {
							if *tag.Key == "aws:autoscaling:groupName" {
								asgName = *tag.Value
							}
							if *tag.Key == "Name" {
								name = *tag.Value
							}
						}
					}

					if instance.Monitoring != nil && instance.Monitoring.State == ec2types.MonitoringStateEnabled {
						if instance.InstanceId == nil {
							log.Warn().Msg("Instance ID is nil, skipping instance")
							continue
						}
						instances = append(instances, InstanceInfo{
							AccountID:       accountID,
							Region:          region,
							InstanceID:      *instance.InstanceId,
							Name:            name,
							MonitoringState: string(instance.Monitoring.State),
							ASGName:         asgName,
						})
					}
				}
			}
		}

		// Check if there are more pages
		if output.NextToken == nil {
			break
		}
		nextToken = output.NextToken

		// Add progress logging for large datasets
		log.Debug().Int("instances_found", len(instances)).Str("region", region).Msg("Processing instances page")
	}

	log.Info().Int("total_instances", len(instances)).Str("region", region).Msg("Completed instance discovery")
	return instances, nil
}

func (s *AWSService) GetRelatedAutoScalingGroups(ctx context.Context, instances []InstanceInfo, accountID, region string) ([]ASGInfo, error) {
	asgNames := make(map[string]struct{})
	for _, instance := range instances {
		if instance.ASGName != "" {
			asgNames[instance.ASGName] = struct{}{}
		}
	}

	// If no ASG names found, return empty slice
	if len(asgNames) == 0 {
		return []ASGInfo{}, nil
	}

	// Convert map to slice for API call
	var asgNamesList []string
	for name := range asgNames {
		asgNamesList = append(asgNamesList, name)
	}

	var relatedASGs []ASGInfo

	// Process ASGs in batches to avoid API limits (max 50 ASG names per call)
	const maxASGsPerCall = 50
	for i := 0; i < len(asgNamesList); i += maxASGsPerCall {
		end := i + maxASGsPerCall
		if end > len(asgNamesList) {
			end = len(asgNamesList)
		}
		batch := asgNamesList[i:end]

		// Use pagination for ASG API calls
		var nextToken *string
		for {
			var output *autoscaling.DescribeAutoScalingGroupsOutput

			err := s.retry.Execute(ctx, func() error {
				apiCtx, cancel := context.WithTimeout(ctx, s.config.APITimeout)
				defer cancel()

				var err error
				output, err = s.asgClient.DescribeAutoScalingGroups(apiCtx, &autoscaling.DescribeAutoScalingGroupsInput{
					AutoScalingGroupNames: batch,
					NextToken:             nextToken,
					MaxRecords:            aws.Int32(100),
				})
				return err
			})

			if err != nil {
				return nil, NewAWSError("failed to describe ASGs", "", region, err)
			}

			for _, asg := range output.AutoScalingGroups {
				if asg.AutoScalingGroupName == nil {
					log.Warn().Msg("ASG name is nil, skipping ASG")
					continue
				}

				var launchTemplate, launchConfig string

				if asg.LaunchTemplate != nil && asg.LaunchTemplate.LaunchTemplateId != nil {
					launchTemplate = *asg.LaunchTemplate.LaunchTemplateId
				}

				if asg.LaunchConfigurationName != nil {
					launchConfig = *asg.LaunchConfigurationName
				}

				relatedASGs = append(relatedASGs, ASGInfo{
					AccountID:           accountID,
					Region:              region,
					ASGName:             *asg.AutoScalingGroupName,
					LaunchTemplate:      launchTemplate,
					LaunchConfiguration: launchConfig,
				})
			}

			if output.NextToken == nil {
				break
			}
			nextToken = output.NextToken
		}
	}

	log.Info().Int("total_asgs", len(relatedASGs)).Str("region", region).Msg("Completed ASG discovery")
	return relatedASGs, nil
}

func (s *AWSService) DisableMonitoring(ctx context.Context, instances []InstanceInfo, accountID, region string) error {
	var instanceIDs []string

	for _, instance := range instances {
		if instance.MonitoringState == string(ec2types.MonitoringStateEnabled) {
			instanceIDs = append(instanceIDs, instance.InstanceID)
		}
	}

	for i := 0; i < len(instanceIDs); i += s.config.BatchSize {
		end := i + s.config.BatchSize
		if end > len(instanceIDs) {
			end = len(instanceIDs)
		}
		batch := instanceIDs[i:end]

		if s.config.DryRun {
			log.Info().Str("accountID", accountID).Str("region", region).Strs("instances", batch).Msg("DRY RUN: Would disable detailed monitoring for instances")
			continue
		}

		err := s.retry.Execute(ctx, func() error {
			apiCtx, cancel := context.WithTimeout(ctx, s.config.APITimeout)
			defer cancel()

			_, err := s.ec2Client.UnmonitorInstances(apiCtx, &ec2.UnmonitorInstancesInput{
				InstanceIds: batch,
			})
			return err
		})

		if err != nil {
			// Log detailed error information
			var ae smithy.APIError
			if errors.As(err, &ae) {
				switch ae.ErrorCode() {
				case "UnauthorizedOperation":
					log.Error().Str("accountID", accountID).Str("region", region).Msg("You do not have permission to unmonitor instances")
				case "InvalidInstanceID.NotFound":
					log.Error().Str("accountID", accountID).Str("region", region).Err(err).Msg("Some instance IDs were not found")
				case "IncorrectInstanceState":
					log.Error().Str("accountID", accountID).Str("region", region).Msg("The instance is not in a valid state for this operation")
				case "OptInRequired":
					log.Error().Str("accountID", accountID).Str("region", region).Msg("You are not subscribed to the service required to perform this action")
				default:
					log.Error().Str("error_code", ae.ErrorCode()).Str("error_message", ae.ErrorMessage()).Msg("EC2 API Error")
				}
			} else if respErr := new(http.ResponseError); errors.As(err, &respErr) {
				log.Error().Str("error", respErr.Error()).Int("status_code", respErr.HTTPStatusCode()).Msg("HTTP Error")
			} else {
				log.Error().Err(err).Msg("Unknown Error")
			}
			return NewAWSError("failed to disable monitoring", "", region, err)
		}
		log.Info().Str("accountID", accountID).Str("region", region).Strs("instances", batch).Msg("Disabled detailed monitoring for instances")
	}

	return nil
}

// Default configuration values
const (
	defaultMaxRetries     = 3
	defaultBatchSize      = 20
	defaultMaxConcurrency = 10
	defaultAPITimeout     = 30 * time.Second
	defaultTotalTimeout   = 10 * time.Minute
)

// Config represents the application configuration
type Config struct {
	MaxRetries     int           `yaml:"max_retries" env:"MAX_RETRIES"`
	BatchSize      int           `yaml:"batch_size" env:"BATCH_SIZE"`
	MaxConcurrency int           `yaml:"max_concurrency" env:"MAX_CONCURRENCY"`
	APITimeout     time.Duration `yaml:"api_timeout" env:"API_TIMEOUT"`
	TotalTimeout   time.Duration `yaml:"total_timeout" env:"TOTAL_TIMEOUT"`
	LogLevel       string        `yaml:"log_level" env:"LOG_LEVEL"`
	LogFormat      string        `yaml:"log_format" env:"LOG_FORMAT"` // "json" or "console"
	DryRun         bool          `yaml:"dry_run" env:"DRY_RUN"`
	ShowProgress   bool          `yaml:"show_progress" env:"SHOW_PROGRESS"`
	Verbose        bool          `yaml:"verbose" env:"VERBOSE"`
}

// NewDefaultConfig returns a config with default values
func NewDefaultConfig() *Config {
	return &Config{
		MaxRetries:     defaultMaxRetries,
		BatchSize:      defaultBatchSize,
		MaxConcurrency: defaultMaxConcurrency,
		APITimeout:     defaultAPITimeout,
		TotalTimeout:   defaultTotalTimeout,
		LogLevel:       "info",
		LogFormat:      "console",
		DryRun:         false,
		ShowProgress:   true,
		Verbose:        false,
	}
}

// LoadConfig loads configuration from file and environment variables
func LoadConfig(configPath string) (*Config, error) {
	config := NewDefaultConfig()

	// Load from file if exists
	if configPath != "" {
		if _, err := os.Stat(configPath); err == nil {
			data, err := os.ReadFile(configPath)
			if err != nil {
				return nil, NewConfigError("failed to read config file", err)
			}

			if err := yaml.Unmarshal(data, config); err != nil {
				return nil, NewConfigError("failed to parse config file", err)
			}
		}
	}

	// Override with environment variables
	if val := os.Getenv("MAX_RETRIES"); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil {
			config.MaxRetries = parsed
		}
	}
	if val := os.Getenv("BATCH_SIZE"); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil {
			config.BatchSize = parsed
		}
	}
	if val := os.Getenv("MAX_CONCURRENCY"); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil {
			config.MaxConcurrency = parsed
		}
	}
	if val := os.Getenv("API_TIMEOUT"); val != "" {
		if parsed, err := time.ParseDuration(val); err == nil {
			config.APITimeout = parsed
		}
	}
	if val := os.Getenv("TOTAL_TIMEOUT"); val != "" {
		if parsed, err := time.ParseDuration(val); err == nil {
			config.TotalTimeout = parsed
		}
	}
	if val := os.Getenv("LOG_LEVEL"); val != "" {
		config.LogLevel = val
	}
	if val := os.Getenv("LOG_FORMAT"); val != "" {
		config.LogFormat = val
	}
	if val := os.Getenv("DRY_RUN"); val != "" {
		config.DryRun = val == "true" || val == "1"
	}
	if val := os.Getenv("SHOW_PROGRESS"); val != "" {
		config.ShowProgress = val == "true" || val == "1"
	}
	if val := os.Getenv("VERBOSE"); val != "" {
		config.Verbose = val == "true" || val == "1"
	}

	return config, nil
}

func showUsage() {
	fmt.Printf(`AWS EC2 Detailed Monitoring Checker

USAGE:
    %s [OPTIONS] COMMAND PROFILES...

COMMANDS:
    check    Check for instances with detailed monitoring enabled
    disable  Disable detailed monitoring for instances

OPTIONS:
    -config PATH          Path to configuration file
    -dry-run             Show what would be done without making changes
    -progress            Show progress bar (default: true)
    -verbose             Enable verbose logging
    -help               Show this help message

EXAMPLES:
    %s check prod staging
    %s -dry-run disable prod
    %s -config custom.yaml -verbose check dev

ENVIRONMENT VARIABLES:
    MAX_RETRIES        Maximum number of API retries (default: 3)
    BATCH_SIZE         Batch size for processing (default: 20)
    MAX_CONCURRENCY    Maximum concurrent goroutines (default: 10)
    API_TIMEOUT        API call timeout (default: 30s)
    TOTAL_TIMEOUT      Total execution timeout (default: 10m)
    LOG_LEVEL          Log level: debug, info, warn, error (default: info)
    LOG_FORMAT         Log format: console, json (default: console)
    DRY_RUN           Enable dry-run mode (default: false)
    SHOW_PROGRESS     Show progress bar (default: true)
    VERBOSE           Enable verbose logging (default: false)

For more information, visit: https://github.com/2matzzz/detailed-monitoring-enabled-ec2-checker
`, os.Args[0], os.Args[0], os.Args[0], os.Args[0])
}

type InstanceInfo struct {
	AccountID       string
	Region          string
	InstanceID      string
	Name            string
	MonitoringState string
	ASGName         string
}

type ASGInfo struct {
	AccountID           string
	Region              string
	ASGName             string
	LaunchTemplate      string
	LaunchConfiguration string
}

type EnvironmentCache struct {
	environmentAppMap   map[string]string
	environmentSettings map[string][]ebtypes.ConfigurationOptionSetting
	ebClient            *elasticbeanstalk.Client
}

func main() {
	// Custom flag parsing for better UX
	var configPath string
	var dryRun bool
	var showProgress bool
	var verbose bool
	var showHelp bool

	flag.StringVar(&configPath, "config", "", "Path to configuration file")
	flag.BoolVar(&dryRun, "dry-run", false, "Show what would be done without making changes")
	flag.BoolVar(&showProgress, "progress", true, "Show progress bar")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose logging")
	flag.BoolVar(&showHelp, "help", false, "Show help message")
	flag.Parse()

	if showHelp {
		showUsage()
		os.Exit(0)
	}

	appConfig, err := LoadConfig(configPath)
	if err != nil {
		var appErr *AppError
		if errors.As(err, &appErr) {
			log.Fatal().Str("error_type", appErr.Type).Err(appErr.Cause).Msg(appErr.Message)
		} else {
			log.Fatal().Err(err).Msg("Failed to load configuration")
		}
	}

	// Override config with CLI flags
	if dryRun {
		appConfig.DryRun = true
	}
	if !showProgress {
		appConfig.ShowProgress = false
	}
	if verbose {
		appConfig.Verbose = true
		appConfig.LogLevel = "debug"
	}

	// Create main context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), appConfig.TotalTimeout)
	defer cancel()

	// Initialize zerolog
	zerolog.TimeFieldFormat = time.RFC3339

	// Set log level - suppress INFO/DEBUG when progress bar is shown
	switch strings.ToLower(appConfig.LogLevel) {
	case "debug":
		if appConfig.ShowProgress && !appConfig.Verbose {
			zerolog.SetGlobalLevel(zerolog.WarnLevel)
		} else {
			zerolog.SetGlobalLevel(zerolog.DebugLevel)
		}
	case "info":
		if appConfig.ShowProgress && !appConfig.Verbose {
			zerolog.SetGlobalLevel(zerolog.WarnLevel)
		} else {
			zerolog.SetGlobalLevel(zerolog.InfoLevel)
		}
	case "warn":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case "error":
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	default:
		if appConfig.ShowProgress && !appConfig.Verbose {
			zerolog.SetGlobalLevel(zerolog.WarnLevel)
		} else {
			zerolog.SetGlobalLevel(zerolog.InfoLevel)
		}
	}

	// Set log format - redirect to file when progress bar is shown
	var logOutput *os.File
	if appConfig.ShowProgress && !appConfig.Verbose {
		// Create log file for progress mode
		var err error
		logOutput, err = os.OpenFile("ec2-checker.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			logOutput = os.Stderr
		}
		defer func() {
			if logOutput != os.Stderr {
				logOutput.Close()
			}
		}()
	} else {
		logOutput = os.Stderr
	}

	if appConfig.LogFormat == "json" {
		log.Logger = zerolog.New(logOutput).With().Timestamp().Logger()
	} else {
		log.Logger = zerolog.New(zerolog.ConsoleWriter{
			Out:        logOutput,
			TimeFormat: "15:04:05",
			FormatLevel: func(i interface{}) string {
				switch i {
				case "debug":
					return "DEBUG"
				case "info":
					return "INFO"
				case "warn":
					return "WARN"
				case "error":
					return "ERROR"
				case "fatal":
					return "FATAL"
				case "panic":
					return "PANIC"
				default:
					return strings.ToUpper(fmt.Sprintf("%s", i))
				}
			},
		}).With().Timestamp().Logger()
	}

	checkCmd := flag.NewFlagSet("check", flag.ExitOnError)
	disableCmd := flag.NewFlagSet("disable", flag.ExitOnError)

	if len(os.Args) < 2 {
		fmt.Println("expected 'check' or 'disable' subcommands")
		os.Exit(1)
	}

	var profiles []string
	switch os.Args[1] {
	case "check":
		checkCmd.Parse(os.Args[2:])
		profiles = checkCmd.Args()
		if len(profiles) == 0 {
			log.Fatal().Msg("No profiles specified for check. Usage: go run main.go check <profile1> [profile2] ...")
		}
	case "disable":
		disableCmd.Parse(os.Args[2:])
		profiles = disableCmd.Args()
		if len(profiles) == 0 {
			log.Fatal().Msg("No profiles specified for disable. Usage: go run main.go disable <profile1> [profile2] ...")
		}
	default:
		fmt.Println("expected 'check' or 'disable' subcommands")
		os.Exit(1)
	}

	action := os.Args[1]

	var allInstances []InstanceInfo
	var allASGs = make(map[string]ASGInfo)
	var launchTemplates = make(map[string]struct{})
	var launchConfigurations = make(map[string]struct{})
	var errors []string
	var mutex sync.Mutex
	var errorMutex sync.Mutex
	var wg sync.WaitGroup

	// Semaphore to limit concurrency
	semaphore := make(chan struct{}, appConfig.MaxConcurrency)

	// Progress tracking
	var totalRegions int
	var completedRegions int
	var progressMutex sync.Mutex
	var progressBar *SimpleProgressBar
	var progressInitialized bool

	for _, profile := range profiles {
		wg.Add(1)
		go func(profile string) {
			defer wg.Done()

			cfg, err := awsconfig.LoadDefaultConfig(ctx,
				awsconfig.WithSharedConfigProfile(profile),
				awsconfig.WithClientLogMode(aws.LogRetries),
			)
			if err != nil {
				errorMutex.Lock()
				errors = append(errors, fmt.Sprintf("Profile %s: unable to load SDK config: %v", profile, err))
				errorMutex.Unlock()
				return
			}

			// Create AWS service clients
			ec2Client := ec2.NewFromConfig(cfg)
			asgClient := autoscaling.NewFromConfig(cfg)
			stsClient := sts.NewFromConfig(cfg)
			ebClient := elasticbeanstalk.NewFromConfig(cfg)

			// Create service layer
			awsService := NewAWSService(ec2Client, asgClient, stsClient, ebClient, appConfig)

			accountID, err := awsService.GetAccountID(ctx)
			if err != nil {
				errorMutex.Lock()
				errors = append(errors, fmt.Sprintf("Profile %s: unable to get account ID: %v", profile, err))
				errorMutex.Unlock()
				return
			}

			regions, err := awsService.GetRegions(ctx)
			if err != nil {
				errorMutex.Lock()
				errors = append(errors, fmt.Sprintf("Profile %s: unable to describe regions: %v", profile, err))
				errorMutex.Unlock()
				return
			}

			// Initialize progress bar with actual region count
			progressMutex.Lock()
			if !progressInitialized && appConfig.ShowProgress && !appConfig.Verbose {
				totalRegions = len(regions) * len(profiles)
				progressBar = NewSimpleProgressBar(totalRegions, "Processing regions...")
				progressInitialized = true
			}
			progressMutex.Unlock()

			for _, region := range regions {
				wg.Add(1)
				go func(region string) {
					defer wg.Done()
					defer func() {
						// Update progress
						progressMutex.Lock()
						completedRegions++
						if progressBar != nil {
							progressBar.Add(1)
						}
						progressMutex.Unlock()
					}()

					// Acquire semaphore to limit concurrency
					semaphore <- struct{}{}
					defer func() { <-semaphore }()

					if appConfig.Verbose {
						log.Info().Str("profile", profile).Str("region", region).Msg("Processing region")
					} else if !appConfig.ShowProgress {
						fmt.Printf("Processing profile: %s, region: %s\n", profile, region)
					}

					regionCfg := cfg.Copy()
					regionCfg.Region = region

					// Create regional clients
					regionalEC2Client := ec2.NewFromConfig(regionCfg)
					regionalASGClient := autoscaling.NewFromConfig(regionCfg)
					regionalEbClient := elasticbeanstalk.NewFromConfig(regionCfg)

					// Create regional service
					regionalService := NewAWSService(regionalEC2Client, regionalASGClient, stsClient, regionalEbClient, appConfig)
					cache := NewEnvironmentCache(regionalEbClient)

					instances, err := regionalService.GetInstances(ctx, cache, region, accountID)
					if err != nil {
						errorMutex.Lock()
						errors = append(errors, fmt.Sprintf("Profile %s, Region %s: unable to describe instances: %v", profile, region, err))
						errorMutex.Unlock()
						return
					}

					asgs, err := regionalService.GetRelatedAutoScalingGroups(ctx, instances, accountID, region)
					if err != nil {
						errorMutex.Lock()
						errors = append(errors, fmt.Sprintf("Profile %s, Region %s: unable to describe ASGs: %v", profile, region, err))
						errorMutex.Unlock()
						return
					}

					mutex.Lock()
					allInstances = append(allInstances, instances...)
					for _, asg := range asgs {
						allASGs[asg.ASGName] = asg

						if asg.LaunchTemplate != "" {
							launchTemplates[fmt.Sprintf("%s,%s,%s", accountID, region, asg.LaunchTemplate)] = struct{}{}
						}
						if asg.LaunchConfiguration != "" {
							launchConfigurations[fmt.Sprintf("%s,%s,%s", accountID, region, asg.LaunchConfiguration)] = struct{}{}
						}
					}
					mutex.Unlock()

					if action == "disable" {
						if err := regionalService.DisableMonitoring(ctx, instances, accountID, region); err != nil {
							log.Error().Err(err).Str("profile", profile).Str("region", region).Msg("Failed to disable monitoring")
						}
					}
				}(region)
			}
		}(profile)
	}

	wg.Wait()

	// Complete progress bar
	if progressBar != nil {
		progressBar.Finish()
		fmt.Println() // Add newline after progress bar
	}

	// Report any errors
	if len(errors) > 0 {
		fmt.Printf("Encountered %d errors during processing:\n", len(errors))
		for _, err := range errors {
			log.Error().Str("error", err).Msg("Processing error")
		}
		if len(allInstances) == 0 && len(allASGs) == 0 {
			os.Exit(1)
		}
		fmt.Printf("Proceeding with partial results...\n")
	}

	if action == "check" {
		saveInstancesToCSV(allInstances, "detailed-monitoring-enabled-ec2-list.csv")
		saveASGsToCSV(allASGs, "asg-list.csv")
		saveMapToCSV(launchTemplates, "launch-template-list.csv", "LaunchTemplate")
		saveMapToCSV(launchConfigurations, "launch-configuration-list.csv", "LaunchConfiguration")
		fmt.Println("CSV export completed successfully.")
	}
}

func NewEnvironmentCache(ebClient *elasticbeanstalk.Client) *EnvironmentCache {
	return &EnvironmentCache{
		environmentAppMap:   make(map[string]string),
		environmentSettings: make(map[string][]ebtypes.ConfigurationOptionSetting),
		ebClient:            ebClient,
	}
}

func getAccountID(ctx context.Context, client *sts.Client, config *Config) (string, error) {
	apiCtx, cancel := context.WithTimeout(ctx, config.APITimeout)
	defer cancel()
	output, err := client.GetCallerIdentity(apiCtx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	if output.Account == nil {
		return "", fmt.Errorf("account ID is nil")
	}
	return *output.Account, nil
}

func getRegions(ctx context.Context, client *ec2.Client, config *Config) ([]string, error) {
	apiCtx, cancel := context.WithTimeout(ctx, config.APITimeout)
	defer cancel()
	output, err := client.DescribeRegions(apiCtx, &ec2.DescribeRegionsInput{})
	if err != nil {
		return nil, err
	}

	var regions []string
	for _, region := range output.Regions {
		if region.RegionName != nil {
			regions = append(regions, *region.RegionName)
		}
	}
	return regions, nil
}

func (cache *EnvironmentCache) GetApplicationName(ctx context.Context, environmentName string) (string, error) {
	if appName, exists := cache.environmentAppMap[environmentName]; exists {
		return appName, nil
	}

	output, err := cache.ebClient.DescribeEnvironments(ctx, &elasticbeanstalk.DescribeEnvironmentsInput{
		EnvironmentNames: []string{environmentName},
	})
	if err != nil {
		return "", err
	}

	if len(output.Environments) == 0 {
		return "", fmt.Errorf("environment %s not found", environmentName)
	}

	if output.Environments[0].ApplicationName == nil {
		return "", fmt.Errorf("application name is nil for environment %s", environmentName)
	}

	appName := *output.Environments[0].ApplicationName
	cache.environmentAppMap[environmentName] = appName
	return appName, nil
}

func (cache *EnvironmentCache) GetEnvironmentSettings(ctx context.Context, applicationName, environmentName string) ([]ebtypes.ConfigurationOptionSetting, error) {
	cacheKey := fmt.Sprintf("%s:%s", applicationName, environmentName)
	if settings, exists := cache.environmentSettings[cacheKey]; exists {
		return settings, nil
	}

	output, err := cache.ebClient.DescribeConfigurationSettings(ctx, &elasticbeanstalk.DescribeConfigurationSettingsInput{
		ApplicationName: aws.String(applicationName),
		EnvironmentName: aws.String(environmentName),
	})
	if err != nil {
		return nil, err
	}

	if len(output.ConfigurationSettings) == 0 {
		return nil, fmt.Errorf("no configuration settings found for environment %s", environmentName)
	}

	settings := output.ConfigurationSettings[0].OptionSettings
	cache.environmentSettings[cacheKey] = settings
	return settings, nil
}

func isExcludedInstance(ctx context.Context, instance ec2types.Instance, cache *EnvironmentCache) (bool, error) {
	var environmentName string

	for _, tag := range instance.Tags {
		if tag.Key != nil && tag.Value != nil && strings.HasPrefix(*tag.Key, "elasticbeanstalk:environment-name") {
			environmentName = *tag.Value
			break
		}
	}

	if environmentName == "" {
		return false, nil
	}

	applicationName, err := cache.GetApplicationName(ctx, environmentName)
	if err != nil {
		return false, err
	}

	settings, err := cache.GetEnvironmentSettings(ctx, applicationName, environmentName)
	if err != nil {
		return false, err
	}

	// Check monitoring setting
	for _, option := range settings {
		if aws.ToString(option.Namespace) == "aws:elasticbeanstalk:healthreporting:system" &&
			aws.ToString(option.OptionName) == "SystemType" {
			if aws.ToString(option.Value) == "enhanced" {
				return true, nil
			}
		}
	}

	return false, nil
}

func getInstances(ctx context.Context, ec2Client *ec2.Client, cache *EnvironmentCache, cfg aws.Config, accountID string, config *Config) ([]InstanceInfo, error) {
	var instances []InstanceInfo
	var nextToken *string

	retry := NewRetryStrategy(config.MaxRetries)

	// Use pagination to handle large numbers of instances efficiently
	for {
		var output *ec2.DescribeInstancesOutput

		// Use retry strategy for API calls
		err := retry.Execute(ctx, func() error {
			apiCtx, cancel := context.WithTimeout(ctx, config.APITimeout)
			defer cancel()

			var err error
			output, err = ec2Client.DescribeInstances(apiCtx, &ec2.DescribeInstancesInput{
				NextToken:  nextToken,
				MaxResults: aws.Int32(100), // Optimize API calls with pagination
			})
			return err
		})

		if err != nil {
			return nil, NewAWSError("failed to describe instances", "", cfg.Region, err)
		}

		// Process current page
		for _, reservation := range output.Reservations {
			for _, instance := range reservation.Instances {
				if instance.State != nil && instance.State.Name == ec2types.InstanceStateNameRunning {
					exclude, err := isExcludedInstance(ctx, instance, cache)
					if err != nil {
						log.Error().Err(err).Str("accountID", accountID).Str("region", cfg.Region).Msg("Error checking excluded instance")
						continue
					}
					if exclude {
						continue
					}

					var name, asgName string
					for _, tag := range instance.Tags {
						if tag.Key != nil && tag.Value != nil {
							if *tag.Key == "aws:autoscaling:groupName" {
								asgName = *tag.Value
							}
							if *tag.Key == "Name" {
								name = *tag.Value
							}
						}
					}

					if instance.Monitoring != nil && instance.Monitoring.State == ec2types.MonitoringStateEnabled {
						if instance.InstanceId == nil {
							log.Warn().Msg("Instance ID is nil, skipping instance")
							continue
						}
						instances = append(instances, InstanceInfo{
							AccountID:       accountID,
							Region:          cfg.Region,
							InstanceID:      *instance.InstanceId,
							Name:            name,
							MonitoringState: string(instance.Monitoring.State),
							ASGName:         asgName,
						})
					}
				}
			}
		}

		// Check if there are more pages
		if output.NextToken == nil {
			break
		}
		nextToken = output.NextToken

		// Add progress logging for large datasets
		log.Debug().Int("instances_found", len(instances)).Str("region", cfg.Region).Msg("Processing instances page")
	}

	log.Info().Int("total_instances", len(instances)).Str("region", cfg.Region).Msg("Completed instance discovery")
	return instances, nil
}

func getRelatedAutoScalingGroups(ctx context.Context, asgClient *autoscaling.Client, instances []InstanceInfo, accountID, region string, config *Config) ([]ASGInfo, error) {
	asgNames := make(map[string]struct{})
	for _, instance := range instances {
		if instance.ASGName != "" {
			asgNames[instance.ASGName] = struct{}{}
		}
	}

	// If no ASG names found, return empty slice
	if len(asgNames) == 0 {
		return []ASGInfo{}, nil
	}

	// Convert map to slice for API call
	var asgNamesList []string
	for name := range asgNames {
		asgNamesList = append(asgNamesList, name)
	}

	var relatedASGs []ASGInfo

	// Process ASGs in batches to avoid API limits (max 50 ASG names per call)
	const maxASGsPerCall = 50
	for i := 0; i < len(asgNamesList); i += maxASGsPerCall {
		end := i + maxASGsPerCall
		if end > len(asgNamesList) {
			end = len(asgNamesList)
		}
		batch := asgNamesList[i:end]

		// Use pagination for ASG API calls
		var nextToken *string
		for {
			apiCtx, cancel := context.WithTimeout(ctx, config.APITimeout)
			output, err := asgClient.DescribeAutoScalingGroups(apiCtx, &autoscaling.DescribeAutoScalingGroupsInput{
				AutoScalingGroupNames: batch,
				NextToken:             nextToken,
				MaxRecords:            aws.Int32(100),
			})
			cancel()
			if err != nil {
				return nil, err
			}

			for _, asg := range output.AutoScalingGroups {
				if asg.AutoScalingGroupName == nil {
					log.Warn().Msg("ASG name is nil, skipping ASG")
					continue
				}

				var launchTemplate, launchConfig string

				if asg.LaunchTemplate != nil && asg.LaunchTemplate.LaunchTemplateId != nil {
					launchTemplate = *asg.LaunchTemplate.LaunchTemplateId
				}

				if asg.LaunchConfigurationName != nil {
					launchConfig = *asg.LaunchConfigurationName
				}

				relatedASGs = append(relatedASGs, ASGInfo{
					AccountID:           accountID,
					Region:              region,
					ASGName:             *asg.AutoScalingGroupName,
					LaunchTemplate:      launchTemplate,
					LaunchConfiguration: launchConfig,
				})
			}

			if output.NextToken == nil {
				break
			}
			nextToken = output.NextToken
		}
	}

	log.Info().Int("total_asgs", len(relatedASGs)).Str("region", region).Msg("Completed ASG discovery")
	return relatedASGs, nil
}

// SimpleProgressBar provides a clean, single-line progress display
type SimpleProgressBar struct {
	total       int
	current     int
	description string
	mutex       sync.Mutex
	lastPrint   time.Time
}

func NewSimpleProgressBar(total int, description string) *SimpleProgressBar {
	return &SimpleProgressBar{
		total:       total,
		current:     0,
		description: description,
		lastPrint:   time.Now(),
	}
}

func (p *SimpleProgressBar) Add(delta int) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.current += delta
	if p.current > p.total {
		p.current = p.total
	}

	// Throttle updates to avoid too frequent redraws
	if time.Since(p.lastPrint) > 200*time.Millisecond || p.current == p.total {
		p.render()
		p.lastPrint = time.Now()
	}
}

func (p *SimpleProgressBar) render() {
	percentage := float64(p.current) / float64(p.total) * 100
	width := 40
	filled := int(percentage / 100 * float64(width))

	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)

	// Clear the line and print progress
	fmt.Fprintf(os.Stderr, "\r%s %3.0f%% |%s| (%d/%d)",
		p.description, percentage, bar, p.current, p.total)
}

func (p *SimpleProgressBar) Finish() {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	// Clear the progress line
	fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", 80))
}

// writeCSVAtomic provides atomic CSV writing with transaction-like behavior
// It writes to a temporary file first, then renames it to the final name on success
func writeCSVAtomic(filename string, writeFunc func(*csv.Writer) error) error {
	// Create temporary file in the same directory as the target file
	dir := filepath.Dir(filename)
	if dir == "." {
		dir = ""
	}

	tempFile, err := os.CreateTemp(dir, filepath.Base(filename)+".tmp.*")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	tempFilename := tempFile.Name()

	// Ensure cleanup of temp file on any error
	defer func() {
		if tempFile != nil {
			tempFile.Close()
			os.Remove(tempFilename)
		}
	}()

	writer := csv.NewWriter(tempFile)

	// Execute the write function
	if err := writeFunc(writer); err != nil {
		return fmt.Errorf("CSV write function failed: %w", err)
	}

	// Flush and check for any write errors
	writer.Flush()
	if err := writer.Error(); err != nil {
		return fmt.Errorf("CSV writer flush failed: %w", err)
	}

	// Sync to ensure data is written to disk
	if err := tempFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync temporary file: %w", err)
	}

	// Close the temp file before rename
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("failed to close temporary file: %w", err)
	}
	tempFile = nil // Prevent defer cleanup

	// Atomic rename to final filename
	if err := os.Rename(tempFilename, filename); err != nil {
		os.Remove(tempFilename) // Manual cleanup since defer won't run
		return fmt.Errorf("failed to rename temporary file: %w", err)
	}

	log.Debug().Str("filename", filename).Msg("CSV file written atomically")
	return nil
}

func saveInstancesToCSV(instances []InstanceInfo, filename string) {
	if err := writeCSVAtomic(filename, func(writer *csv.Writer) error {
		// Write header
		if err := writer.Write([]string{"AccountID", "Region", "InstanceID", "Name", "Monitoring", "ASGName"}); err != nil {
			return fmt.Errorf("failed to write CSV header: %w", err)
		}

		// Stream write instances to reduce memory usage for large datasets
		const batchSize = 1000
		for i := 0; i < len(instances); i += batchSize {
			end := i + batchSize
			if end > len(instances) {
				end = len(instances)
			}

			// Write batch
			for j := i; j < end; j++ {
				instance := instances[j]
				if err := writer.Write([]string{
					instance.AccountID,
					instance.Region,
					instance.InstanceID,
					instance.Name,
					instance.MonitoringState,
					instance.ASGName,
				}); err != nil {
					return fmt.Errorf("failed to write CSV row: %w", err)
				}
			}

			// Flush periodically to free up write buffer
			writer.Flush()
			if err := writer.Error(); err != nil {
				return fmt.Errorf("CSV writer error: %w", err)
			}

			log.Debug().Int("written", end).Int("total", len(instances)).Msg("CSV batch written")
		}
		return nil
	}); err != nil {
		log.Fatal().Err(err).Str("filename", filename).Msg("Failed to save instances to CSV")
	}
}

func saveASGsToCSV(asgs map[string]ASGInfo, filename string) {
	if err := writeCSVAtomic(filename, func(writer *csv.Writer) error {
		// Write header
		if err := writer.Write([]string{"AccountID", "Region", "ASGName", "LaunchTemplate", "LaunchConfiguration"}); err != nil {
			return fmt.Errorf("failed to write CSV header: %w", err)
		}

		// Convert map to slice for batched processing
		asgList := make([]ASGInfo, 0, len(asgs))
		for _, asg := range asgs {
			asgList = append(asgList, asg)
		}

		// Stream write ASGs in batches
		const batchSize = 1000
		for i := 0; i < len(asgList); i += batchSize {
			end := i + batchSize
			if end > len(asgList) {
				end = len(asgList)
			}

			for j := i; j < end; j++ {
				asg := asgList[j]
				if err := writer.Write([]string{
					asg.AccountID,
					asg.Region,
					asg.ASGName,
					asg.LaunchTemplate,
					asg.LaunchConfiguration,
				}); err != nil {
					return fmt.Errorf("failed to write CSV row: %w", err)
				}
			}

			writer.Flush()
			if err := writer.Error(); err != nil {
				return fmt.Errorf("CSV writer error: %w", err)
			}
		}
		return nil
	}); err != nil {
		log.Fatal().Err(err).Str("filename", filename).Msg("Failed to save ASGs to CSV")
	}
}

func saveMapToCSV(data map[string]struct{}, filename, header string) {
	if err := writeCSVAtomic(filename, func(writer *csv.Writer) error {
		// Write header
		if err := writer.Write([]string{"AccountID", "Region", header}); err != nil {
			return fmt.Errorf("failed to write CSV header: %w", err)
		}

		for key := range data {
			parts := strings.Split(key, ",")
			if len(parts) == 3 {
				if err := writer.Write(parts); err != nil {
					return fmt.Errorf("failed to write CSV row: %w", err)
				}
			}
		}
		return nil
	}); err != nil {
		log.Fatal().Err(err).Str("filename", filename).Msg("Failed to save map to CSV")
	}
}

func disableMonitoring(ctx context.Context, ec2Client *ec2.Client, instances []InstanceInfo, accountID string, region string, config *Config) {
	var instanceIDs []string

	for _, instance := range instances {
		if instance.MonitoringState == string(ec2types.MonitoringStateEnabled) {
			instanceIDs = append(instanceIDs, instance.InstanceID)
		}
	}

	for i := 0; i < len(instanceIDs); i += config.BatchSize {
		end := i + config.BatchSize
		if end > len(instanceIDs) {
			end = len(instanceIDs)
		}
		batch := instanceIDs[i:end]

		apiCtx, cancel := context.WithTimeout(ctx, config.APITimeout)
		_, err := ec2Client.UnmonitorInstances(apiCtx, &ec2.UnmonitorInstancesInput{
			InstanceIds: batch,
		})
		cancel()
		if err != nil {
			var ae smithy.APIError
			if errors.As(err, &ae) {
				switch ae.ErrorCode() {
				case "UnauthorizedOperation":
					log.Printf("Error: AccountID: %s, Region: %s - You do not have permission to unmonitor instances.\n", accountID, region)
				case "InvalidInstanceID.NotFound":
					log.Printf("Error: AccountID: %s, Region: %s - Some instance IDs were not found: %s\n", accountID, region, err.Error())
				case "IncorrectInstanceState":
					log.Printf("Error: AccountID: %s, Region: %s - The instance is not in a valid state for this operation.\n", accountID, region)
				case "OptInRequired":
					log.Printf("Error: AccountID: %s, Region: %s - You are not subscribed to the service required to perform this action.\n", accountID, region)
				default:
					log.Printf("EC2 API Error: %s\n", ae.ErrorMessage())
				}
			} else if respErr := new(http.ResponseError); errors.As(err, &respErr) {
				log.Printf("HTTP Error: %s, Status Code: %d\n", respErr.Error(), respErr.HTTPStatusCode())
			} else {
				log.Printf("Unknown Error: %s\n", err.Error())
			}
		}
		log.Printf("AccountID: %s, Region: %s - Disabled detailed monitoring for instances: %v", accountID, region, batch)
	}
}
