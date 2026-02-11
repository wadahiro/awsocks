package ec2

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestInstanceManager_GetInstanceState(t *testing.T) {
	tests := []struct {
		name          string
		instanceID    string
		mockOutput    *DescribeInstancesOutput
		mockErr       error
		expectedState string
		expectedErr   string
	}{
		{
			name:       "running instance",
			instanceID: "i-12345",
			mockOutput: &DescribeInstancesOutput{
				Reservations: []Reservation{
					{
						Instances: []InstanceInfo{
							{
								InstanceId: "i-12345",
								State:      InstanceState{Name: "running"},
							},
						},
					},
				},
			},
			expectedState: "running",
		},
		{
			name:       "stopped instance",
			instanceID: "i-12345",
			mockOutput: &DescribeInstancesOutput{
				Reservations: []Reservation{
					{
						Instances: []InstanceInfo{
							{
								InstanceId: "i-12345",
								State:      InstanceState{Name: "stopped"},
							},
						},
					},
				},
			},
			expectedState: "stopped",
		},
		{
			name:       "instance not found",
			instanceID: "i-nonexistent",
			mockOutput: &DescribeInstancesOutput{
				Reservations: []Reservation{},
			},
			expectedErr: "instance not found",
		},
		{
			name:        "API error",
			instanceID:  "i-12345",
			mockErr:     errors.New("API error"),
			expectedErr: "failed to describe instance",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := new(MockEC2Client)
			mockClient.On("DescribeInstances", mock.Anything, mock.Anything).Return(tt.mockOutput, tt.mockErr)

			manager := NewInstanceManager(mockClient)
			state, err := manager.GetInstanceState(context.Background(), tt.instanceID)

			if tt.expectedErr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedState, state)
			}

			mockClient.AssertExpectations(t)
		})
	}
}

func TestInstanceManager_StartAndWait(t *testing.T) {
	t.Run("successful start", func(t *testing.T) {
		mockClient := new(MockEC2Client)

		// StartInstances succeeds
		mockClient.On("StartInstances", mock.Anything, mock.Anything).Return(
			&StartInstancesOutput{},
			nil,
		)

		// First call: pending, second call: running
		mockClient.On("DescribeInstances", mock.Anything, mock.Anything).Return(
			&DescribeInstancesOutput{
				Reservations: []Reservation{
					{
						Instances: []InstanceInfo{
							{
								InstanceId: "i-12345",
								State:      InstanceState{Name: "running"},
							},
						},
					},
				},
			},
			nil,
		)

		manager := NewInstanceManager(mockClient)
		err := manager.StartAndWait(context.Background(), "i-12345", 30*time.Second)

		require.NoError(t, err)
		mockClient.AssertExpectations(t)
	})

	t.Run("start API error", func(t *testing.T) {
		mockClient := new(MockEC2Client)
		mockClient.On("StartInstances", mock.Anything, mock.Anything).Return(
			nil,
			errors.New("cannot start instance"),
		)

		manager := NewInstanceManager(mockClient)
		err := manager.StartAndWait(context.Background(), "i-12345", 30*time.Second)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to start instance")
	})
}

func TestInstanceManager_Stop(t *testing.T) {
	t.Run("successful stop", func(t *testing.T) {
		mockClient := new(MockEC2Client)
		mockClient.On("StopInstances", mock.Anything, mock.Anything).Return(
			&StopInstancesOutput{},
			nil,
		)

		manager := NewInstanceManager(mockClient)
		err := manager.Stop(context.Background(), "i-12345")

		require.NoError(t, err)
		mockClient.AssertExpectations(t)
	})

	t.Run("stop API error", func(t *testing.T) {
		mockClient := new(MockEC2Client)
		mockClient.On("StopInstances", mock.Anything, mock.Anything).Return(
			nil,
			errors.New("cannot stop instance"),
		)

		manager := NewInstanceManager(mockClient)
		err := manager.Stop(context.Background(), "i-12345")

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to stop instance")
	})
}
