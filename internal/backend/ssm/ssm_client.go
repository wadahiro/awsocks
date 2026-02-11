package ssm

import (
	"context"
)

// SSMClient is an interface for SSM operations, allowing for mocking in tests
type SSMClient interface {
	StartSession(ctx context.Context, input *StartSessionInput) (*StartSessionOutput, error)
	TerminateSession(ctx context.Context, input *TerminateSessionInput) (*TerminateSessionOutput, error)
}
