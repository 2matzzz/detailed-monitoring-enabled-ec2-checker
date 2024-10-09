package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type InstanceInfo struct {
	AccountID       string
	Region          string
	InstanceID      string
	Name            string
	MonitoringState string
}

type byAccountID []InstanceInfo

func (a byAccountID) Len() int           { return len(a) }
func (a byAccountID) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byAccountID) Less(i, j int) bool { return a[i].AccountID < a[j].AccountID }

func main() {
	if len(os.Args) < 2 {
		log.Fatal("No profiles specified. Usage: go run main.go <profile1> [profile2] ...")
	}

	profiles := os.Args[1:]

	var allInstances []InstanceInfo
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
					instances, err := getInstances(ec2Client)
					if err != nil {
						log.Fatalf("unable to describe instances for profile %s in region %s: %v", profile, region, err)
					}

					mutex.Lock()
					for _, instance := range instances {
						allInstances = append(allInstances, InstanceInfo{
							AccountID:       accountID,
							Region:          region,
							InstanceID:      instance.InstanceID,
							Name:            instance.Name,
							MonitoringState: instance.MonitoringState,
						})
					}
					mutex.Unlock()
				}(region)
			}
		}(profile)
	}

	wg.Wait()

	sort.Sort(byAccountID(allInstances))

	file, err := os.Create("detailed-monitoring-enabled-ec2-list.csv")
	if err != nil {
		log.Fatal("Could not create CSV file", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	writer.Write([]string{"AccountID", "Region", "InstanceID", "Name", "Monitoring"})

	for _, instance := range allInstances {
		writer.Write([]string{instance.AccountID, instance.Region, instance.InstanceID, instance.Name, instance.MonitoringState})
	}

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

func getInstances(client *ec2.Client) ([]InstanceInfo, error) {
	output, err := client.DescribeInstances(context.TODO(), &ec2.DescribeInstancesInput{})
	if err != nil {
		return nil, err
	}

	var instances []InstanceInfo
	for _, reservation := range output.Reservations {
		for _, instance := range reservation.Instances {
			if instance.Monitoring != nil && instance.Monitoring.State == "enabled" {
				var name string
				for _, tag := range instance.Tags {
					if *tag.Key == "Name" {
						name = *tag.Value
						break
					}
				}
				instances = append(instances, InstanceInfo{
					InstanceID:      *instance.InstanceId,
					Name:            name,
					MonitoringState: string(instance.Monitoring.State),
				})
			}
		}
	}
	return instances, nil
}
