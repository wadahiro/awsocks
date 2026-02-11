package ec2

import (
	"context"
	"fmt"
)

// Instance represents an EC2 instance
type Instance struct {
	ID    string
	Name  string
	State string
}

// Resolver resolves EC2 instances by name
type Resolver struct {
	client Client
}

// NewResolver creates a new EC2 resolver
func NewResolver(client Client) *Resolver {
	return &Resolver{client: client}
}

// ResolveByName searches EC2 instances by Name tag
// Returns:
// - Single match: returns instance ID directly
// - Multiple matches: returns list for interactive selection
// - No match: returns error
func (r *Resolver) ResolveByName(ctx context.Context, namePattern string) ([]Instance, error) {
	input := &DescribeInstancesInput{
		Filters: []Filter{
			{
				Name:   "tag:Name",
				Values: []string{namePattern},
			},
			{
				Name:   "instance-state-name",
				Values: []string{"running", "stopped"},
			},
		},
	}

	result, err := r.client.DescribeInstances(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to describe instances: %w", err)
	}

	var instances []Instance
	for _, reservation := range result.Reservations {
		for _, inst := range reservation.Instances {
			name := ""
			for _, tag := range inst.Tags {
				if tag.Key == "Name" {
					name = tag.Value
					break
				}
			}
			instances = append(instances, Instance{
				ID:    inst.InstanceId,
				Name:  name,
				State: inst.State.Name,
			})
		}
	}

	if len(instances) == 0 {
		return nil, fmt.Errorf("no instances found with Name tag: %s", namePattern)
	}

	return instances, nil
}
