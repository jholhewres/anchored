package ctx

import (
	"context"
	"fmt"
	"time"
)

type Sandbox struct{}

func NewSandbox(timeout time.Duration, maxOutputBytes int64, workDir string) *Sandbox {
	return &Sandbox{}
}

func (s *Sandbox) Execute(ctx context.Context, language string, code string) (*ExecuteResult, error) {
	return nil, fmt.Errorf("sandbox not supported on Windows")
}
