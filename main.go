package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticbeanstalk"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/elasticbeanstalk/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/transport/http"
)

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
			log.Fatal("No profiles specified for check. Usage: go run main.go check <profile1> [profile2] ...")
		}
	case "disable":
		disableCmd.Parse(os.Args[2:])
		profiles = disableCmd.Args()
		if len(profiles) == 0 {
			log.Fatal("No profiles specified for disable. Usage: go run main.go disable <profile1> [profile2] ...")
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
	var mutex sync.Mutex
	var wg sync.WaitGroup

	for _, profile := range profiles {
		wg.Add(1)
		go func(profile string) {
			defer wg.Done()

			cfg, err := config.LoadDefaultConfig(context.TODO(),
				config.WithSharedConfigProfile(profile),
			)
			if err != nil {
				log.Fatalf("unable to load SDK config for profile %s: %v", profile, err)
			}

			stsClient := sts.NewFromConfig(cfg)
			accountID, err := getAccountID(stsClient)
			if err != nil {
				log.Fatalf("unable to get account ID for profile %s: %v", profile, err)
			}

			baseEC2Client := ec2.NewFromConfig(cfg)
			regions, err := getRegions(baseEC2Client)
			if err != nil {
				log.Fatalf("unable to describe regions for profile %s: %v", profile, err)
			}

			for _, region := range regions {
				wg.Add(1)
				go func(region string) {
					defer wg.Done()
					fmt.Printf("Processing profile: %s, region: %s\n", profile, region)

					regionCfg := cfg.Copy()
					regionCfg.Region = region

					regionalEC2Client := ec2.NewFromConfig(regionCfg)
					regionalEbClient := elasticbeanstalk.NewFromConfig(regionCfg)
					cache := NewEnvironmentCache(regionalEbClient)
					instances, err := getInstances(regionalEC2Client, cache, regionCfg, accountID)
					if err != nil {
						log.Fatalf("unable to describe instances for profile %s in region %s: %v", profile, region, err)
					}

					asgClient := autoscaling.NewFromConfig(regionCfg)
					asgs, err := getRelatedAutoScalingGroups(asgClient, instances, accountID, region)
					if err != nil {
						log.Fatalf("unable to describe ASGs for profile %s in region %s: %v", profile, region, err)
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
						disableMonitoring(regionalEC2Client, instances, accountID, region)
					}
				}(region)
			}
		}(profile)
	}

	wg.Wait()

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

func getAccountID(client *sts.Client) (string, error) {
	output, err := client.GetCallerIdentity(context.TODO(), &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return *output.Account, nil
}

func getRegions(client *ec2.Client) ([]string, error) {
	output, err := client.DescribeRegions(context.TODO(), &ec2.DescribeRegionsInput{})
	if err != nil {
		return nil, err
	}

	var regions []string
	for _, region := range output.Regions {
		regions = append(regions, *region.RegionName)
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
		if strings.HasPrefix(*tag.Key, "elasticbeanstalk:environment-name") {
			environmentName = *tag.Value
			break
		}
	}

	if environmentName == "" {
		// log.Printf("Instance %s does not belong to an Elastic Beanstalk environment", *instance.InstanceId)
		return false, nil
	}

	// log.Printf("Fetching application name for environment: %s", environmentName)
	applicationName, err := cache.GetApplicationName(ctx, environmentName)
	if err != nil {
		// log.Printf("Error fetching application name: %v", err)
		return false, err
	}
	// log.Printf("Application name for environment %s: %s", environmentName, applicationName)

	// log.Printf("Fetching environment settings for application: %s, environment: %s", applicationName, environmentName)
	settings, err := cache.GetEnvironmentSettings(ctx, applicationName, environmentName)
	if err != nil {
		// log.Printf("Error fetching environment settings: %v", err)
		return false, err
	}
	// log.Printf("Settings for application %s, environment %s: %+v", applicationName, environmentName, settings)

	// Check monitoring setting
	found := false
	for _, option := range settings {
		// log.Printf("Option - Namespace: %s, OptionName: %s, ResourceName: %s, Value: %s",
		// 	aws.ToString(option.Namespace),
		// 	aws.ToString(option.OptionName),
		// 	aws.ToString(option.ResourceName),
		// 	aws.ToString(option.Value),
		// )
		if aws.ToString(option.Namespace) == "aws:elasticbeanstalk:healthreporting:system" &&
			aws.ToString(option.OptionName) == "SystemType" {
			// log.Printf("SystemType option found: %s", aws.ToString(option.Value))
			if aws.ToString(option.Value) == "enhanced" {
				return true, nil
			}
		}
	}

	if !found {
		// log.Printf("Monitoring option not found in settings for environment: %s", environmentName)
	}

	return false, nil
}

func getInstances(ec2Client *ec2.Client, cache *EnvironmentCache, cfg aws.Config, accountID string) ([]InstanceInfo, error) {
	output, err := ec2Client.DescribeInstances(context.TODO(), &ec2.DescribeInstancesInput{})
	if err != nil {
		return nil, err
	}

	var instances []InstanceInfo
	for _, reservation := range output.Reservations {
		for _, instance := range reservation.Instances {
			if instance.State != nil && instance.State.Name == ec2types.InstanceStateNameRunning {
				exclude, err := isExcludedInstance(context.TODO(), instance, cache)
				if err != nil {
					log.Printf("Error: AccountID: %s, Region: %s - %s\n", accountID, cfg.Region, err.Error())
					continue
				}
				if exclude {
					continue
				}

				var name, asgName string
				for _, tag := range instance.Tags {
					if *tag.Key == "aws:autoscaling:groupName" {
						asgName = *tag.Value
					}
					if *tag.Key == "Name" {
						name = *tag.Value
					}
				}

				if instance.Monitoring != nil && instance.Monitoring.State == ec2types.MonitoringStateEnabled {
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
	return instances, nil
}

func getRelatedAutoScalingGroups(asgClient *autoscaling.Client, instances []InstanceInfo, accountID, region string) ([]ASGInfo, error) {
	asgNames := make(map[string]struct{})
	for _, instance := range instances {
		if instance.ASGName != "" {
			asgNames[instance.ASGName] = struct{}{}
		}
	}

	output, err := asgClient.DescribeAutoScalingGroups(context.TODO(), &autoscaling.DescribeAutoScalingGroupsInput{})
	if err != nil {
		return nil, err
	}

	var relatedASGs []ASGInfo
	for _, asg := range output.AutoScalingGroups {
		if _, exists := asgNames[*asg.AutoScalingGroupName]; exists {
			var launchTemplate, launchConfig string

			if asg.LaunchTemplate != nil {
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
	}
	return relatedASGs, nil
}

func saveInstancesToCSV(instances []InstanceInfo, filename string) {
	file, err := os.Create(filename)
	if err != nil {
		log.Fatal("Could not create CSV file", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	writer.Write([]string{"AccountID", "Region", "InstanceID", "Name", "Monitoring", "ASGName"})

	for _, instance := range instances {
		writer.Write([]string{
			instance.AccountID,
			instance.Region,
			instance.InstanceID,
			instance.Name,
			instance.MonitoringState,
			instance.ASGName,
		})
	}
}

func saveASGsToCSV(asgs map[string]ASGInfo, filename string) {
	file, err := os.Create(filename)
	if err != nil {
		log.Fatal("Could not create CSV file", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	writer.Write([]string{"AccountID", "Region", "ASGName", "LaunchTemplate", "LaunchConfiguration"})

	for _, asg := range asgs {
		writer.Write([]string{
			asg.AccountID,
			asg.Region,
			asg.ASGName,
			asg.LaunchTemplate,
			asg.LaunchConfiguration,
		})
	}
}

func saveMapToCSV(data map[string]struct{}, filename, header string) {
	file, err := os.Create(filename)
	if err != nil {
		log.Fatal("Could not create CSV file", err)
	}
	defer file.Close()

	_, err = file.WriteString(fmt.Sprintf("AccountID,Region,%s\n", header))
	if err != nil {
		log.Fatal("Could not write CSV header", err)
	}

	for key := range data {
		_, err := file.WriteString(fmt.Sprintf("%s\n", key))
		if err != nil {
			log.Fatal("Could not write CSV data", err)
		}
	}
}

func disableMonitoring(ec2Client *ec2.Client, instances []InstanceInfo, accountID string, region string) {
	var instanceIDs []string

	for _, instance := range instances {
		if instance.MonitoringState == string(ec2types.MonitoringStateEnabled) {
			instanceIDs = append(instanceIDs, instance.InstanceID)
		}
	}

	const batchSize = 20
	for i := 0; i < len(instanceIDs); i += batchSize {
		end := i + batchSize
		if end > len(instanceIDs) {
			end = len(instanceIDs)
		}
		batch := instanceIDs[i:end]

		_, err := ec2Client.UnmonitorInstances(context.TODO(), &ec2.UnmonitorInstancesInput{
			InstanceIds: batch,
		})
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
