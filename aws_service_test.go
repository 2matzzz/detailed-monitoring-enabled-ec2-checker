package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticbeanstalk"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Mock implementations for testing
type mockEC2Client struct {
	describeInstancesFunc  func(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	describeRegionsFunc    func(ctx context.Context, params *ec2.DescribeRegionsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error)
	unmonitorInstancesFunc func(ctx context.Context, params *ec2.UnmonitorInstancesInput, optFns ...func(*ec2.Options)) (*ec2.UnmonitorInstancesOutput, error)
}

func (m *mockEC2Client) DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if m.describeInstancesFunc != nil {
		return m.describeInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.DescribeInstancesOutput{}, nil
}

func (m *mockEC2Client) DescribeRegions(ctx context.Context, params *ec2.DescribeRegionsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error) {
	if m.describeRegionsFunc != nil {
		return m.describeRegionsFunc(ctx, params, optFns...)
	}
	return &ec2.DescribeRegionsOutput{
		Regions: []ec2types.Region{
			{RegionName: aws.String("us-east-1")},
			{RegionName: aws.String("us-west-2")},
		},
	}, nil
}

func (m *mockEC2Client) UnmonitorInstances(ctx context.Context, params *ec2.UnmonitorInstancesInput, optFns ...func(*ec2.Options)) (*ec2.UnmonitorInstancesOutput, error) {
	if m.unmonitorInstancesFunc != nil {
		return m.unmonitorInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.UnmonitorInstancesOutput{}, nil
}

type mockASGClient struct {
	describeASGsFunc func(ctx context.Context, params *autoscaling.DescribeAutoScalingGroupsInput, optFns ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
}

func (m *mockASGClient) DescribeAutoScalingGroups(ctx context.Context, params *autoscaling.DescribeAutoScalingGroupsInput, optFns ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	if m.describeASGsFunc != nil {
		return m.describeASGsFunc(ctx, params, optFns...)
	}
	return &autoscaling.DescribeAutoScalingGroupsOutput{}, nil
}

type mockSTSClient struct {
	getCallerIdentityFunc func(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

func (m *mockSTSClient) GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	if m.getCallerIdentityFunc != nil {
		return m.getCallerIdentityFunc(ctx, params, optFns...)
	}
	return &sts.GetCallerIdentityOutput{
		Account: aws.String("123456789012"),
	}, nil
}

type mockEBClient struct{}

func (m *mockEBClient) DescribeEnvironments(ctx context.Context, params *elasticbeanstalk.DescribeEnvironmentsInput, optFns ...func(*elasticbeanstalk.Options)) (*elasticbeanstalk.DescribeEnvironmentsOutput, error) {
	return &elasticbeanstalk.DescribeEnvironmentsOutput{}, nil
}

func (m *mockEBClient) DescribeConfigurationSettings(ctx context.Context, params *elasticbeanstalk.DescribeConfigurationSettingsInput, optFns ...func(*elasticbeanstalk.Options)) (*elasticbeanstalk.DescribeConfigurationSettingsOutput, error) {
	return &elasticbeanstalk.DescribeConfigurationSettingsOutput{}, nil
}

func TestAWSService_GetAccountID(t *testing.T) {
	tests := []struct {
		name    string
		mockSTS *mockSTSClient
		want    string
		wantErr bool
	}{
		{
			name: "successful get account ID",
			mockSTS: &mockSTSClient{
				getCallerIdentityFunc: func(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
					return &sts.GetCallerIdentityOutput{
						Account: aws.String("123456789012"),
					}, nil
				},
			},
			want:    "123456789012",
			wantErr: false,
		},
		{
			name: "API error",
			mockSTS: &mockSTSClient{
				getCallerIdentityFunc: func(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
					return nil, errors.New("API error")
				},
			},
			want:    "",
			wantErr: true,
		},
		{
			name: "nil account ID",
			mockSTS: &mockSTSClient{
				getCallerIdentityFunc: func(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
					return &sts.GetCallerIdentityOutput{
						Account: nil,
					}, nil
				},
			},
			want:    "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := NewDefaultConfig()
			config.APITimeout = 5 * time.Second

			service := NewAWSService(&mockEC2Client{}, &mockASGClient{}, tt.mockSTS, &mockEBClient{}, config)

			ctx := context.Background()
			got, err := service.GetAccountID(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("AWSService.GetAccountID() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if got != tt.want {
				t.Errorf("AWSService.GetAccountID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAWSService_GetRegions(t *testing.T) {
	tests := []struct {
		name    string
		mockEC2 *mockEC2Client
		want    []string
		wantErr bool
	}{
		{
			name: "successful get regions",
			mockEC2: &mockEC2Client{
				describeRegionsFunc: func(ctx context.Context, params *ec2.DescribeRegionsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error) {
					return &ec2.DescribeRegionsOutput{
						Regions: []ec2types.Region{
							{RegionName: aws.String("us-east-1")},
							{RegionName: aws.String("us-west-2")},
						},
					}, nil
				},
			},
			want:    []string{"us-east-1", "us-west-2"},
			wantErr: false,
		},
		{
			name: "API error",
			mockEC2: &mockEC2Client{
				describeRegionsFunc: func(ctx context.Context, params *ec2.DescribeRegionsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error) {
					return nil, errors.New("API error")
				},
			},
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := NewDefaultConfig()
			config.APITimeout = 5 * time.Second

			service := NewAWSService(tt.mockEC2, &mockASGClient{}, &mockSTSClient{}, &mockEBClient{}, config)

			ctx := context.Background()
			got, err := service.GetRegions(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("AWSService.GetRegions() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if len(got) != len(tt.want) {
				t.Errorf("AWSService.GetRegions() = %v, want %v", got, tt.want)
				return
			}

			for i, region := range got {
				if region != tt.want[i] {
					t.Errorf("AWSService.GetRegions()[%d] = %v, want %v", i, region, tt.want[i])
				}
			}
		})
	}
}

func TestRetryStrategy_Execute(t *testing.T) {
	tests := []struct {
		name      string
		operation func() error
		wantErr   bool
		attempts  int
	}{
		{
			name: "success on first attempt",
			operation: func() error {
				return nil
			},
			wantErr:  false,
			attempts: 1,
		},
		{
			name: "success after retry",
			operation: func() func() error {
				attempt := 0
				return func() error {
					attempt++
					if attempt < 2 {
						return errors.New("temporary error")
					}
					return nil
				}
			}(),
			wantErr:  false,
			attempts: 2,
		},
		{
			name: "permanent failure",
			operation: func() error {
				return errors.New("permanent error")
			},
			wantErr:  true,
			attempts: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retry := NewRetryStrategy(3)
			retry.BaseDelay = 1 * time.Millisecond // Fast test

			ctx := context.Background()
			err := retry.Execute(ctx, tt.operation)

			if (err != nil) != tt.wantErr {
				t.Errorf("RetryStrategy.Execute() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
