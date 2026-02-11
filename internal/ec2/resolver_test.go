package ec2

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type MockEC2Client struct {
	mock.Mock
}

func (m *MockEC2Client) DescribeInstances(ctx context.Context, params *DescribeInstancesInput) (*DescribeInstancesOutput, error) {
	args := m.Called(ctx, params)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*DescribeInstancesOutput), args.Error(1)
}

func (m *MockEC2Client) StartInstances(ctx context.Context, params *StartInstancesInput) (*StartInstancesOutput, error) {
	args := m.Called(ctx, params)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*StartInstancesOutput), args.Error(1)
}

func (m *MockEC2Client) StopInstances(ctx context.Context, params *StopInstancesInput) (*StopInstancesOutput, error) {
	args := m.Called(ctx, params)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*StopInstancesOutput), args.Error(1)
}

func TestResolveByName_SingleMatch(t *testing.T) {
	mockClient := new(MockEC2Client)
	mockClient.On("DescribeInstances", mock.Anything, mock.Anything).Return(
		&DescribeInstancesOutput{
			Reservations: []Reservation{
				{
					Instances: []InstanceInfo{
						{
							InstanceId: "i-12345",
							State:      InstanceState{Name: "running"},
							Tags: []Tag{
								{Key: "Name", Value: "my-server"},
							},
						},
					},
				},
			},
		},
		nil,
	)

	resolver := NewResolver(mockClient)
	instances, err := resolver.ResolveByName(context.Background(), "my-server")

	require.NoError(t, err)
	assert.Len(t, instances, 1)
	assert.Equal(t, "i-12345", instances[0].ID)
	assert.Equal(t, "my-server", instances[0].Name)
	assert.Equal(t, "running", instances[0].State)

	mockClient.AssertExpectations(t)
}

func TestResolveByName_MultipleMatches(t *testing.T) {
	mockClient := new(MockEC2Client)
	mockClient.On("DescribeInstances", mock.Anything, mock.Anything).Return(
		&DescribeInstancesOutput{
			Reservations: []Reservation{
				{
					Instances: []InstanceInfo{
						{
							InstanceId: "i-11111",
							State:      InstanceState{Name: "running"},
							Tags: []Tag{
								{Key: "Name", Value: "web-server-1"},
							},
						},
						{
							InstanceId: "i-22222",
							State:      InstanceState{Name: "running"},
							Tags: []Tag{
								{Key: "Name", Value: "web-server-2"},
							},
						},
					},
				},
			},
		},
		nil,
	)

	resolver := NewResolver(mockClient)
	instances, err := resolver.ResolveByName(context.Background(), "web-*")

	require.NoError(t, err)
	assert.Len(t, instances, 2)
	assert.Equal(t, "i-11111", instances[0].ID)
	assert.Equal(t, "web-server-1", instances[0].Name)
	assert.Equal(t, "i-22222", instances[1].ID)
	assert.Equal(t, "web-server-2", instances[1].Name)

	mockClient.AssertExpectations(t)
}

func TestResolveByName_NoMatch(t *testing.T) {
	mockClient := new(MockEC2Client)
	mockClient.On("DescribeInstances", mock.Anything, mock.Anything).Return(
		&DescribeInstancesOutput{
			Reservations: []Reservation{},
		},
		nil,
	)

	resolver := NewResolver(mockClient)
	instances, err := resolver.ResolveByName(context.Background(), "nonexistent")

	assert.Error(t, err)
	assert.Nil(t, instances)
	assert.Contains(t, err.Error(), "no instances found")

	mockClient.AssertExpectations(t)
}

func TestResolveByName_APIError(t *testing.T) {
	mockClient := new(MockEC2Client)
	mockClient.On("DescribeInstances", mock.Anything, mock.Anything).Return(
		nil,
		errors.New("API error"),
	)

	resolver := NewResolver(mockClient)
	instances, err := resolver.ResolveByName(context.Background(), "my-server")

	assert.Error(t, err)
	assert.Nil(t, instances)
	assert.Contains(t, err.Error(), "failed to describe instances")

	mockClient.AssertExpectations(t)
}

func TestResolveByName_MultipleReservations(t *testing.T) {
	mockClient := new(MockEC2Client)
	mockClient.On("DescribeInstances", mock.Anything, mock.Anything).Return(
		&DescribeInstancesOutput{
			Reservations: []Reservation{
				{
					Instances: []InstanceInfo{
						{
							InstanceId: "i-aaaa",
							State:      InstanceState{Name: "running"},
							Tags: []Tag{
								{Key: "Name", Value: "server-a"},
							},
						},
					},
				},
				{
					Instances: []InstanceInfo{
						{
							InstanceId: "i-bbbb",
							State:      InstanceState{Name: "running"},
							Tags: []Tag{
								{Key: "Name", Value: "server-b"},
							},
						},
					},
				},
			},
		},
		nil,
	)

	resolver := NewResolver(mockClient)
	instances, err := resolver.ResolveByName(context.Background(), "server-*")

	require.NoError(t, err)
	assert.Len(t, instances, 2)

	mockClient.AssertExpectations(t)
}
