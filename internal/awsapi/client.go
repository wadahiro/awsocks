// Package awsapi provides a unified AWS API client for EC2 operations and SSM backend creation.
// It centralizes all AWS API interactions and supports custom network routes (e.g., via VM NAT).
package awsapi

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ssmbackend "github.com/wadahiro/awsocks/internal/backend/ssm"
	"github.com/wadahiro/awsocks/internal/backend/ssm/datachannel"
	ec2pkg "github.com/wadahiro/awsocks/internal/ec2"
	"github.com/wadahiro/awsocks/internal/log"
)

var logger = log.For(log.ComponentEC2)

// DialContextFunc is a function that dials a network connection.
type DialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Client provides unified EC2 and SSM operations with an optional custom dial function.
type Client struct {
	ec2Client ec2pkg.Client
	ssmClient ssmbackend.SSMClient
	awsCfg    aws.Config
	dialFn    DialContextFunc // nil = direct (standard net.Dialer)
}

// NewClient creates a new unified AWS API client.
// If dialFn is nil, standard network connections are used.
func NewClient(awsCfg aws.Config, dialFn DialContextFunc) *Client {
	var ec2Opts []ec2pkg.HTTPClientOption
	var ssmOpts []ssmbackend.HTTPClientOption

	if dialFn != nil {
		transport := &http.Transport{
			DialContext: dialFn,
		}
		ec2Opts = append(ec2Opts, ec2pkg.WithTransport(transport))
		ssmOpts = append(ssmOpts, ssmbackend.WithTransport(transport))
	}

	return &Client{
		ec2Client: ec2pkg.NewHTTPClient(awsCfg, ec2Opts...),
		ssmClient: ssmbackend.NewHTTPClient(awsCfg, ssmOpts...),
		awsCfg:    awsCfg,
		dialFn:    dialFn,
	}
}

// EC2Client returns the underlying EC2 client.
func (c *Client) EC2Client() ec2pkg.Client {
	return c.ec2Client
}

// ResolveInstanceByName resolves an EC2 instance by Name tag.
// Returns instance ID, state, and error.
func (c *Client) ResolveInstanceByName(ctx context.Context, name string) (string, string, error) {
	resolver := ec2pkg.NewResolver(c.ec2Client)
	instances, err := resolver.ResolveByName(ctx, name)
	if err != nil {
		return "", "", err
	}
	if len(instances) == 0 {
		return "", "", fmt.Errorf("no instances found with name '%s'", name)
	}
	// Use the first instance
	return instances[0].ID, instances[0].State, nil
}

// GetInstanceState returns the current state of an EC2 instance.
func (c *Client) GetInstanceState(ctx context.Context, instanceID string) (string, error) {
	instMgr := ec2pkg.NewInstanceManager(c.ec2Client)
	return instMgr.GetInstanceState(ctx, instanceID)
}

// StartInstanceAndWait starts an EC2 instance and waits for it to reach running state.
func (c *Client) StartInstanceAndWait(ctx context.Context, instanceID string, timeout time.Duration) error {
	instMgr := ec2pkg.NewInstanceManager(c.ec2Client)
	return instMgr.StartAndWait(ctx, instanceID, timeout)
}

// WaitForInstanceState waits for an EC2 instance to reach the specified state.
func (c *Client) WaitForInstanceState(ctx context.Context, instanceID string, targetState string, timeout time.Duration) error {
	instMgr := ec2pkg.NewInstanceManager(c.ec2Client)
	return instMgr.WaitForState(ctx, instanceID, targetState, timeout)
}

// StopInstance stops an EC2 instance.
func (c *Client) StopInstance(ctx context.Context, instanceID string) error {
	instMgr := ec2pkg.NewInstanceManager(c.ec2Client)
	return instMgr.Stop(ctx, instanceID)
}

// WaitForSSMAgent polls until the SSM agent on the instance is online.
func (c *Client) WaitForSSMAgent(ctx context.Context, instanceID string, timeout time.Duration) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// Check immediately
	output, err := c.ssmClient.DescribeInstanceInformation(ctx, &ssmbackend.DescribeInstanceInformationInput{
		InstanceID: instanceID,
	})
	if err == nil && output.PingStatus == "Online" {
		logger.Info("SSM agent is online", "instance", instanceID)
		return nil
	}

	logger.Info("Waiting for SSM agent to become online...", "instance", instanceID)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("timeout waiting for SSM agent after %v", timeout)
		case <-ticker.C:
			output, err := c.ssmClient.DescribeInstanceInformation(ctx, &ssmbackend.DescribeInstanceInformationInput{
				InstanceID: instanceID,
			})
			if err != nil {
				logger.Warn("DescribeInstanceInformation failed", "error", err)
				continue
			}
			if output.PingStatus == "Online" {
				logger.Info("SSM agent is now online", "instance", instanceID)
				return nil
			}
			pingStatus := output.PingStatus
			if pingStatus == "" {
				pingStatus = "not-registered"
			}
			logger.Info("Waiting for SSM agent", "instance", instanceID, "pingStatus", pingStatus)
		}
	}
}

// SSMBackendConfig holds configuration for creating a new SSM backend.
type SSMBackendConfig struct {
	InstanceID   string
	Region       string
	SSHUser      string
	SSHKeyPath   string
	AutoStartEC2 bool
}

// NewSSMBackend creates a new SSM backend with the client's dial function injected.
func (c *Client) NewSSMBackend(cfg *SSMBackendConfig) *ssmbackend.Backend {
	backendConfig := &ssmbackend.Config{
		InstanceID:  cfg.InstanceID,
		Region:      cfg.Region,
		SSHUser:     cfg.SSHUser,
		SSHKeyPath:  cfg.SSHKeyPath,
	}

	if cfg.AutoStartEC2 {
		backendConfig.AutoStartEC2 = true
		backendConfig.EC2Client = c.ec2Client
	}

	// Inject custom dial function for WebSocket connections
	if c.dialFn != nil {
		backendConfig.DialContextFn = datachannel.DialContextFunc(c.dialFn)
	}

	return ssmbackend.New(backendConfig, c.ssmClient)
}
