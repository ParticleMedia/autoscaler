/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aws

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestBuildAsg(t *testing.T) {
	asgCache := &asgCache{}

	asg, err := asgCache.buildAsgFromSpec("1:5:test-asg")
	assert.NoError(t, err)
	assert.Equal(t, asg.minSize, 1)
	assert.Equal(t, asg.maxSize, 5)
	assert.Equal(t, asg.Name, "test-asg")

	_, err = asgCache.buildAsgFromSpec("a")
	assert.Error(t, err)
	_, err = asgCache.buildAsgFromSpec("a:b:c")
	assert.Error(t, err)
	_, err = asgCache.buildAsgFromSpec("1:")
	assert.Error(t, err)
	_, err = asgCache.buildAsgFromSpec("1:2:")
	assert.Error(t, err)
}

func validateAsg(t *testing.T, asg *asg, name string, minSize int, maxSize int) {
	assert.Equal(t, name, asg.Name)
	assert.Equal(t, minSize, asg.minSize)
	assert.Equal(t, maxSize, asg.maxSize)
}

func TestCreatePlaceholders(t *testing.T) {
	registeredAsgName := aws.String("test-asg")
	registeredAsgRef := AwsRef{Name: *registeredAsgName}

	cases := []struct {
		name                string
		desiredCapacity     *int32
		activities          []autoscalingtypes.Activity
		groupLastUpdateTime time.Time
		describeErr         error
		asgToCheck          *string
		vpcZoneIdentifier   *string
		// failureIgnored marks cases where a failed activity must not make the ASG unavailable.
		failureIgnored bool
	}{
		{
			name:            "add placeholders successful",
			desiredCapacity: aws.Int32(10),
		},
		{
			name:            "no placeholders needed",
			desiredCapacity: aws.Int32(0),
		},
		{
			name:            "DescribeScalingActivities failed",
			desiredCapacity: aws.Int32(1),
			describeErr:     errors.New("timeout"),
		},
		{
			name:            "early abort if AWS scaling up fails",
			desiredCapacity: aws.Int32(1),
			activities: []autoscalingtypes.Activity{
				{
					StatusCode: autoscalingtypes.ScalingActivityStatusCodeFailed,
					StartTime:  aws.Time(time.Unix(10, 0)),
				},
			},
			groupLastUpdateTime: time.Unix(9, 0),
		},
		{
			name:            "AWS scaling failed event before CA scale_up",
			desiredCapacity: aws.Int32(1),
			activities: []autoscalingtypes.Activity{
				{
					StatusCode: autoscalingtypes.ScalingActivityStatusCodeFailed,
					StartTime:  aws.Time(time.Unix(9, 0)),
				},
			},
			groupLastUpdateTime: time.Unix(10, 0),
		},
		{
			name:            "asg not registered",
			desiredCapacity: aws.Int32(10),
			activities: []autoscalingtypes.Activity{
				{
					StatusCode: autoscalingtypes.ScalingActivityStatusCodeFailed,
					StartTime:  aws.Time(time.Unix(10, 0)),
				},
			},
			groupLastUpdateTime: time.Unix(9, 0),
			asgToCheck:          aws.String("unregisteredAsgName"),
		},
		{
			name:            "subnet IP exhaustion ignored when ASG spans multiple subnets",
			desiredCapacity: aws.Int32(1),
			activities: []autoscalingtypes.Activity{
				{
					StatusCode:    autoscalingtypes.ScalingActivityStatusCodeFailed,
					StatusMessage: aws.String("Could not launch On-Demand Instances. InsufficientFreeAddressesInSubnet - There are not enough free addresses in subnet 'subnet-b' to satisfy the requested number of instances. Launching EC2 instance failed."),
					StartTime:     aws.Time(time.Unix(10, 0)),
				},
			},
			groupLastUpdateTime: time.Unix(9, 0),
			vpcZoneIdentifier:   aws.String("subnet-a,subnet-b,subnet-c"),
			failureIgnored:      true,
		},
		{
			name:            "subnet IP exhaustion not ignored when ASG has a single subnet",
			desiredCapacity: aws.Int32(1),
			activities: []autoscalingtypes.Activity{
				{
					StatusCode:    autoscalingtypes.ScalingActivityStatusCodeFailed,
					StatusMessage: aws.String("Could not launch On-Demand Instances. InsufficientFreeAddressesInSubnet - There are not enough free addresses in subnet 'subnet-a'."),
					StartTime:     aws.Time(time.Unix(10, 0)),
				},
			},
			groupLastUpdateTime: time.Unix(9, 0),
			vpcZoneIdentifier:   aws.String("subnet-a"),
		},
		{
			name:            "non subnet-scoped failure not ignored with multiple subnets",
			desiredCapacity: aws.Int32(1),
			activities: []autoscalingtypes.Activity{
				{
					StatusCode:    autoscalingtypes.ScalingActivityStatusCodeFailed,
					StatusMessage: aws.String("Could not launch On-Demand Instances. InsufficientInstanceCapacity - We currently do not have sufficient capacity."),
					StartTime:     aws.Time(time.Unix(10, 0)),
				},
			},
			groupLastUpdateTime: time.Unix(9, 0),
			vpcZoneIdentifier:   aws.String("subnet-a,subnet-b"),
		},
		{
			name:            "other failure after subnet IP exhaustion still detected",
			desiredCapacity: aws.Int32(1),
			activities: []autoscalingtypes.Activity{
				{
					StatusCode:    autoscalingtypes.ScalingActivityStatusCodeFailed,
					StatusMessage: aws.String("InsufficientFreeAddressesInSubnet - There are not enough free addresses in subnet 'subnet-b'."),
					StartTime:     aws.Time(time.Unix(11, 0)),
				},
				{
					StatusCode:    autoscalingtypes.ScalingActivityStatusCodeFailed,
					StatusMessage: aws.String("InsufficientInstanceCapacity - We currently do not have sufficient capacity."),
					StartTime:     aws.Time(time.Unix(10, 0)),
				},
			},
			groupLastUpdateTime: time.Unix(9, 0),
			vpcZoneIdentifier:   aws.String("subnet-a,subnet-b"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shouldCallDescribeScalingActivities := true
			if *tc.desiredCapacity == int32(0) {
				shouldCallDescribeScalingActivities = false
			}

			asgName := registeredAsgName
			if tc.asgToCheck != nil {
				asgName = tc.asgToCheck
			}

			a := &autoScalingMock{}
			if shouldCallDescribeScalingActivities {
				a.On("DescribeScalingActivities",
					mock.Anything,
					&autoscaling.DescribeScalingActivitiesInput{
						AutoScalingGroupName: asgName,
					},
				).Return(
					&autoscaling.DescribeScalingActivitiesOutput{Activities: tc.activities},
					tc.describeErr,
				).Once()
			}

			asgCache := &asgCache{
				awsService: &awsWrapper{
					autoScalingI: a,
					ec2I:         nil,
				},
				registeredAsgs: map[AwsRef]*asg{
					registeredAsgRef: {
						AwsRef:         registeredAsgRef,
						lastUpdateTime: tc.groupLastUpdateTime,
					},
				},
			}

			groups := []autoscalingtypes.AutoScalingGroup{
				{
					AutoScalingGroupName: asgName,
					AvailabilityZones:    []string{"westeros-1a"},
					DesiredCapacity:      tc.desiredCapacity,
					Instances:            []autoscalingtypes.Instance{},
					VPCZoneIdentifier:    tc.vpcZoneIdentifier,
				},
			}
			groups = asgCache.createPlaceholdersForDesiredNonStartedInstances(groups)
			assert.Equal(t, int32(len(groups[0].Instances)), *tc.desiredCapacity)
			if tc.activities != nil && tc.activities[0].StatusCode == autoscalingtypes.ScalingActivityStatusCodeFailed && tc.activities[0].StartTime.After(tc.groupLastUpdateTime) && asgName == registeredAsgName && !tc.failureIgnored {
				assert.Equal(t, *groups[0].Instances[0].HealthStatus, placeholderUnfulfillableStatus)
			} else if len(groups[0].Instances) > 0 {
				assert.Equal(t, *groups[0].Instances[0].HealthStatus, "")
			}
			a.AssertExpectations(t)
		})
	}
}
