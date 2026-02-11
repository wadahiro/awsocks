package ec2

import (
	"context"
	"fmt"
	"time"
)

// InstanceManager manages EC2 instance lifecycle
type InstanceManager struct {
	client Client
}

// NewInstanceManager creates a new instance manager
func NewInstanceManager(client Client) *InstanceManager {
	return &InstanceManager{client: client}
}

// GetInstanceState returns the current state of an instance
func (m *InstanceManager) GetInstanceState(ctx context.Context, instanceID string) (string, error) {
	input := &DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}

	result, err := m.client.DescribeInstances(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to describe instance: %w", err)
	}

	if len(result.Reservations) == 0 || len(result.Reservations[0].Instances) == 0 {
		return "", fmt.Errorf("instance not found: %s", instanceID)
	}

	return result.Reservations[0].Instances[0].State.Name, nil
}

// StartAndWait starts an instance and waits for it to be running
func (m *InstanceManager) StartAndWait(ctx context.Context, instanceID string, timeout time.Duration) error {
	// Start the instance
	startInput := &StartInstancesInput{
		InstanceIds: []string{instanceID},
	}

	_, err := m.client.StartInstances(ctx, startInput)
	if err != nil {
		return fmt.Errorf("failed to start instance: %w", err)
	}

	// Wait for running state
	return m.waitForState(ctx, instanceID, "running", timeout)
}

// Stop stops an instance
func (m *InstanceManager) Stop(ctx context.Context, instanceID string) error {
	input := &StopInstancesInput{
		InstanceIds: []string{instanceID},
	}

	_, err := m.client.StopInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to stop instance: %w", err)
	}

	return nil
}

// waitForState waits for an instance to reach the specified state
func (m *InstanceManager) waitForState(ctx context.Context, instanceID string, targetState string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("timeout waiting for instance %s to reach state %s", instanceID, targetState)
			}

			state, err := m.GetInstanceState(ctx, instanceID)
			if err != nil {
				return err
			}

			if state == targetState {
				return nil
			}

			fmt.Printf("[ec2] Instance %s is %s, waiting for %s...\n", instanceID, state, targetState)
		}
	}
}
