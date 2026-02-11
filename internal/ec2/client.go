package ec2

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// Client abstracts EC2 API for testing
type Client interface {
	DescribeInstances(ctx context.Context, params *DescribeInstancesInput) (*DescribeInstancesOutput, error)
	StartInstances(ctx context.Context, params *StartInstancesInput) (*StartInstancesOutput, error)
	StopInstances(ctx context.Context, params *StopInstancesInput) (*StopInstancesOutput, error)
}

// NewClient creates a real EC2 client using direct HTTP
func NewClient(cfg aws.Config) Client {
	return NewHTTPClient(cfg)
}
