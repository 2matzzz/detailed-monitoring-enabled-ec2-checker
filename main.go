package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
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

func main() {
	if len(os.Args) < 2 {
		log.Fatal("No profiles specified. Usage: go run main.go <profile1> [profile2] ...")
	}

	profiles := os.Args[1:]

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
			fmt.Println("Processing profile:", profile)

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

			ec2Client := ec2.NewFromConfig(cfg)
			regions, err := getRegions(ec2Client)
			if err != nil {
				log.Fatalf("unable to describe regions for profile %s: %v", profile, err)
			}

			for _, region := range regions {
				wg.Add(1)
				go func(region string) {
					defer wg.Done()
					fmt.Println("Processing region:", region)

					regionCfg := cfg.Copy()
					regionCfg.Region = region

					ec2Client = ec2.NewFromConfig(regionCfg)
					instances, err := getInstances(ec2Client, regionCfg, accountID)
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
				}(region)
			}
		}(profile)
	}

	wg.Wait()

	saveInstancesToCSV(allInstances, "detailed-monitoring-enabled-ec2-list.csv")

	saveASGsToCSV(allASGs, "asg-list.csv")

	saveMapToCSV(launchTemplates, "launch-template-list.csv", "LaunchTemplate")

	saveMapToCSV(launchConfigurations, "launch-configuration-list.csv", "LaunchConfiguration")

	fmt.Println("CSV export completed successfully.")
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

func getInstances(ec2Client *ec2.Client, cfg aws.Config, accountID string) ([]InstanceInfo, error) {
	output, err := ec2Client.DescribeInstances(context.TODO(), &ec2.DescribeInstancesInput{})
	if err != nil {
		return nil, err
	}

	var instances []InstanceInfo
	for _, reservation := range output.Reservations {
		for _, instance := range reservation.Instances {
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
