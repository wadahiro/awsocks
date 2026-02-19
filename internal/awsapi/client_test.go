package awsapi

import (
	"context"
	"net"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testAWSConfig() aws.Config {
	return aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("key", "secret", "token"),
	}
}

func TestNewClient_DirectMode(t *testing.T) {
	client := NewClient(testAWSConfig(), nil)
	require.NotNil(t, client)
	assert.NotNil(t, client.EC2Client())
	assert.Nil(t, client.dialFn)
}

func TestNewClient_WithCustomDialer(t *testing.T) {
	dialCalled := false
	customDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialCalled = true
		return nil, nil
	}

	client := NewClient(testAWSConfig(), customDial)
	require.NotNil(t, client)
	assert.NotNil(t, client.dialFn)
	assert.NotNil(t, client.EC2Client())

	// Verify the custom dialer is stored
	_, _ = client.dialFn(context.Background(), "tcp", "example.com:443")
	assert.True(t, dialCalled, "custom dial function should be invoked")
}

func TestNewSSMBackend_InjectsDialFn(t *testing.T) {
	customDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, nil
	}

	client := NewClient(testAWSConfig(), customDial)

	backend := client.NewSSMBackend(&SSMBackendConfig{
		InstanceID: "i-12345",
		Region:     "us-east-1",
		SSHUser:    "ec2-user",
	})
	require.NotNil(t, backend)
}

func TestNewSSMBackend_NilDialFn(t *testing.T) {
	client := NewClient(testAWSConfig(), nil)

	backend := client.NewSSMBackend(&SSMBackendConfig{
		InstanceID: "i-12345",
		Region:     "us-east-1",
		SSHUser:    "ec2-user",
	})
	require.NotNil(t, backend)
}
